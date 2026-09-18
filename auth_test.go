package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestLoadOrCreateAdminAuthGeneratesOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/admin-password.txt"

	auth1, err := LoadOrCreateAdminAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if auth1.Password() == "" {
		t.Fatal("expected a non-empty generated password")
	}

	// A second call must read back the same password, not generate a
	// new one each restart.
	auth2, err := LoadOrCreateAdminAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if auth1.Password() != auth2.Password() {
		t.Errorf("password changed across calls: %q vs %q", auth1.Password(), auth2.Password())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreateAdminAuthReadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/admin-password.txt"
	if err := os.WriteFile(path, []byte("s3cret12\n"), 0600); err != nil {
		t.Fatal(err)
	}
	auth, err := LoadOrCreateAdminAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Password() != "s3cret12" {
		t.Errorf("got %q, want s3cret12", auth.Password())
	}
}

func TestAdminAuthSetPassword(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/admin-password.txt"
	auth, err := LoadOrCreateAdminAuth(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := auth.SetPassword("new-password-123"); err != nil {
		t.Fatal(err)
	}
	if auth.Password() != "new-password-123" {
		t.Errorf("in-memory password not updated, got %q", auth.Password())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "new-password-123" {
		t.Errorf("file not updated, got %q", string(data))
	}
}

func TestAdminAuthSetPasswordRejectsTooShort(t *testing.T) {
	dir := t.TempDir()
	auth, err := LoadOrCreateAdminAuth(dir + "/admin-password.txt")
	if err != nil {
		t.Fatal(err)
	}
	original := auth.Password()

	if err := auth.SetPassword("short"); err == nil {
		t.Error("expected an error for a password under the minimum length")
	}
	if auth.Password() != original {
		t.Error("password should not change when the new one is rejected")
	}
}

func TestIsLocalRequest(t *testing.T) {
	cases := []struct {
		remoteAddr string
		want       bool
	}{
		{"127.0.0.1:54321", true},
		{"[::1]:54321", true},
		{"192.168.1.50:54321", false},
		{"10.0.0.5:1234", false},
	}
	for _, c := range cases {
		r := &http.Request{RemoteAddr: c.remoteAddr}
		if got := isLocalRequest(r); got != c.want {
			t.Errorf("isLocalRequest(%q) = %v, want %v", c.remoteAddr, got, c.want)
		}
	}
}

func newTestWebServer(t *testing.T, password string) *WebServer {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/admin-password.txt"
	if err := os.WriteFile(path, []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	auth, err := LoadOrCreateAdminAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	return &WebServer{auth: auth}
}

func TestRequireAuthRejectsMissingOrWrongCredentials(t *testing.T) {
	w := newTestWebServer(t, "correct-horse")
	called := false
	h := w.requireAuth(func(rw http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest("GET", "/admin.html", nil)
	rw := httptest.NewRecorder()
	h(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("no creds: status = %d, want 401", rw.Code)
	}
	if called {
		t.Error("handler should not run without credentials")
	}

	req = httptest.NewRequest("GET", "/admin.html", nil)
	req.SetBasicAuth(AdminUsername, "wrong")
	rw = httptest.NewRecorder()
	h(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", rw.Code)
	}

	req = httptest.NewRequest("GET", "/admin.html", nil)
	req.SetBasicAuth(AdminUsername, "correct-horse")
	rw = httptest.NewRecorder()
	h(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("correct creds: status = %d, want 200", rw.Code)
	}
	if !called {
		t.Error("handler should run with correct credentials")
	}
}

func TestRequireAuthUnlessLocalAllowsLocalWithoutCredentials(t *testing.T) {
	w := newTestWebServer(t, "correct-horse")
	called := false
	h := w.requireAuthUnlessLocal(func(rw http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	rw := httptest.NewRecorder()
	h(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("local request: status = %d, want 200", rw.Code)
	}
	if !called {
		t.Error("handler should run for a local request without credentials")
	}
}

func TestRequireAuthUnlessLocalRequiresCredentialsRemotely(t *testing.T) {
	w := newTestWebServer(t, "correct-horse")
	called := false
	h := w.requireAuthUnlessLocal(func(rw http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.77:55555"
	rw := httptest.NewRecorder()
	h(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("remote request without creds: status = %d, want 401", rw.Code)
	}
	if called {
		t.Error("handler should not run for a remote request without credentials")
	}
}

func TestHandleChangePasswordRequiresCorrectCurrentPassword(t *testing.T) {
	w := newTestWebServer(t, "correct-horse")

	body := strings.NewReader(`{"currentPassword":"wrong","newPassword":"brand-new-pw"}`)
	req := httptest.NewRequest("POST", "/api/admin/password", body)
	rw := httptest.NewRecorder()
	w.handleChangePassword(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rw.Code)
	}
	if w.auth.Password() != "correct-horse" {
		t.Error("password should not change when the current password is wrong")
	}
}

func TestHandleChangePasswordSucceeds(t *testing.T) {
	w := newTestWebServer(t, "correct-horse")

	body := strings.NewReader(`{"currentPassword":"correct-horse","newPassword":"brand-new-pw"}`)
	req := httptest.NewRequest("POST", "/api/admin/password", body)
	rw := httptest.NewRecorder()
	w.handleChangePassword(rw, req)

	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rw.Code)
	}
	if w.auth.Password() != "brand-new-pw" {
		t.Errorf("password not updated, got %q", w.auth.Password())
	}
}

func TestHandleChangePasswordRejectsTooShortNewPassword(t *testing.T) {
	w := newTestWebServer(t, "correct-horse")

	body := strings.NewReader(`{"currentPassword":"correct-horse","newPassword":"short"}`)
	req := httptest.NewRequest("POST", "/api/admin/password", body)
	rw := httptest.NewRecorder()
	w.handleChangePassword(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rw.Code)
	}
	if w.auth.Password() != "correct-horse" {
		t.Error("password should not change when the new one is too short")
	}
}
