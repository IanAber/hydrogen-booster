package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// AdminUsername is the single shared login the operator/admin/diagnostics
// screens use. There's no per-user account system - just one gate
// separating staff from the public customer screen.
const AdminUsername = "egadmin"

// MinAdminPasswordLength is enforced only when an operator sets a new
// password from the admin screen - the auto-generated bootstrap password
// (see LoadOrCreateAdminPassword) is always longer than this anyway.
const MinAdminPasswordLength = 8

// AdminAuth holds the current admin password behind a lock, backed by a
// plain-text file, so it can be changed at runtime (from the admin
// screen's "Change Password" form) without a restart. Deliberately kept
// out of Config/ConfigDTO (never appears in a GET /api/config response
// or the human-readable operational config.json) and out of the command
// line (a CLI flag's value is visible to any local user via `ps`; a file
// path is not).
type AdminAuth struct {
	mu       sync.RWMutex
	password string
	file     string
}

// LoadOrCreateAdminAuth reads the admin password from path (plain text,
// trimmed - one line). If the file doesn't exist, it generates a random
// password, writes it to path (mode 0600) and logs it once - the same
// bootstrap pattern Jenkins/Grafana-style tools use, so the admin
// screens are usable without any manual pre-provisioning.
func LoadOrCreateAdminAuth(path string) (*AdminAuth, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if pw := strings.TrimSpace(string(data)); pw != "" {
			return &AdminAuth{password: pw, file: path}, nil
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	pw, err := generatePassword()
	if err != nil {
		return nil, fmt.Errorf("generating password: %w", err)
	}
	if err := os.WriteFile(path, []byte(pw+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	log.Printf("No admin password file at %s - generated one. Admin login: username %q, password %q (also saved to that file)",
		path, AdminUsername, pw)
	return &AdminAuth{password: pw, file: path}, nil
}

func generatePassword() (string, error) {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (a *AdminAuth) Password() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.password
}

// SetPassword validates and writes a new password to disk, then swaps
// it in for subsequent requests - no restart needed. Any request already
// in flight (or a browser's already-cached Basic Auth credentials) keeps
// using the old password until it's challenged again with a fresh 401,
// which is why the admin screen tells the operator to reload after a
// successful change.
func (a *AdminAuth) SetPassword(pw string) error {
	if len(pw) < MinAdminPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinAdminPasswordLength)
	}
	if err := os.WriteFile(a.file, []byte(pw+"\n"), 0600); err != nil {
		return fmt.Errorf("writing %s: %w", a.file, err)
	}
	a.mu.Lock()
	a.password = pw
	a.mu.Unlock()
	log.Printf("Admin password changed (saved to %s)", a.file)
	return nil
}

// requireAuth wraps a handler so it only runs for a request carrying
// valid HTTP Basic Auth credentials; otherwise it sends 401 with a
// WWW-Authenticate challenge, which every browser turns into its native
// login prompt - no custom login page or session/cookie handling needed.
// Always requires auth, regardless of where the request came from; see
// requireAuthUnlessLocal for the public customer screen's relaxed rule.
func (w *WebServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		validUser := ok && subtle.ConstantTimeCompare([]byte(user), []byte(AdminUsername)) == 1
		validPass := ok && subtle.ConstantTimeCompare([]byte(pass), []byte(w.auth.Password())) == 1
		if !validUser || !validPass {
			rw.Header().Set("WWW-Authenticate", `Basic realm="Hydrogen Booster Admin"`)
			http.Error(rw, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(rw, r)
	}
}

// requireAuthUnlessLocal skips the auth check for requests originating
// from localhost (the on-site kiosk showing the public customer screen
// directly on the Pi's own display), and otherwise requires the same
// credentials as requireAuth - so the customer screen and its supporting
// API calls are only ever open to the physical terminal, not to anyone
// else on the network.
func (w *WebServer) requireAuthUnlessLocal(next http.HandlerFunc) http.HandlerFunc {
	authed := w.requireAuth(next)
	return func(rw http.ResponseWriter, r *http.Request) {
		if isLocalRequest(r) {
			next(rw, r)
			return
		}
		authed(rw, r)
	}
}

// requireAuthUnlessLocalRead is requireAuthUnlessLocal's counterpart for
// a handler that serves both a public read and a staff-only write on
// the same route (currently just GET/POST /api/config): a GET from
// localhost is treated the same as requireAuthUnlessLocal (open to the
// on-site kiosk - the customer screen's initI18n() reads system
// name/language/mass-measurement/flow-sensor settings this way on every
// page load), but every POST - local or not - still requires the
// admin credentials, since that's what actually changes safety limits
// and other operational settings. Without this split, the customer
// kiosk's local, read-only /api/config fetch would trigger the
// browser's native Basic Auth prompt on every page load even though
// nothing on that screen needs the admin login.
func (w *WebServer) requireAuthUnlessLocalRead(next http.HandlerFunc) http.HandlerFunc {
	authed := w.requireAuth(next)
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && isLocalRequest(r) {
			next(rw, r)
			return
		}
		authed(rw, r)
	}
}

func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
