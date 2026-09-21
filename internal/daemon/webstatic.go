// Embedded Web Admin static serving (ADR-006 §Web Admin): the compiled
// SvelteKit SPA ships inside the relayd binary via webassets.Build. This
// file owns request hygiene — path normalization, content types, cache
// policy, and the document Content-Security-Policy derived from the
// actual embedded index.html.
package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/rceman/reposuite-relay/internal/api"
	webassets "github.com/rceman/reposuite-relay/web"
)

// webStatic serves the embedded SPA. Everything it can reach comes from
// the compiled-in filesystem; there is no disk web root.
type webStatic struct {
	fsys  fs.FS
	index []byte // SPA entry document, also the route fallback
	csp   string // document policy incl. sha256 of each inline block
}

// inlineBlock matches one HTML element body for CSP hashing.
var inlineBlock = map[string]*regexp.Regexp{
	"script": regexp.MustCompile(`(?is)<script((\s[^>]*)?)>(.*?)</script>`),
	"style":  regexp.MustCompile(`(?is)<style((\s[^>]*)?)>(.*?)</style>`),
}

// newWebStatic opens the embedded build, extracts index.html, and derives
// the CSP: every inline <script>/<style> block (none carry src) gets a
// sha256 hash so no 'unsafe-inline' is ever required.
func newWebStatic() (*webStatic, error) {
	sub, err := fs.Sub(webassets.Build, "build")
	if err != nil {
		return nil, fmt.Errorf("web build fs: %w", err)
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, fmt.Errorf("web index: %w", err)
	}
	ws := &webStatic{
		fsys:  sub,
		index: index,
	}
	ws.csp = ws.documentCSP(index)
	return ws, nil
}

// documentCSP builds the Web Admin document policy: scripts and styles
// from self plus a sha256 hash per inline block present in the emitted
// document; connect/img/font only from self; no objects, framing, or
// base hijack. No unsafe-eval, no unsafe-inline.
func (ws *webStatic) documentCSP(doc []byte) string {
	var script, style []string
	for _, m := range inlineBlock["script"].FindAllSubmatch(doc, -1) {
		if !bytes.Contains(bytes.ToLower(m[1]), []byte("src=")) {
			script = append(script, cspHash(m[3]))
		}
	}
	for _, m := range inlineBlock["style"].FindAllSubmatch(doc, -1) {
		style = append(style, cspHash(m[3]))
	}
	parts := []string{
		"default-src 'none'",
		"script-src 'self'" + strings.Join(prefixEach(script), ""),
		"style-src 'self'" + strings.Join(prefixEach(style), ""),
		"connect-src 'self'",
		"img-src 'self'",
		"font-src 'self'",
		"form-action 'self'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"object-src 'none'",
	}
	return strings.Join(parts, "; ")
}

func prefixEach(hashes []string) []string {
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, " "+h)
	}
	return out
}

func cspHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// webContentType maps the committed asset extensions to explicit types —
// mime.TypeByExtension depends on the host's mime database, which is not
// part of the deterministic contract.
func webContentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".webmanifest":
		return "application/manifest+json"
	}
	return "application/octet-stream"
}

// serveStatic handles every request that is not /auth/* or /v1/*: real
// embedded assets, or the SPA index.html fallback for client-side routes.
// Only GET/HEAD are document/asset methods; anything else is 405.
func (d *Daemon) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, api.ErrInvalidRequest, "method not allowed")
		return
	}
	p := r.URL.Path
	// A traversal attempt is rejected outright rather than normalized
	// into a different document — the request itself is malformed.
	if strings.Contains(p, "..") || strings.ContainsRune(p, 0) ||
		strings.Contains(p, "\\") {
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	if rel == "" {
		d.serveIndex(w, r)
		return
	}
	if !fs.ValidPath(rel) {
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
		return
	}
	if f, err := d.static.fsys.Open(rel); err == nil {
		defer f.Close()
		fi, err := f.Stat()
		if err == nil && fi.IsDir() {
			err = fs.ErrNotExist
		}
		if err == nil {
			d.serveAsset(w, r, rel, f, fi)
			return
		}
	}
	// Missing file: everything under _app/ is a build asset and any path
	// with an extension names a file — never fall back to HTML for those.
	// Extensionless paths are client-side routes and get the SPA entry
	// document.
	if strings.HasPrefix(rel, "_app/") || strings.Contains(path.Base(rel), ".") {
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
		return
	}
	d.serveIndex(w, r)
}

// serveIndex writes the SPA entry document: no-store, text/html, and the
// hash-derived document CSP.
func (d *Daemon) serveIndex(w http.ResponseWriter, r *http.Request) {
	d.staticHeaders(w.Header())
	h := w.Header()
	h.Set("Content-Type", webContentType("index.html"))
	h.Set("Content-Security-Policy", d.static.csp)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(d.static.index)
	}
}

// serveAsset writes one embedded file. Content-hashed files under
// _app/immutable are immutable and cacheable long-term; every other file
// (robots.txt, favicon, _app/version.json) is revalidated every load.
func (d *Daemon) serveAsset(w http.ResponseWriter, r *http.Request, rel string, f fs.File, fi fs.FileInfo) {
	d.staticHeaders(w.Header())
	h := w.Header()
	h.Set("Content-Type", webContentType(rel))
	if strings.HasPrefix(rel, "_app/immutable/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-store")
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "unreadable asset")
		return
	}
	http.ServeContent(w, r, path.Base(rel), fi.ModTime(), rs)
}

// staticHeaders applies the baseline security policy to document/asset
// responses. The CSP for documents is added by serveIndex; JSON error
// bodies on this surface also inherit the restrictive default-src 'none'
// flavor via the CSP-less baseline (nosniff makes it safe regardless).
func (d *Daemon) staticHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}
