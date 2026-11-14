// Command portal is the home server's front door: login, forward-auth
// for the reverse proxy, and the dashboard. One binary, several
// subcommands:
//
//	portal serve               run the web server (the container's default)
//	portal set-password        set or change the login password
//	portal totp enable         turn on 2FA and print the secret for your app
//	portal totp disable        turn 2FA off
//	portal totp status
//	portal sessions            list active sessions
//	portal sessions revoke     sign out everywhere
//	portal sessions create [--ttl 15m] [--note text]
//	                           mint a short-lived session, print its cookie
//	portal sessions revoke <cookie value>
//	                           end just that one session
//	portal healthcheck         exit 0 if the server answers (for Docker)
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed the timezone database: the image has no /usr/share/zoneinfo

	"golang.org/x/term"

	"portal/internal/auth"
	"portal/internal/server"
	"portal/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func config() server.Config {
	return server.Config{
		Listen:       env("PORTAL_LISTEN", "127.0.0.1:7080"),
		Domain:       env("PORTAL_DOMAIN", "exp.lab"),
		DataDir:      env("PORTAL_DATA", "/data"),
		DockerSock:   env("PORTAL_DOCKER_SOCK", "/run/docker-proxy/docker.sock"),
		User:         env("PORTAL_USER", "admin"),
		CookieSecure: env("PORTAL_COOKIE_SECURE", "0") == "1",
		NtfyURL:      os.Getenv("PORTAL_NTFY_URL"),
		NtfyToken:    os.Getenv("PORTAL_NTFY_TOKEN"),
		VMURL:        os.Getenv("PORTAL_VM_URL"),
		BackupDir:    os.Getenv("PORTAL_BACKUP_DIR"), // default <data>/backup
		// Memory panel: the host's /proc (read-only). Unset = panel off.
		HostProc:       os.Getenv("PORTAL_HOST_PROC"),
		ClaudeSessions: os.Getenv("PORTAL_CLAUDE_SESSIONS"),
		// Silences for containers stopped from the dashboard. Set it to
		// "off" to disable.
		AlertmanagerURL: alertmanagerURL(),
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	// Files this process creates (the database) are readable by the
	// owner only: it holds the password hash.
	syscall.Umask(0o077)
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg := config()

	if cmd == "healthcheck" {
		healthcheck(cfg)
		return
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		return
	}

	db, err := store.Open(filepath.Join(cfg.DataDir, "portal.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	switch cmd {
	case "serve":
		serve(cfg, db)
	case "set-password":
		setPassword(db)
	case "totp":
		totp(cfg, db, arg(2))
	case "sessions":
		sessions(db, os.Args[min(2, len(os.Args)):])
	default:
		usage()
		os.Exit(2)
	}
}

func arg(i int) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	return ""
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: portal <command>

  serve               run the web server
  set-password        set or change the login password
  totp enable         turn on two-factor login
  totp disable        turn it off
  totp status
  sessions            list active sessions
  sessions revoke     sign out everywhere
  sessions create [--ttl 15m] [--note text]
                      start a short-lived session (for testing with curl)
                      and print the cookie value
  sessions revoke <cookie value>
                      end just that one session
  healthcheck         exit 0 if the server is healthy`)
}

func serve(cfg server.Config, db *sql.DB) {
	srv, err := server.New(cfg, db)
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	if hash, _ := store.GetSetting(db, store.KeyPasswordHash); hash == "" {
		log.Printf("no password set yet: run 'portal set-password'")
	}
	// SIGTERM is what "docker stop" sends. Cancelling the context lets
	// open requests finish before the process exits.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	log.Printf("portal stopped")
}

// readSecret reads a line without echoing it when stdin is a terminal.
// When input is piped (scripts, tests) it reads a plain line instead.
func readSecret(prompt string, in *bufio.Reader) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func setPassword(db *sql.DB) {
	in := bufio.NewReader(os.Stdin)
	pw, err := readSecret("New password: ", in)
	if err != nil {
		log.Fatalf("read password: %v", err)
	}
	if len(pw) < 10 {
		fmt.Fprintln(os.Stderr, "Password must be at least 10 characters. Nothing was changed.")
		os.Exit(1)
	}
	if len(pw) > 1024 {
		fmt.Fprintln(os.Stderr, "Password must be at most 1024 characters. Nothing was changed.")
		os.Exit(1)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		again, _ := readSecret("Repeat password: ", in)
		if again != pw {
			fmt.Fprintln(os.Stderr, "The two entries differ. Nothing was changed.")
			os.Exit(1)
		}
	}
	// Only the argon2id hash is stored. The password itself exists in
	// memory for the duration of this command and nowhere else.
	hash, err := auth.HashPassword(pw, auth.DefaultParams)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}
	if err := store.SetSetting(db, store.KeyPasswordHash, hash); err != nil {
		log.Fatalf("save password: %v", err)
	}
	// Whoever knew the old password may still hold a session: end them all.
	n, _ := auth.NewSessionStore(db).DeleteAll()
	fmt.Printf("Password set. %d existing session(s) signed out.\n", n)
}

func totp(cfg server.Config, db *sql.DB, sub string) {
	switch sub {
	case "status":
		if s, _ := store.GetSetting(db, store.KeyTOTPSecret); s != "" {
			fmt.Println("Two-factor login is ON.")
		} else {
			fmt.Println("Two-factor login is off.")
		}
	case "disable":
		store.DeleteSetting(db, store.KeyTOTPSecret)
		store.DeleteSetting(db, store.KeyTOTPLastStep)
		fmt.Println("Two-factor login is off.")
	case "enable":
		secret, err := auth.NewTOTPSecret()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Add this account to your authenticator app (choose \"enter a setup key\"):")
		fmt.Println()
		fmt.Println("  Account:  " + cfg.User + "@" + cfg.Domain)
		fmt.Println("  Key:      " + secret)
		fmt.Println("  Type:     time-based, 6 digits, 30 seconds")
		fmt.Println()
		fmt.Println("  Or open this link on the phone: " + auth.TOTPURI(secret, "Portal", cfg.User+"@"+cfg.Domain))
		fmt.Println()
		// Nothing is saved until a code from the app verifies. Otherwise
		// a typo while adding the key would lock you out.
		fmt.Fprint(os.Stderr, "Enter the 6-digit code the app shows now: ")
		code, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		step, ok := auth.VerifyTOTP(secret, code, time.Now(), 0)
		if !ok {
			fmt.Fprintln(os.Stderr, "That code is not right. Two-factor login was NOT turned on.")
			os.Exit(1)
		}
		store.SetSetting(db, store.KeyTOTPSecret, secret)
		store.SetSetting(db, store.KeyTOTPLastStep, strconv.FormatInt(step, 10))
		fmt.Println("Two-factor login is ON. The login page now asks for a code.")
	default:
		usage()
		os.Exit(2)
	}
}

func sessions(db *sql.DB, args []string) {
	st := auth.NewSessionStore(db)
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "revoke" && len(args) > 1:
		if err := st.Delete(args[1]); err != nil {
			log.Fatal(err)
		}
		fmt.Println("That session is signed out (if it existed).")
		return
	case sub == "revoke":
		n, err := st.DeleteAll()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d session(s) signed out.\n", n)
		return
	case sub == "create":
		token, expires, err := createSession(st, args[1:], os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "Session valid until %s. Use it as:  Cookie: portal_session=<value>\n"+
			"End it early with:  portal sessions revoke <value>\n", expires.Local().Format("2 Jan 15:04:05"))
		fmt.Println(token) // only the token on stdout, so scripts can capture it
		return
	case sub != "":
		usage()
		os.Exit(2)
	}
	list, err := st.List()
	if err != nil {
		log.Fatal(err)
	}
	if len(list) == 0 {
		fmt.Println("No active sessions.")
		return
	}
	for _, s := range list {
		ua := s.UserAgent
		if len(ua) > 60 {
			ua = ua[:60] + "…"
		}
		fmt.Printf("%-15s  last seen %s  expires %s  %s\n", s.IP,
			s.LastSeen.Local().Format("2 Jan 15:04"), s.ExpiresAt.Local().Format("2 Jan"), ua)
	}
}

// createSession implements "portal sessions create". It needs shell access
// to the container (docker compose exec), which already implies full
// control of Portal, so it grants nothing new. The session is fixed-length
// (it does not slide with use) and is listed by "portal sessions" with the
// IP "cli" and the note as its user agent.
func createSession(st *auth.SessionStore, args []string, errOut io.Writer) (string, time.Time, error) {
	fs := flag.NewFlagSet("sessions create", flag.ContinueOnError)
	fs.SetOutput(errOut)
	ttl := fs.Duration("ttl", 15*time.Minute, "lifetime, at most 24h")
	note := fs.String("note", "", "label shown by 'portal sessions'")
	if err := fs.Parse(args); err != nil {
		return "", time.Time{}, err
	}
	if fs.NArg() > 0 {
		return "", time.Time{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// A test session has no business living longer than a day.
	if *ttl <= 0 || *ttl > 24*time.Hour {
		return "", time.Time{}, fmt.Errorf("--ttl must be between 1s and 24h")
	}
	token, err := st.CreateWithTTL("cli", "portal sessions create: "+*note, *ttl)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, time.Now().Add(*ttl), nil
}

// healthcheck is run by Docker inside the container. The image has no
// shell or curl, so the binary checks itself.
func healthcheck(cfg server.Config) {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + cfg.Listen + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, resp.Status)
		os.Exit(1)
	}
}

// alertmanagerURL: PORTAL_ALERTMANAGER_URL, default Alertmanager on the
// host's loopback (Portal runs with host networking); "off" disables.
func alertmanagerURL() string {
	v := env("PORTAL_ALERTMANAGER_URL", "http://127.0.0.1:9093")
	if v == "off" {
		return ""
	}
	return v
}
