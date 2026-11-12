package auth

import (
	"net/url"
	"strings"
)

// SafeNext validates the "next" parameter of the login page and returns
// the URL to send the browser to after login.
//
// The danger it prevents is an "open redirect": a link such as
//
//	http://exp.lab/login?next=https://evil.example/fake-login
//
// looks trustworthy (it starts with our own domain), but after a real
// login it would forward the user to an attacker's page. So "next" is only
// honoured when it points at the domain itself or one of its subdomains.
// Anything else falls back to the dashboard.
//
// scheme is "http" or "https"; domain is the bare domain, e.g. "exp.lab".
func SafeNext(next, scheme, domain string) string {
	fallback := scheme + "://" + domain + "/"
	if next == "" || len(next) > 2048 {
		return fallback
	}
	// Browsers treat "\" like "/" and strip tabs/newlines, which are the
	// classic tricks for smuggling a different host past a parser.
	if strings.ContainsAny(next, "\\\t\r\n") {
		return fallback
	}
	for _, r := range next {
		if r < 0x20 || r == 0x7f {
			return fallback
		}
	}

	// A plain path ("/settings") stays on the dashboard host. "//host" is
	// NOT a path: browsers read it as "same scheme, different host".
	if strings.HasPrefix(next, "/") {
		if strings.HasPrefix(next, "//") {
			return fallback
		}
		return scheme + "://" + domain + next
	}

	u, err := url.Parse(next)
	if err != nil {
		return fallback
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fallback // javascript:, data:, etc.
	}
	if u.User != nil {
		return fallback // http://exp.lab@evil.example/
	}
	if u.Port() != "" {
		return fallback // apps are only published on the default port
	}
	host := strings.ToLower(u.Hostname())
	// The "." in the suffix check matters: without it "evilexp.lab" would
	// pass as if it were a subdomain.
	if host != domain && !strings.HasSuffix(host, "."+domain) {
		return fallback
	}
	return u.String()
}
