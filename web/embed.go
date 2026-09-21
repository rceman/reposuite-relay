// Package webassets embeds the deterministic production build of the
// SvelteKit Web Admin (web/build/). The build output is committed to the
// repository, so a clean checkout compiles relayd without Node, npm, or
// Vite. scripts/check-web.sh is the drift gate proving web/build matches
// the committed TypeScript source and lockfile.
package webassets

import "embed"

// Build is the complete adapter-static output tree rooted at "build"
// (index.html plus hashed _app/immutable/* assets).
//
//go:embed all:build
var Build embed.FS
