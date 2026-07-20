package oauth

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) renderLogin(w http.ResponseWriter, q url.Values, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageShell("Sign in to Takan", `
  <h1>Sign in to connect Takan</h1>
  <p class="muted">Authorize your AI client (Grok, Claude, …) to use your Takan modules.</p>
  `+errBlock(errMsg)+`
  <form method="post" action="/oauth/authorize">
    `+hiddenOAuthFields(q)+`
    <input type="hidden" name="action" value="login"/>
    <label>Email</label>
    <input type="email" name="email" required autocomplete="username"/>
    <label>Password</label>
    <input type="password" name="password" required autocomplete="current-password"/>
    <button type="submit">Sign in &amp; authorize</button>
  </form>
  <p class="muted">Accounts are invitation-only for now.</p>
`))
}

func (s *Server) renderConsent(w http.ResponseWriter, q url.Values, email, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageShell("Authorize Takan", `
  <h1>Authorize Takan</h1>
  <p class="muted">Signed in as <strong>`+htmlEscape(email)+`</strong></p>
  `+errBlock(errMsg)+`
  <p>Allow this application to call your Takan MCP tools (modules you enable in the panel).</p>
  <form method="post" action="/oauth/authorize" style="display:inline">
    `+hiddenOAuthFields(q)+`
    <input type="hidden" name="action" value="allow"/>
    <button type="submit">Allow</button>
  </form>
  <form method="post" action="/oauth/authorize" style="display:inline;margin-left:.5rem">
    `+hiddenOAuthFields(q)+`
    <input type="hidden" name="action" value="deny"/>
    <button type="submit" class="secondary">Deny</button>
  </form>
`))
}

func hiddenOAuthFields(q url.Values) string {
	var b strings.Builder
	for _, k := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"} {
		if v := q.Get(k); v != "" {
			fmt.Fprintf(&b, `<input type="hidden" name="%s" value="%s"/>`, k, htmlEscape(v))
		}
	}
	return b.String()
}

func errBlock(msg string) string {
	if msg == "" {
		return ""
	}
	return `<p class="err">` + htmlEscape(msg) + `</p>`
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func pageShell(title, body string) string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>` + htmlEscape(title) + ` · Takan</title>
<style>
:root{--bg:#0b0d10;--card:#14181f;--text:#e8eaed;--muted:#9aa0a6;--acc:#7c9cff;--bad:#f07178;--border:#252b36}
body{margin:0;font-family:system-ui,sans-serif;background:var(--bg);color:var(--text)}
.wrap{max-width:420px;margin:3rem auto;padding:0 1rem}
.card{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:1.25rem}
h1{font-size:1.25rem;margin:0 0 .5rem}
.muted{color:var(--muted);font-size:.9rem}
.err{color:var(--bad);font-size:.9rem}
label{display:block;margin:.6rem 0 .25rem;font-size:.85rem;color:var(--muted)}
input{width:100%;padding:.55rem .7rem;border-radius:8px;border:1px solid var(--border);background:#0e1218;color:var(--text);box-sizing:border-box}
button{margin-top:.75rem;padding:.55rem 1rem;border:0;border-radius:8px;background:var(--acc);color:#0b0d10;font-weight:600;cursor:pointer}
button.secondary{background:transparent;color:var(--text);border:1px solid var(--border)}
a{color:var(--acc)}
</style></head><body><div class="wrap"><div class="card">` + body + `</div></div></body></html>`
}
