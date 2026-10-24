package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression: the login page loads /style.css and /app.js before any
// session exists — the assets must be served without authentication and
// with their real content types (GitHub issue: unstyled login page).
func TestStaticAssetsServedWithoutAuth(t *testing.T) {
	t.Setenv("OPFFK_ROOT", t.TempDir())
	p := NewPaths()
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultUIConfig()
	salt, hash, _ := pbkdf2Hash("pw", 100)
	cfg.Auth.Salt = hexEnc(salt)
	cfg.Auth.Hash = hexEnc(hash)
	app := NewApp(p, cfg, "test")
	handler := app.routes()

	cases := []struct {
		path       string
		wantCT     string
		wantSubstr string
	}{
		{"/style.css", "text/css", "--accent"},
		{"/app.js", "application/javascript", "opffk web UI"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", c.path, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, c.wantCT) {
			t.Errorf("%s: content-type %q, want prefix %q", c.path, ct, c.wantCT)
		}
		if !strings.Contains(w.Body.String(), c.wantSubstr) {
			t.Errorf("%s: body lacks %q", c.path, c.wantSubstr)
		}
	}

	// The API stays locked without a session.
	r := httptest.NewRequest("GET", "/api/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/state without session: status %d, want 401", w.Code)
	}
}

// Regression: the login page renders its form (it is the entry point).
func TestLoginPageServesHTML(t *testing.T) {
	t.Setenv("OPFFK_ROOT", t.TempDir())
	p := NewPaths()
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultUIConfig()
	salt, hash, _ := pbkdf2Hash("pw", 100)
	cfg.Auth.Salt = hexEnc(salt)
	cfg.Auth.Hash = hexEnc(hash)
	app := NewApp(p, cfg, "test")

	r := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "login-form") {
		t.Fatalf("/login: status %d, body lacks login form", w.Code)
	}
	if w.Header().Get("Set-Cookie") == "" {
		t.Error("/login did not issue the pre-login token cookie")
	}
}
