package breeze

// Asset generation for the client-side bundles.
//
// Run with:
//
//	go generate ./...
//
// Requires Node (for `npx esbuild`), exactly like the existing SPA runtime
// harnesses in template_spa_test.go require Node — and, like them, only to
// *regenerate* or *verify*, never to build or run Breeze. The generated
// .min.js/.min.css files are committed, so `go build` and `go test` work on a
// machine with no Node and no npm install.
//
// esbuild is pinned to an exact version so a regeneration months from now
// produces the same bytes rather than silently picking up a new minifier's
// output (and any new minifier bug) mid-release.
//
// Why esbuild rather than a Go minifier: correct JS minification is not
// regex-safe — regex literals, template strings, division vs. comment
// ambiguity and ASI all break naive approaches — and this runtime resolves
// developer functions by name off `window` (data-spa-mount="fnName"), so a
// minifier that renames the wrong identifier produces a page that loads and
// then quietly does nothing. esbuild renames only locals inside the IIFE and
// never object properties or `window` lookups unless --mangle-props is passed,
// which it deliberately is not.
//
// Verified by TestSPAMinifiedMatchesSource (drift) and by both SPA harnesses
// running against the minified bundle (behaviour).

//go:generate npx --yes esbuild@0.28.2 spa.js --minify --target=es2017 --legal-comments=none --outfile=spa.min.js
//go:generate npx --yes esbuild@0.28.2 dashboard/templates/public/dashboard.js --minify --target=es2017 --legal-comments=none --outfile=dashboard/templates/public/dashboard.min.js
//go:generate npx --yes esbuild@0.28.2 dashboard/templates/public/dashboard.css --minify --outfile=dashboard/templates/public/dashboard.min.css
