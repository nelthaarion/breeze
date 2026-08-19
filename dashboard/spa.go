package dashboard

import "strings"

// SPA returns the dashboard's single-file HTML shell. All CSS and JS are
// inlined so the dashboard ships with zero external dependencies and zero
// additional HTTP requests on first load.
//
// The shell contains:
//   - A sidebar with the 13 dashboard pages grouped into sections.
//   - A topbar with the page title and the live WebSocket status.
//   - A content pane that is swapped by the SPA runtime as the user
//     navigates between pages.
//
// Pages are rendered on demand by the JavaScript in spaJS.go.
func SPA() string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta name="color-scheme" content="dark">
<title>Breeze Dashboard</title>
<style>`)
	b.WriteString(spaCSS)
	b.WriteString(`</style>
</head>
<body>
<div class="app">
  <aside class="sidebar">
    <div class="sidebar-header">
      <div class="logo"><img src="/public/logo.png"/> </div>
      <div>
        <h1>Breeze</h1>
        <div class="sub">Developer Dashboard</div>
      </div>
    </div>
    <nav class="nav"></nav>
    <div class="sidebar-footer">
      <span><span class="ws-dot off"></span><span class="ws-status">connecting...</span></span>
      <span>v1.0</span>
    </div>
  </aside>
  <main class="main">
    <header class="topbar">
      <h2>Overview</h2>
      <div class="right">
        <span id="server-time"></span>
      </div>
    </header>
    <div class="content"></div>
  </main>
</div>
<script>`)
	b.WriteString(spaJS)
	b.WriteString(`</script>
</body>
</html>`)
	return b.String()
}
