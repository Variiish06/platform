package server

import (
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"portal/internal/auth"
	"portal/internal/store"
)

// handleVerify is the forward-auth endpoint.
//
// For every request to <app>.exp.lab, Caddy first sends a copy of the
// request's headers here and waits for the verdict:
//
//	2xx        -> allowed. Caddy proxies the original request to the
//	              address in the X-Portal-Upstream response header.
//	anything   -> denied. Caddy sends THIS response to the browser, so a
//	else          302 here becomes the redirect to the login page.
//
// The app never sees unauthenticated traffic and needs no auth code.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	// Caddy describes the original request in X-Forwarded-* headers.
	host := strings.ToLower(r.Header.Get("X-Forwarded-Host"))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	app, ok := s.apps.ByHost(host)
	if !ok {
		s.metrics.Verify("unknown_host")
		http.Error(w, "No app is configured for "+host, http.StatusNotFound)
		return
	}

	if app.RequiresAuth() {
		if _, ok := s.sessionToken(r); !ok {
			s.metrics.Verify("denied")
			// A person opening a page gets sent to the login form and
			// comes back afterwards. A script's API call (fetch, curl)
			// cannot use a login page, so it gets a plain 401.
			method := r.Header.Get("X-Forwarded-Method")
			if (method == "" || method == http.MethodGet) && strings.Contains(r.Header.Get("Accept"), "text/html") {
				original := s.cfg.Scheme() + "://" + host + r.Header.Get("X-Forwarded-Uri")
				http.Redirect(w, r, s.cfg.BaseURL()+"/login?next="+url.QueryEscape(original), http.StatusFound)
				return
			}
			http.Error(w, "Sign in at "+s.cfg.BaseURL()+"/login", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Portal-User", s.cfg.User)
	}
	s.metrics.Verify("allowed")
	w.Header().Set("X-Portal-Upstream", app.Upstream)
	w.WriteHeader(http.StatusOK)
}

// loginPage is the data for login.html.
type loginPage struct {
	V        string
	User     string
	Next     string // validated return URL
	NextHost string // shown as "to continue to ...", empty for the dashboard
	Error    string
	TOTP     bool
	NoPass   bool // no password has been set yet
	Setup    string
}

func (s *Server) loginData(next string) loginPage {
	p := loginPage{V: s.version, User: s.cfg.User, Next: auth.SafeNext(next, s.cfg.Scheme(), s.cfg.Domain)}
	if u, err := url.Parse(p.Next); err == nil && u.Hostname() != s.cfg.Domain {
		p.NextHost = u.Hostname()
	}
	hash, _ := store.GetSetting(s.db, store.KeyPasswordHash)
	p.NoPass = hash == ""
	p.Setup = "cd ~/platform && ./portalctl set-password"
	secret, _ := store.GetSetting(s.db, store.KeyTOTPSecret)
	p.TOTP = secret != ""
	return p
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	p := s.loginData(r.URL.Query().Get("next"))
	if _, ok := s.sessionToken(r); ok {
		http.Redirect(w, r, p.Next, http.StatusSeeOther) // already signed in
		return
	}
	s.render(w, http.StatusOK, "login.html", p)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	// Cap the body: a login form is tiny, and argon2 on a megabyte-long
	// "password" would be a cheap way to burn CPU.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p := s.loginData(r.PostForm.Get("next"))
	ip := clientIP(r)

	// Rate limit first: a locked-out client must not get to test a guess.
	if ok, wait := s.limiter.Allow(ip); !ok {
		s.metrics.Login("rate_limited")
		secs := int(math.Ceil(wait.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		p.Error = "Too many attempts. Try again in " + humanWait(wait) + "."
		s.render(w, http.StatusTooManyRequests, "login.html", p)
		return
	}

	hash, err := store.GetSetting(s.db, store.KeyPasswordHash)
	if err != nil || hash == "" {
		s.render(w, http.StatusServiceUnavailable, "login.html", p)
		return
	}

	password := r.PostForm.Get("password")
	ok := false
	if password != "" && len(password) <= 1024 {
		ok, err = auth.VerifyPassword(password, hash)
		// Argon2 just used ~19 MiB. Go would hand it back to the OS
		// eventually; do it now so the container's RAM stays low.
		go debug.FreeOSMemory()
		if err != nil {
			log.Printf("login: stored password hash is unreadable: %v", err)
		}
	}
	// Second factor, only when enabled. Checked after the password so a
	// wrong password never reveals whether the code was right.
	if ok {
		if secret, _ := store.GetSetting(s.db, store.KeyTOTPSecret); secret != "" {
			last, _ := store.GetSetting(s.db, store.KeyTOTPLastStep)
			lastStep, _ := strconv.ParseInt(last, 10, 64)
			step, good := auth.VerifyTOTP(secret, r.PostForm.Get("code"), time.Now(), lastStep)
			if good {
				store.SetSetting(s.db, store.KeyTOTPLastStep, strconv.FormatInt(step, 10))
			}
			ok = good
		}
	}

	if !ok {
		s.metrics.Login("failed")
		lock := s.limiter.Fail(ip)
		log.Printf("login failed from %s (locked for %s)", ip, lock)
		// One message for every kind of failure: saying which part was
		// wrong would help an attacker.
		p.Error = "That didn't match. Check your password and try again."
		if p.TOTP {
			p.Error = "That didn't match. Check your password and code and try again."
		}
		if lock > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(lock.Seconds()))))
			p.Error += " Next attempt in " + humanWait(lock) + "."
		}
		s.render(w, http.StatusUnauthorized, "login.html", p)
		return
	}

	s.limiter.Success(ip)
	token, err := s.sessions.Create(ip, r.UserAgent())
	if err != nil {
		log.Printf("login: create session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.metrics.Login("ok")
	log.Printf("login ok from %s", ip)
	s.setSessionCookie(w, token)
	// 303 "See Other": the browser follows with a GET, so reloading the
	// destination never re-submits the password form.
	http.Redirect(w, r, p.Next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		// Delete the server-side row: the cookie value is now worthless
		// even if a copy of it survives somewhere.
		s.sessions.Delete(c.Value)
	}
	s.setSessionCookie(w, "")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleSessionProbe lets the dashboard's JavaScript ask "am I still
// signed in?" when the event stream drops.
func (s *Server) handleSessionProbe(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionToken(r); !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// humanWait formats a lockout for people: "8 seconds", "4 minutes".
func humanWait(d time.Duration) string {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 60 {
		if secs == 1 {
			return "1 second"
		}
		return fmt.Sprintf("%d seconds", secs)
	}
	mins := int(math.Ceil(d.Minutes()))
	if mins == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", mins)
}
