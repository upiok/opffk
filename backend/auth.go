package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---- password hashing (PBKDF2-HMAC-SHA256, RFC 2898) ------------------------
//
// Implemented with the standard library only; no third-party dependencies
// are allowed in this backend. Iterations default to 120000 — slow enough
// to make offline guessing expensive, fast enough for a ~1 GHz router core
// (well under a second per attempt even there).

const pbkdf2DefaultIterations = 120000

func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	u := make([]byte, hashLen)
	dk := make([]byte, 0, numBlocks*hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf[:], uint32(block))
		prf.Write(buf[:])
		u = prf.Sum(u[:0])
		t := make([]byte, hashLen)
		copy(t, u)
		for n := 2; n <= iterations; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := range u {
				t[x] ^= u[x]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func pbkdf2Hash(password string, iterations int) (salt, hash []byte, err error) {
	salt = make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return nil, nil, err
	}
	hash = pbkdf2SHA256([]byte(password), salt, iterations, 32)
	return salt, hash, nil
}

func pbkdf2Verify(password string, saltHex, hashHex string, iterations int) bool {
	salt, err1 := hex.DecodeString(saltHex)
	want, err2 := hex.DecodeString(hashHex)
	if err1 != nil || err2 != nil || len(salt) == 0 || len(want) == 0 || iterations < 1 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func hexEnc(b []byte) string { return hex.EncodeToString(b) }

// generatePassword returns n characters from a 58-char alphabet without
// visually ambiguous glyphs — safe to read aloud from the console once.
func generatePassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, n*2)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, 0, n)
	for _, b := range raw {
		if len(out) == n {
			break
		}
		// Rejection sampling keeps the distribution uniform.
		if int(b) >= 232 { // 232 = 58*4, the largest multiple of 58 in a byte
			continue
		}
		out = append(out, alphabet[int(b)%len(alphabet)])
	}
	return string(out), nil
}

// generateToken returns a 32-byte random token, hex encoded.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hexEnc(b), nil
}

// ---- sessions ----------------------------------------------------------------

type session struct {
	token     string
	created   time.Time
	lastSeen  time.Time
	csrfToken string
}

const (
	sessionCookie    = "opffk_session"
	sessionTTL       = 12 * time.Hour
	maxSessions      = 8
	maxLoginAttempts = 5
	loginLockout     = 60 * time.Second
)

type sessionStore struct {
	mu        sync.Mutex
	sessions  map[string]*session
	fails     int
	lockUntil time.Time
}

func newSessionStore() *sessionStore { return &sessionStore{sessions: make(map[string]*session)} }

// login validates the password, applies the failure lockout and creates a
// session. The returned bool is false on wrong password or lockout.
func (s *sessionStore) login(password, hashHex, saltHex string, iterations int) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.lockUntil) {
		return nil, false
	}
	if !pbkdf2Verify(password, saltHex, hashHex, iterations) {
		s.fails++
		if s.fails >= maxLoginAttempts {
			s.lockUntil = time.Now().Add(loginLockout)
			s.fails = 0
		}
		return nil, false
	}
	s.fails = 0
	if len(s.sessions) >= maxSessions {
		// Drop the oldest session to make room.
		var oldest *session
		for _, sess := range s.sessions {
			if oldest == nil || sess.lastSeen.Before(oldest.lastSeen) {
				oldest = sess
			}
		}
		if oldest != nil {
			delete(s.sessions, oldest.token)
		}
	}
	token, err := generateToken()
	if err != nil {
		return nil, false
	}
	csrf, err := generateToken()
	if err != nil {
		return nil, false
	}
	now := time.Now()
	sess := &session{token: token, created: now, lastSeen: now, csrfToken: csrf}
	s.sessions[token] = sess
	return sess, true
}

func (s *sessionStore) get(token string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return nil, false
	}
	if time.Since(sess.lastSeen) > sessionTTL || time.Since(sess.created) > sessionTTL {
		delete(s.sessions, token)
		return nil, false
	}
	sess.lastSeen = time.Now()
	return sess, true
}

func (s *sessionStore) drop(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func (s *sessionStore) lockedOut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.lockUntil)
}

// ---- HTTP auth & CSRF middleware ----------------------------------------------

// currentUser resolves the session cookie to a session, if valid.
func (s *sessionStore) currentUser(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	sess, ok := s.get(c.Value)
	if !ok {
		return nil
	}
	return sess
}

// checkCSRF enforces CSRF protection on state-changing requests:
// the token must arrive in the X-Opffk-CSRF header (fetch) or the
// csrf_token form field (no-JS fallback) and match the session token;
// an Origin header, when present, must match the request host.
func checkCSRF(r *http.Request, sess *session) bool {
	if sess == nil {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u := origin
		u = strings.TrimPrefix(u, "https://")
		u = strings.TrimPrefix(u, "http://")
		if i := strings.Index(u, "/"); i >= 0 {
			u = u[:i]
		}
		if u != "" && !strings.EqualFold(u, r.Host) {
			return false
		}
	}
	tok := r.Header.Get("X-Opffk-CSRF")
	if tok == "" {
		tok = r.FormValue("csrf_token")
	}
	if tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(sess.csrfToken)) == 1
}
