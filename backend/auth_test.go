package main

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPBKDF2Vectors checks the hand-rolled PBKDF2-HMAC-SHA256 against
// published test vectors (RFC 7914 §11 / well-known SHA-256 vectors).
func TestPBKDF2Vectors(t *testing.T) {
	cases := []struct {
		password, salt string
		iter, keyLen   int
		want           string
	}{
		{"password", "salt", 1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(pbkdf2SHA256([]byte(c.password), []byte(c.salt), c.iter, c.keyLen))
		if got != c.want {
			t.Errorf("pbkdf2(p=%q s=%q i=%d l=%d):\n got  %s\n want %s", c.password, c.salt, c.iter, c.keyLen, got, c.want)
		}
	}
}

func TestPBKDF2Verify(t *testing.T) {
	salt, hash, err := pbkdf2Hash("correct horse", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !pbkdf2Verify("correct horse", hex.EncodeToString(salt), hex.EncodeToString(hash), 1000) {
		t.Error("verify failed for the correct password")
	}
	if pbkdf2Verify("wrong", hex.EncodeToString(salt), hex.EncodeToString(hash), 1000) {
		t.Error("verify accepted a wrong password")
	}
	if pbkdf2Verify("correct horse", "zz", hex.EncodeToString(hash), 1000) {
		t.Error("verify accepted malformed salt")
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw, err := generatePassword(20)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 20 {
			t.Fatalf("password length %d != 20", len(pw))
		}
		if strings.ContainsAny(pw, "0O1lIo") {
			t.Fatalf("ambiguous character in %q", pw)
		}
		if seen[pw] {
			t.Fatalf("duplicate password generated: %q", pw)
		}
		seen[pw] = true
	}
}

func TestGenerateToken(t *testing.T) {
	a, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generateToken()
	if a == b || len(a) != 64 {
		t.Fatalf("tokens not random/64-hex: %q %q", a, b)
	}
}

func newTestStore() *sessionStore { return newSessionStore() }

func TestSessionLoginFlow(t *testing.T) {
	s := newTestStore()
	salt, hash, _ := pbkdf2Hash("secretpw", 1000)
	sh, ss := hexEnc(hash), hexEnc(salt)

	sess, ok := s.login("secretpw", sh, ss, 1000)
	if !ok || sess == nil {
		t.Fatal("login with correct password failed")
	}
	if s.currentUser(reqWithCookie(sess.token)) == nil {
		t.Fatal("currentUser did not resolve the session")
	}
	if s.currentUser(reqWithCookie("no-such-token")) != nil {
		t.Fatal("currentUser resolved a bogus token")
	}
	s.drop(sess.token)
	if s.currentUser(reqWithCookie(sess.token)) != nil {
		t.Fatal("dropped session still valid")
	}
}

func TestSessionLockout(t *testing.T) {
	s := newTestStore()
	salt, hash, _ := pbkdf2Hash("secretpw", 100)
	sh, ss := hexEnc(hash), hexEnc(salt)
	for i := 0; i < maxLoginAttempts; i++ {
		if _, ok := s.login("bad"+string(rune('a'+i)), sh, ss, 100); ok {
			t.Fatal("wrong password accepted")
		}
	}
	if !s.lockedOut() {
		t.Fatal("lockout not active after max failures")
	}
	if _, ok := s.login("secretpw", sh, ss, 100); ok {
		t.Fatal("correct password accepted during lockout")
	}
}

func reqWithCookie(token string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	return r
}

func TestCheckCSRF(t *testing.T) {
	s := newTestStore()
	salt, hash, _ := pbkdf2Hash("pw", 100)
	sess, ok := s.login("pw", hexEnc(hash), hexEnc(salt), 100)
	if !ok {
		t.Fatal("login failed")
	}

	mk := func(header, form, origin, host string) *http.Request {
		r := httptest.NewRequest("POST", "http://"+host+"/api/config", strings.NewReader("csrf_token="+form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if header != "" {
			r.Header.Set("X-Opffk-CSRF", header)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	if !checkCSRF(mk(sess.csrfToken, "", "", "192.168.1.1:8791"), sess) {
		t.Error("valid header token rejected")
	}
	if !checkCSRF(mk("", sess.csrfToken, "", "192.168.1.1:8791"), sess) {
		t.Error("valid form token rejected")
	}
	if checkCSRF(mk("bad-token", "", "", "h"), sess) {
		t.Error("bad token accepted")
	}
	if checkCSRF(mk(sess.csrfToken, "", "", "h"), nil) {
		t.Error("nil session accepted")
	}
	if checkCSRF(mk(sess.csrfToken, "", "http://evil.example", "192.168.1.1:8791"), sess) {
		t.Error("cross-origin request accepted")
	}
	if checkCSRF(mk(sess.csrfToken, "", "http://192.168.1.1:8791", "192.168.1.1:8791"), sess) {
		// matching origin is fine
	} else {
		t.Error("same-origin request rejected")
	}
}
