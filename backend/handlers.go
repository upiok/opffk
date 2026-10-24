package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// App wires everything the web backend needs.
type App struct {
	paths    *Paths
	cfg      *UIConfig
	sessions *sessionStore
	sup      *supervisor
	log      *logRing // core log (fed by the supervisor)
	webLog   *logRing // backend's own log
	version  string

	loginMu     sync.Mutex
	loginToken  string // pre-login CSRF token for the login form
	loginIssued time.Time
}

func NewApp(p *Paths, cfg *UIConfig, version string) *App {
	coreLog := newLogRing(p.CoreLogFile())
	webLog := newLogRing(p.WebLogFile())
	app := &App{
		paths:    p,
		cfg:      cfg,
		sessions: newSessionStore(),
		log:      coreLog,
		webLog:   webLog,
		version:  version,
	}
	app.sup = newSupervisor(p, coreLog)
	return app
}

// ---- response helpers --------------------------------------------------------

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// securityHeaders applies the baseline browser protections to every answer.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// ---- routing -------------------------------------------------------------------

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/login", app.handleLoginPage)
	// Static assets are served WITHOUT session auth: the login page itself
	// loads them. They contain no secrets (verified: embedded UI only).
	mux.HandleFunc("/app.js", app.serveStatic("app.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("/style.css", app.serveStatic("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/", app.withAuth(app.handleIndex))
	mux.HandleFunc("/api/state", app.withAuth(app.handleState))
	mux.HandleFunc("/api/logs", app.withAuth(app.handleLogs))
	mux.HandleFunc("/api/config", app.withAuthCSRF(app.handleConfig))
	mux.HandleFunc("/api/service/start", app.withAuthCSRF(app.handleServiceStart))
	mux.HandleFunc("/api/service/stop", app.withAuthCSRF(app.handleServiceStop))
	mux.HandleFunc("/api/service/restart", app.withAuthCSRF(app.handleServiceRestart))
	mux.HandleFunc("/api/link", app.withAuthCSRF(app.handleLink))
	mux.HandleFunc("/api/key/rotate", app.withAuthCSRF(app.handleKeyRotate))
	mux.HandleFunc("/api/password", app.withAuthCSRF(app.handlePassword))
	mux.HandleFunc("/api/update", app.withAuthCSRF(app.handleUpdate))
	mux.HandleFunc("/api/uninstall", app.withAuthCSRF(app.handleUninstall))
	mux.HandleFunc("/logout", app.withAuth(app.handleLogout))

	return securityHeaders(mux)
}

type authedHandler func(w http.ResponseWriter, r *http.Request, sess *session)

func (app *App) withAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := app.sessions.currentUser(r)
		if sess == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				jsonErr(w, http.StatusUnauthorized, "authentication required")
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, sess)
	}
}

func (app *App) withAuthCSRF(h authedHandler) http.HandlerFunc {
	return app.withAuth(func(w http.ResponseWriter, r *http.Request, sess *session) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		if !checkCSRF(r, sess) {
			jsonErr(w, http.StatusForbidden, "CSRF validation failed")
			return
		}
		h(w, r, sess)
	})
}

// handleLogout ends the session (POST with CSRF; a GET simply redirects).
func (app *App) handleLogout(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method == http.MethodPost && checkCSRF(r, sess) {
		app.sessions.drop(sess.token)
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- static ----------------------------------------------------------------

//go:embed web/index.html web/app.js web/style.css
var webFS embed.FS

func (app *App) handleIndex(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.URL.Path != "/" {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "index missing")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (app *App) serveStatic(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		_, _ = w.Write(data)
	}
}

// ---- login -------------------------------------------------------------------

// newLoginToken issues the pre-login CSRF token (single active token is
// enough; a reload replaces it).
func (app *App) newLoginToken() string {
	app.loginMu.Lock()
	defer app.loginMu.Unlock()
	tok, err := generateToken()
	if err != nil {
		return ""
	}
	app.loginToken = tok
	app.loginIssued = time.Now()
	return tok
}

func (app *App) takeLoginToken(tok string) bool {
	app.loginMu.Lock()
	defer app.loginMu.Unlock()
	if app.loginToken == "" || tok == "" {
		return false
	}
	if time.Since(app.loginIssued) > 15*time.Minute {
		app.loginToken = ""
		return false
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(app.loginToken)) != 1 {
		return false
	}
	app.loginToken = ""
	return true
}

func (app *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if app.sessions.currentUser(r) != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "index missing")
			return
		}
		tok := app.newLoginToken()
		if tok != "" {
			http.SetCookie(w, &http.Cookie{
				Name: "opffk_pre", Value: tok, Path: "/", MaxAge: 900,
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
		return
	case http.MethodPost:
	default:
		jsonErr(w, http.StatusMethodNotAllowed, "GET or POST required")
		return
	}
	if app.sessions.lockedOut() {
		jsonErr(w, http.StatusTooManyRequests, "too many failed attempts; wait a minute")
		return
	}
	// Cross-origin form posts are rejected even before the session exists.
	if origin := r.Header.Get("Origin"); origin != "" {
		o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
		if i := strings.Index(o, "/"); i >= 0 {
			o = o[:i]
		}
		if o != "" && !strings.EqualFold(o, r.Host) {
			jsonErr(w, http.StatusForbidden, "cross-origin login rejected")
			return
		}
	}
	pre, err := r.Cookie("opffk_pre")
	if err != nil || pre.Value == "" {
		jsonErr(w, http.StatusForbidden, "missing login token (reload the page)")
		return
	}
	if err := r.ParseForm(); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad form")
		return
	}
	if !app.takeLoginToken(pre.Value) {
		jsonErr(w, http.StatusForbidden, "stale login token (reload the page)")
		return
	}
	password := r.FormValue("password")
	sess, ok := app.sessions.login(password, app.cfg.Auth.Hash, app.cfg.Auth.Salt, app.cfg.Auth.Iterations)
	if !ok {
		app.logWebEvent("failed login attempt")
		jsonErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.token, Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	app.logWebEvent("login OK")

	// The one-time initial password is consumed after the first successful
	// login so it cannot be read later from the config directory.
	if _, still := consumeInitialPassword(app.paths); still {
		app.logWebEvent("one-time initial password consumed")
	}
	jsonOK(w, map[string]interface{}{"ok": true, "csrf": sess.csrfToken, "redirect": "/"})
}

func (app *App) logWebEvent(msg string) {
	app.webLog.write(time.Now().UTC().Format(time.RFC3339) + " " + msg)
}

// ---- state & logs ---------------------------------------------------------------

type stateResponse struct {
	Backend      string            `json:"backend"`
	OpffkVersion string            `json:"opffk_version"`
	Paths    map[string]string `json:"paths"`
	Service  Snapshot          `json:"service"`
	Config   *UIConfig         `json:"config"`
	Logs     []string          `json:"logs"`
	CSRF     string            `json:"csrf"`
	HasKey   bool              `json:"has_key"`
	Platform platformInfo      `json:"platform"`
}

type platformInfo struct {
	GOOS        string `json:"goos"`
	GOARCH      string `json:"goarch"`
	UnameM      string `json:"uname_m"`
	TestMode    bool   `json:"test_mode"`
	Root        string `json:"root"`
	BackendRoot bool   `json:"backend_root"`
}

func (app *App) handleState(w http.ResponseWriter, r *http.Request, sess *session) {
	resp := stateResponse{
		Backend:      "opffk " + app.version,
		OpffkVersion: opffkVersionInstalled(app.paths),
		Service:  app.sup.Snapshot(),
		Config:   app.cfg,
		Logs:     app.log.tail(120),
		CSRF:     sess.csrfToken,
		HasKey:   fileExists(app.paths.KeyFile()),
		Platform: platformInfo{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, UnameM: hostUnameM(), TestMode: TestMode(), Root: app.paths.Root, BackendRoot: os.Geteuid() == 0},
		Paths: map[string]string{
			"conf": app.paths.ConfFile(),
			"key":  app.paths.KeyFile(),
			"core": app.paths.CoreBin,
			"logs": app.paths.CoreLogFile(),
		},
	}
	jsonOK(w, resp)
}

func (app *App) handleLogs(w http.ResponseWriter, r *http.Request, sess *session) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := atoiClamp(v, 1, ringLines); err == nil {
			n = parsed
		}
	}
	jsonOK(w, map[string]interface{}{"logs": app.log.tail(n)})
}

func atoiClamp(s string, lo, hi int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
		if n > hi {
			n = hi
			break
		}
	}
	if n < lo {
		return 0, errors.New("too small")
	}
	return n, nil
}

// ---- config -------------------------------------------------------------------

func (app *App) handleConfig(w http.ResponseWriter, r *http.Request, sess *session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "body too large or unreadable")
		return
	}
	var req struct {
		Settings OpenFluxSettings `json:"settings"`
		Restart  bool             `json:"restart"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if errs := ValidateSettings(&req.Settings, app.cfg.AllowL3); len(errs) > 0 {
		jsonOK(w, map[string]interface{}{"ok": false, "errors": errs})
		return
	}

	// Persist settings, then render the core config from them.
	app.cfg.OpenFlux = req.Settings
	if err := app.cfg.Save(app.paths); err != nil {
		jsonErr(w, http.StatusInternalServerError, "save ui.json: "+err.Error())
		return
	}
	if err := WriteCoreConfig(app.paths, &req.Settings); err != nil {
		jsonErr(w, http.StatusInternalServerError, "write core config: "+err.Error())
		return
	}
	app.log.write("config: settings applied via web UI")

	restartErr := ""
	if req.Restart {
		if err := app.sup.Restart(); err != nil {
			restartErr = err.Error()
		}
	}
	jsonOK(w, map[string]interface{}{"ok": restartErr == "", "restart_error": restartErr})
}

// ---- service controls ---------------------------------------------------------------

func (app *App) handleServiceStart(w http.ResponseWriter, r *http.Request, sess *session) {
	if err := app.sup.Start(); err != nil {
		jsonOK(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	app.log.write("service: start requested via web UI")
	jsonOK(w, map[string]interface{}{"ok": true})
}

func (app *App) handleServiceStop(w http.ResponseWriter, r *http.Request, sess *session) {
	if err := app.sup.Stop(); err != nil {
		jsonOK(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	app.log.write("service: stop requested via web UI")
	jsonOK(w, map[string]interface{}{"ok": true})
}

func (app *App) handleServiceRestart(w http.ResponseWriter, r *http.Request, sess *session) {
	if err := app.sup.Restart(); err != nil {
		jsonOK(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	app.log.write("service: restart requested via web UI")
	jsonOK(w, map[string]interface{}{"ok": true})
}

// ---- client link -----------------------------------------------------------------

// handleLink builds an openflux:// client link with the pinned core's
// --make-link (JSON on stdin — settings never reach the command line).
// The link contains the encryption key; it is only produced on explicit
// request and must be treated as a secret.
func (app *App) handleLink(w http.ResponseWriter, r *http.Request, sess *session) {
	if !fileExists(app.paths.CoreBin) {
		jsonErr(w, http.StatusConflict, "core binary missing")
		return
	}
	key, err := os.ReadFile(app.paths.KeyFile())
	if err != nil {
		jsonErr(w, http.StatusConflict, "encryption key missing — apply settings first")
		return
	}
	secret := strings.TrimSpace(string(key))

	if err := r.ParseForm(); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad form")
		return
	}
	shareHost := r.FormValue("share_host")
	if !confSafe(shareHost) {
		jsonErr(w, http.StatusBadRequest, "invalid share host")
		return
	}
	if shareHost == "" {
		shareHost = app.publicHostHint()
	}

	type shareTransport struct {
		Type     string `json:"type,omitempty"`
		Name     string `json:"name,omitempty"`
		URL      string `json:"url,omitempty"`
		Priority int    `json:"priority"`
		Dial     string `json:"dial,omitempty"`
	}
	cfg := struct {
		Name       string           `json:"name"`
		Negotiate  bool             `json:"negotiate"`
		Secret     string           `json:"secret"`
		Context    string           `json:"context,omitempty"`
		Transports []shareTransport `json:"transports"`
	}{
		Name:      "opffk exit",
		Negotiate: true,
		Secret:    secret,
		Context:   app.cfg.OpenFlux.SessionContext,
	}
	for _, t := range app.cfg.OpenFlux.Transports {
		st := shareTransport{Type: t.Type, URL: t.URL, Priority: t.Priority, Name: t.Type}
		switch t.Type {
		case "direct":
			port := ""
			if i := strings.LastIndex(t.Listen, ":"); i >= 0 {
				port = t.Listen[i+1:]
			}
			if shareHost == "" || port == "" {
				continue
			}
			st.Dial = shareHost + ":" + port
			st.URL = ""
		case "oneme":
			continue // a MAX token belongs to one account; clients need their own
		}
		if t.Type != "direct" && st.URL == "" {
			continue
		}
		cfg.Transports = append(cfg.Transports, st)
	}
	if len(cfg.Transports) == 0 {
		jsonErr(w, http.StatusConflict, "no dialable transport for a client link (direct needs a public share host)")
		return
	}
	payload, _ := json.Marshal(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, app.paths.CoreBin, "--make-link", "-")
	cmd.Stdin = strings.NewReader(string(payload))
	out, err := cmd.Output()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "make-link failed: "+err.Error())
		return
	}
	var result struct {
		Link   string          `json:"link"`
		Error  string          `json:"error"`
		Config json.RawMessage `json:"config"` // object; opaque to the backend
	}
	if err := json.Unmarshal(out, &result); err != nil {
		jsonErr(w, http.StatusInternalServerError, "make-link output unreadable: "+err.Error())
		return
	}
	if result.Error != "" {
		jsonErr(w, http.StatusConflict, "make-link: "+result.Error)
		return
	}
	app.log.write("link: client link generated via web UI (secret displayed once)")
	jsonOK(w, map[string]string{"link": result.Link})
}

// publicHostHint suggests the share host for direct clients: the host part
// of the configured direct Dial, else empty.
func (app *App) publicHostHint() string {
	for _, t := range app.cfg.OpenFlux.Transports {
		if t.Type == "direct" && t.Dial != "" {
			if i := strings.LastIndex(t.Dial, ":"); i > 0 {
				return t.Dial[:i]
			}
		}
	}
	return ""
}

// ---- key & password ------------------------------------------------------------------

func (app *App) handleKeyRotate(w http.ResponseWriter, r *http.Request, sess *session) {
	key, err := GenerateKey()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	if err := writeFileAtomic(app.paths.KeyFile(), []byte(key+"\n"), 0o600); err != nil {
		jsonErr(w, http.StatusInternalServerError, "write key: "+err.Error())
		return
	}
	app.log.write("key: encryption key rotated via web UI (clients must re-pair)")
	go func() { _ = app.sup.Restart() }()
	jsonOK(w, map[string]interface{}{"ok": true, "message": "key rotated; core restarting; existing client links are now invalid"})
}

func (app *App) handlePassword(w http.ResponseWriter, r *http.Request, sess *session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad body")
		return
	}
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if !pbkdf2Verify(req.Current, app.cfg.Auth.Salt, app.cfg.Auth.Hash, app.cfg.Auth.Iterations) {
		jsonErr(w, http.StatusForbidden, "current password is wrong")
		return
	}
	if len(req.New) < 10 || len(req.New) > 128 {
		jsonErr(w, http.StatusBadRequest, "new password must be 10..128 characters")
		return
	}
	salt, hash, err := pbkdf2Hash(req.New, app.cfg.Auth.Iterations)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "hash failed")
		return
	}
	app.cfg.Auth.Salt = hexEnc(salt)
	app.cfg.Auth.Hash = hexEnc(hash)
	if err := app.cfg.Save(app.paths); err != nil {
		jsonErr(w, http.StatusInternalServerError, "save: "+err.Error())
		return
	}
	app.sessions.drop(sess.token)
	app.logWebEvent("password changed via web UI")
	jsonOK(w, map[string]interface{}{"ok": true, "message": "password changed; log in again"})
}

// ---- uninstall ------------------------------------------------------------------------

func (app *App) handleUninstall(w http.ResponseWriter, r *http.Request, sess *session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad body")
		return
	}
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Confirm), []byte("UNINSTALL")) != 1 {
		jsonErr(w, http.StatusBadRequest, "type UNINSTALL to confirm")
		return
	}
	jsonOK(w, app.uninstall())
}

func (app *App) uninstall() map[string]interface{} {
	steps := []string{}
	backup := ""
	if tarPath, err := app.makeBackup(); err == nil {
		backup = tarPath
		steps = append(steps, "backup saved to "+tarPath)
	} else {
		steps = append(steps, "backup FAILED: "+err.Error())
	}
	if err := app.sup.Stop(); err != nil {
		steps = append(steps, "stop core: "+err.Error())
	} else {
		steps = append(steps, "core stopped")
	}
	if !TestMode() {
		if err := os.Remove(app.paths.InitScript); err == nil {
			steps = append(steps, "init script removed: "+app.paths.InitScript)
		} else {
			steps = append(steps, "init script remove failed (may already be gone): "+err.Error())
		}
	}
	for _, f := range []string{app.paths.ConfFile(), app.paths.KeyFile(), app.paths.UIConfigFile(),
		app.paths.InitialPassFile(), app.paths.CoreBin, app.paths.CorePrevBin, app.paths.ManifestFile()} {
		if err := os.Remove(f); err == nil {
			steps = append(steps, "removed "+f)
		}
	}
	app.log.write("uninstall: requested via web UI; opffk-web keeps running until stopped")
	return map[string]interface{}{
		"ok":     true,
		"steps":  steps,
		"backup": backup,
		"message": "Config, key and binaries removed. Stop opffk-web (kill $(cat " + app.paths.PIDFile() +
			")) and delete " + app.paths.LogDir + " and " + app.paths.StateDir +
			" when you no longer need the logs and backups. The service account openflux in " +
			app.paths.PasswdFile + " can be removed by hand.",
	}
}

// makeBackup tars the configuration directory into the state directory.
func (app *App) makeBackup() (string, error) {
	if _, err := exec.LookPath("tar"); err != nil {
		return "", errors.New("tar is not available")
	}
	name := "opffk-backup-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	dst := app.paths.StateDir + "/" + name
	if err := os.MkdirAll(app.paths.StateDir, 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("tar", "-czf", dst, "-C", app.paths.EtcDir, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("tar: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = os.Chmod(dst, 0o600)
	pruneBackups(app.paths.StateDir, 5)
	return dst, nil
}

func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "opffk-backup-") && strings.HasSuffix(e.Name(), ".tar.gz") {
			names = append(names, e.Name())
		}
	}
	for i := 0; i < len(names)-keep; i++ {
		_ = os.Remove(dir + "/" + names[i])
	}
}

// ---- update --------------------------------------------------------------------------

func (app *App) handleUpdate(w http.ResponseWriter, r *http.Request, sess *session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad body")
		return
	}
	var req struct {
		Action string `json:"action"` // check | apply | rollback | check_self | apply_full
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON")
		return
	}
	switch req.Action {
	case "check":
		res, err := app.CheckUpdate()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, res)
	case "apply":
		res, err := app.ApplyUpdate()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, res)
	case "check_self":
		res, err := app.CheckSelfUpdate()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, res)
	case "apply_full":
		res, err := app.ApplySelfUpdate()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, res) // on a real install the backend re-execs mid-call
	case "rollback":
		res, err := app.Rollback()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, res)
	default:
		jsonErr(w, http.StatusBadRequest, "action must be check|apply|rollback")
	}
}

func init() {
	// The stdlib logger is only used for fatal startup errors on stderr.
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("opffk-web: ")
}
