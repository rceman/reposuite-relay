package daemon

import "html/template"

// webTemplate is the complete server-rendered Web Admin shell. No
// scripts, no external resources, no inline styles: the CSP is
// default-src 'none'. Three states share one document skeleton.
var webTemplate = template.Must(template.New("web").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>RepoSuite Relay</title>
</head>
<body>
<h1>RepoSuite Relay</h1>
{{if eq .Mode "setup"}}
<h2>Web Admin setup</h2>
<p>No administrator is configured. Create the single admin account.</p>
<form method="post" action="/auth/setup">
<input type="hidden" name="form_token" value="{{.FormToken}}">
<label>Username <input type="text" name="username" required autocomplete="username"></label>
<label>Password <input type="password" name="password" required autocomplete="new-password"></label>
<label>Confirm password <input type="password" name="confirm" required autocomplete="new-password"></label>
<button type="submit">Create admin</button>
</form>
{{else if eq .Mode "login"}}
<h2>Web Admin sign in</h2>
<form method="post" action="/auth/login">
<input type="hidden" name="form_token" value="{{.FormToken}}">
<label>Username <input type="text" name="username" required autocomplete="username"></label>
<label>Password <input type="password" name="password" required autocomplete="current-password"></label>
<button type="submit">Sign in</button>
</form>
{{else}}
<p>Signed in as {{.Username}}</p>
<p>Web Admin</p>
<form method="post" action="/auth/logout">
<input type="hidden" name="csrf" value="{{.CSRFToken}}">
<button type="submit">Sign out</button>
</form>
{{end}}
{{if .Error}}<p>{{.Error}}</p>{{end}}
</body>
</html>
`))
