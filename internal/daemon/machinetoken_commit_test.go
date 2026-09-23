// Machine-token commit-point classification over HTTP: post-commit
// dirsync failure still commits the credential (durability unconfirmed);
// pre-commit faults leave the old token canonical; concurrent rotations
// serialize; the credential leaks nowhere else in the state root.
package daemon

import (
	"encoding/json"
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/auth"
)

// TestMachineTokenPostCommit: dirsync failure after rename → the token
// IS committed; response returns it with durabilityConfirmed=false;
// live + disk converge on the new credential.
func TestMachineTokenPostCommit(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
		MachineTokenHooks: &auth.Hooks{
			SyncDir: func(dir string) error { return errors.New("injected dirsync") },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	old, _ := auth.Load(d.paths.MachineToken())

	resp := cookieJSON(t, c, http.MethodPost, endpoint,
		"/v1/settings/machine-token/rotate", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-commit rotation: %d", resp.StatusCode)
	}
	out := decodeRotate(t, resp)
	if out.DurabilityConfirmed {
		t.Fatal("dirsync failure must report durabilityConfirmed=false")
	}
	if !auth.Valid(out.Token) || out.Token == old {
		t.Fatal("post-commit rotation must return the committed token")
	}
	if s := bearerStatus(t, endpoint, out.Token); s != http.StatusOK {
		t.Fatalf("committed token: %d", s)
	}
	if s := bearerStatus(t, endpoint, old); s != http.StatusUnauthorized {
		t.Fatalf("old token: %d", s)
	}
	got, _ := auth.Load(d.paths.MachineToken())
	if got != out.Token {
		t.Fatal("disk token != committed token")
	}
}

// TestMachineTokenPreCommit: a rename fault leaves the old credential
// canonical everywhere — error response carries no token.
func TestMachineTokenPreCommit(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
		MachineTokenHooks: &auth.Hooks{
			Rename: func(o, n string) error { return errors.New("injected rename") },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	old, _ := auth.Load(d.paths.MachineToken())

	resp := cookieJSON(t, c, http.MethodPost, endpoint,
		"/v1/settings/machine-token/rotate", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("pre-commit rotation: %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if _, leaked := body["token"]; leaked {
		t.Fatal("pre-commit failure must not expose a token")
	}
	if s := bearerStatus(t, endpoint, old); s != http.StatusOK {
		t.Fatalf("old token after failed rotation: %d", s)
	}
	got, _ := auth.Load(d.paths.MachineToken())
	if got != old {
		t.Fatal("canonical file changed on pre-commit failure")
	}
}

// TestMachineTokenRestart: the rotated credential persists across a
// daemon generation; the old one stays dead; no re-mint.
func TestMachineTokenRestart(t *testing.T) {
	d, p, c, done := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	old, _ := auth.Load(d.paths.MachineToken())
	resp := cookieJSON(t, c, http.MethodPost, endpoint,
		"/v1/settings/machine-token/rotate", csrf, "")
	out := decodeRotate(t, resp)
	stopDaemonNow(t, d, done)

	d2, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d2)
	if s := bearerStatus(t, d2.endpoint(), out.Token); s != http.StatusOK {
		t.Fatalf("rotated token after restart: %d", s)
	}
	if s := bearerStatus(t, d2.endpoint(), old); s != http.StatusUnauthorized {
		t.Fatalf("old token after restart: %d", s)
	}
	got, _ := auth.Load(p.MachineToken())
	if got != out.Token {
		t.Fatal("restart re-minted or diverged the machine token")
	}
}

// TestMachineTokenConcurrentRotation: rotations serialize under the
// write lock — every admitted request succeeds, every returned token is
// valid and unique, and exactly the last committed credential
// authenticates. Workers report results on a channel; all assertions
// run on the main test goroutine.
func TestMachineTokenConcurrentRotation(t *testing.T) {
	d, _, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	const N = 8
	type result struct {
		code  int
		token string
	}
	resCh := make(chan result, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := cookieJSONDo(c, http.MethodPost, endpoint,
				"/v1/settings/machine-token/rotate", csrf, "")
			if err != nil {
				resCh <- result{code: -1}
				return
			}
			defer resp.Body.Close()
			var out api.MachineTokenRotateResponse
			if resp.StatusCode != http.StatusOK ||
				json.NewDecoder(resp.Body).Decode(&out) != nil ||
				!out.DurabilityConfirmed {
				resCh <- result{code: resp.StatusCode}
				return
			}
			resCh <- result{
				code:  http.StatusOK,
				token: out.Token,
			}
		}()
	}
	wg.Wait()
	close(resCh)

	seen := map[string]bool{}
	var ok, failed int
	for r := range resCh {
		if r.code != http.StatusOK {
			failed++
			continue
		}
		if !auth.Valid(r.token) {
			t.Fatal("concurrent rotation returned a malformed token")
		}
		if seen[r.token] {
			t.Fatal("duplicate token generated")
		}
		seen[r.token] = true
		ok++
	}
	// Serialization has no contention failure: all admitted rotations
	// must succeed.
	if ok != N || failed != 0 {
		t.Fatalf("rotations: %d ok, %d failed, want %d ok", ok, failed, N)
	}
	// Exactly the final committed token authenticates.
	final, err := auth.Load(d.paths.MachineToken())
	if err != nil {
		t.Fatal(err)
	}
	if !seen[final] {
		t.Fatal("final disk token was never returned to a caller")
	}
	for tok := range seen {
		want := http.StatusUnauthorized
		if tok == final {
			want = http.StatusOK
		}
		if s := bearerStatus(t, endpoint, tok); s != want {
			t.Fatalf("token admission = %d, want %d", s, want)
		}
	}
}

// TestMachineAuthVsRotation: machine-bearer reads race with admin
// rotations under -race. Each read splits at the linearization point —
// old token may be accepted pre-switch — but once a rotation has
// returned, every later OLD-token request must reject.
func TestMachineAuthVsRotation(t *testing.T) {
	d, _, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	old, err := auth.Load(d.paths.MachineToken())
	if err != nil {
		t.Fatal(err)
	}

	const M = 32
	type result struct {
		code  int
		token string
	}
	readsDone := make(chan int, M)
	var wg sync.WaitGroup
	for i := 0; i < M; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, err := bearerGetStatus(endpoint, old)
			if err != nil {
				readsDone <- -1
				return
			}
			readsDone <- code
		}()
	}
	// Rotate while the reads are in flight.
	rotCh := make(chan result, 1)
	go func() {
		resp, err := cookieJSONDo(c, http.MethodPost, endpoint,
			"/v1/settings/machine-token/rotate", csrf, "")
		if err != nil {
			rotCh <- result{code: -1}
			return
		}
		defer resp.Body.Close()
		var out api.MachineTokenRotateResponse
		if resp.StatusCode != http.StatusOK ||
			json.NewDecoder(resp.Body).Decode(&out) != nil {
			rotCh <- result{code: resp.StatusCode}
			return
		}
		rotCh <- result{
			code:  http.StatusOK,
			token: out.Token,
		}
	}()
	wg.Wait()
	rot := <-rotCh
	if rot.code != http.StatusOK || !auth.Valid(rot.token) {
		t.Fatalf("rotation: %d", rot.code)
	}
	// In-flight reads may have split at the linearization point — any
	// 200/401 mix is legal; a transport error (-1) is not.
	for i := 0; i < M; i++ {
		if code := <-readsDone; code == -1 {
			t.Fatal("reader transport failure")
		}
	}
	// Post-rotation admission is exact.
	if s := bearerStatus(t, endpoint, old); s != http.StatusUnauthorized {
		t.Fatalf("old token after committed rotation: %d", s)
	}
	if s := bearerStatus(t, endpoint, rot.token); s != http.StatusOK {
		t.Fatalf("new token after committed rotation: %d", s)
	}
}

// TestMachineTokenNonDisclosure: after rotation the new credential
// exists only in config/api.token — nowhere else in the state root.
func TestMachineTokenNonDisclosure(t *testing.T) {
	d, p, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	createCold(t, c, endpoint, csrf, "disclosure", "/tmp")
	resp := cookieJSON(t, c, http.MethodPost, endpoint,
		"/v1/settings/machine-token/rotate", csrf, "")
	out := decodeRotate(t, resp)

	leaks := []string{}
	err := filepath.Walk(p.RelayRoot(), func(path string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), out.Token) &&
			filepath.Base(path) != "api.token" {
			leaks = append(leaks, filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leaks) != 0 {
		t.Fatalf("machine token leaked into %v", leaks)
	}
}
