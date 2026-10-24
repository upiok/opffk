package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Core updates work against a manifest.json in the schema committed to this
// repository (see the repo root manifest.json). The manifest pins the
// upstream core version and the SHA-256 of each published binary; the
// backend downloads only over HTTPS, only from the URL template in the
// manifest, and refuses to install anything whose hash does not match.
//
// The previous core binary is kept at <prefix>/bin/openflux.previous so a
// failed update (or a bad release) can be rolled back — automatically when
// the new core fails the health check, and manually from the web UI.

const (
	maxManifestBytes = 1 << 20  // 1 MB
	maxCoreBytes     = 96 << 20 // 96 MB (upstream node binaries are ~25 MB)
	updateTimeout    = 5 * time.Minute
	healthTimeout    = 30 * time.Second
)

type Manifest struct {
	Schema int `json:"schema"`
	Core   struct {
		Project     string            `json:"project"`
		Repository  string            `json:"repository"`
		Version     string            `json:"version"`
		Channel     string            `json:"channel"`
		URLTemplate string            `json:"url_template"`
		SHA256      map[string]string `json:"sha256"`
	} `json:"core"`
}

func parseManifest(data []byte) (*Manifest, error) {
	m := &Manifest{}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Schema != 1 {
		return nil, fmt.Errorf("unsupported manifest schema %d", m.Schema)
	}
	if m.Core.Version == "" || m.Core.URLTemplate == "" || len(m.Core.SHA256) == 0 {
		return nil, errors.New("manifest missing core version, url_template or sha256")
	}
	if !strings.Contains(m.Core.URLTemplate, "{arch}") {
		return nil, errors.New("manifest url_template lacks {arch}")
	}
	for arch, h := range m.Core.SHA256 {
		if _, ok := archToUpstream(archNameFor(arch)); !ok {
			return nil, fmt.Errorf("manifest lists unsupported arch %q", arch)
		}
		if len(h) != 64 {
			return nil, fmt.Errorf("manifest sha256 for %s is malformed", arch)
		}
	}
	return m, nil
}

// archNameFor maps manifest keys ("arm") to uname-style names used by
// archToUpstream ("armv7l"); amd64/arm64 map to themselves.
func archNameFor(arch string) string {
	if arch == "arm" {
		return "armv7l"
	}
	return arch
}

// loadManifestFrom parses a manifest at an arbitrary path (used by the
// self-update to cross-check the bundled core).
func loadManifestFrom(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseManifest(data)
}

func loadLocalManifest(p *Paths) (*Manifest, error) {
	data, err := os.ReadFile(p.ManifestFile())
	if err != nil {
		return nil, err
	}
	return parseManifest(data)
}

func fetchManifest(url string) (*Manifest, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errors.New("update manifest URL must be https")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest fetch: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return nil, err
	}
	return parseManifest(data)
}

// fetchCore downloads the manifest-pinned core binary for arch into dst,
// verifying the SHA-256 while streaming.
func fetchCore(m *Manifest, arch, dst string) error {
	sumHex, ok := m.Core.SHA256[arch]
	if !ok {
		return fmt.Errorf("manifest has no sha256 for arch %s", arch)
	}
	url := strings.Replace(m.Core.URLTemplate, "{arch}", arch, 1)
	client := &http.Client{Timeout: updateTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("core download: HTTP %d from %s", resp.StatusCode, url)
	}

	hasher := sha256.New()
	tmp := dst + ".download"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(resp.Body, maxCoreBytes+1))
	f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if n > maxCoreBytes {
		_ = os.Remove(tmp)
		return errors.New("core download exceeds size limit")
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != strings.ToLower(sumHex) {
		_ = os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: got %s, manifest pins %s (refusing to install)", shortHash(got), shortHash(sumHex))
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16] + "…"
	}
	return h
}

// coreVersionInstalled reads the version recorded at install/update time.
func coreVersionInstalled(p *Paths) string {
	data, err := os.ReadFile(p.CoreVersionFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// coreVersionNewer reports whether b (candidate) is a newer node-vX.Y.Z than a.
func coreVersionNewer(a, b string) bool {
	pa, ok1 := splitNodeVersion(a)
	pb, ok2 := splitNodeVersion(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if pb[i] != pa[i] {
			return pb[i] > pa[i]
		}
	}
	return false
}

func splitNodeVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "node-v")
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, s := range parts {
		// Tolerate suffixes like "1.3.1-rc1" on the last component.
		if j := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
			s = s[:j]
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// UpdateResult is the JSON answer for the update endpoints.
type UpdateResult struct {
	OK         bool   `json:"ok"`
	Action     string `json:"action"` // check | update | rollback
	Installed  string `json:"installed"`
	Candidate  string `json:"candidate,omitempty"`
	Message    string `json:"message,omitempty"`
	RolledBack bool   `json:"rolled_back,omitempty"`
}

// CheckUpdate compares the installed core with the configured manifest URL,
// falling back to the local (install-time) manifest.
func (app *App) CheckUpdate() (*UpdateResult, error) {
	installed := coreVersionInstalled(app.paths)
	res := &UpdateResult{Action: "check", Installed: installed}
	var m *Manifest
	if app.cfg.UpdateManifestURL != "" {
		fetched, err := fetchManifest(app.cfg.UpdateManifestURL)
		if err != nil {
			res.Message = "manifest fetch failed: " + err.Error()
			return res, nil
		}
		m = fetched
	} else {
		local, err := loadLocalManifest(app.paths)
		if err != nil {
			res.Message = "no update URL configured and no local manifest: " + err.Error()
			return res, nil
		}
		m = local
	}
	res.Candidate = m.Core.Version
	switch {
	case installed == "":
		res.Message = "no core version recorded; run update to install " + m.Core.Version
	case installed == m.Core.Version:
		res.OK = true
		res.Message = "up to date"
	case !coreVersionNewer(installed, m.Core.Version):
		res.OK = true
		res.Message = "installed " + installed + " is newer than candidate " + m.Core.Version
	default:
		res.OK = true
		res.Message = "update available: " + m.Core.Version
	}
	return res, nil
}

// ApplyUpdate downloads the candidate core, verifies its hash, swaps it in,
// restarts the core and health-checks it; on failure it rolls back.
func (app *App) ApplyUpdate() (*UpdateResult, error) {
	res := &UpdateResult{Action: "update"}
	var m *Manifest
	if app.cfg.UpdateManifestURL != "" {
		fetched, err := fetchManifest(app.cfg.UpdateManifestURL)
		if err != nil {
			return nil, fmt.Errorf("manifest fetch failed: %w", err)
		}
		m = fetched
	} else {
		local, err := loadLocalManifest(app.paths)
		if err != nil {
			return nil, fmt.Errorf("no update URL configured and no local manifest: %w", err)
		}
		m = local
	}
	installed := coreVersionInstalled(app.paths)
	if installed != "" && !coreVersionNewer(installed, m.Core.Version) {
		res.OK = true
		res.Installed = installed
		res.Candidate = m.Core.Version
		res.Message = "nothing to do (installed " + installed + ")"
		return res, nil
	}

	arch, ok := archToUpstream(hostUnameM())
	if !ok {
		return nil, fmt.Errorf("architecture %s has no upstream core binary (see docs/COMPATIBILITY.md)", hostUnameM())
	}
	if err := fetchCore(m, arch, app.paths.CoreBin+".new"); err != nil {
		return nil, fmt.Errorf("download/verify failed: %w", err)
	}

	app.log.write("update: installing " + m.Core.Version + " (" + arch + ")")
	wasRunning := false
	snap := app.sup.Snapshot()
	if snap.Running {
		wasRunning = true
		if err := app.sup.Stop(); err != nil {
			return nil, fmt.Errorf("stop old core: %w", err)
		}
	}

	// Swap binaries. Keep the previous one for rollback.
	if fileExists(app.paths.CorePrevBin) {
		_ = os.Remove(app.paths.CorePrevBin)
	}
	hadOld := fileExists(app.paths.CoreBin)
	if hadOld {
		if err := os.Rename(app.paths.CoreBin, app.paths.CorePrevBin); err != nil {
			return nil, fmt.Errorf("swap: %w", err)
		}
	}
	if err := os.Rename(app.paths.CoreBin+".new", app.paths.CoreBin); err != nil {
		if hadOld {
			_ = os.Rename(app.paths.CorePrevBin, app.paths.CoreBin)
		}
		return nil, fmt.Errorf("swap: %w", err)
	}
	_ = os.WriteFile(app.paths.CoreVersionFile(), []byte(m.Core.Version+"\n"), 0o644)

	healthErr := app.healthCheck()
	if healthErr == nil {
		res.OK = true
		res.Installed = m.Core.Version
		res.Candidate = m.Core.Version
		res.Message = "updated and healthy"
		app.log.write("update: " + m.Core.Version + " is healthy")
		return res, nil
	}

	// Rollback.
	app.log.write("update: health check failed (" + healthErr.Error() + "), rolling back")
	if hadOld {
		_ = os.Remove(app.paths.CoreBin)
		_ = os.Rename(app.paths.CorePrevBin, app.paths.CoreBin)
	}
	_ = os.WriteFile(app.paths.CoreVersionFile(), []byte(installed+"\n"), 0o644)
	rbErr := ""
	if wasRunning {
		if err := app.sup.Start(); err != nil {
			rbErr = " (rollback start failed: " + err.Error() + ")"
		}
	}
	res.RolledBack = true
	res.Installed = installed
	res.Candidate = m.Core.Version
	res.Message = "update to " + m.Core.Version + " failed health check: " + healthErr.Error() + "; rolled back to " + installed + rbErr
	return res, nil
}

// healthCheck waits until the core process is up AND reports a status frame
// over IPC (the same readiness signal upstream's own installer requires).
func (app *App) healthCheck() error {
	if err := app.sup.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	deadline := time.Now().Add(healthTimeout)
	for time.Now().Before(deadline) {
		st, _ := app.sup.ipc.snapshot()
		if st != nil {
			return nil
		}
		snap := app.sup.Snapshot()
		if !snap.Running {
			return fmt.Errorf("core exited during health check: %s", snap.LastExit)
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("no IPC status within deadline")
}

// Rollback swaps the previous core binary back in and restarts.
func (app *App) Rollback() (*UpdateResult, error) {
	res := &UpdateResult{Action: "rollback"}
	if !fileExists(app.paths.CorePrevBin) {
		return nil, errors.New("no previous core binary kept — nothing to roll back to")
	}
	prev := coreVersionInstalled(app.paths) // best effort label; prev version is unknown
	snap := app.sup.Snapshot()
	if snap.Running {
		if err := app.sup.Stop(); err != nil {
			return nil, err
		}
	}
	cur := app.paths.CoreBin
	_ = os.Remove(cur)
	if err := os.Rename(app.paths.CorePrevBin, cur); err != nil {
		return nil, fmt.Errorf("rollback swap: %w", err)
	}
	if err := app.sup.Start(); err != nil {
		return nil, err
	}
	res.OK = true
	res.Installed = prev + " (previous binary)"
	res.Message = "rolled back to the previous core binary"
	app.log.write("update: manual rollback applied")
	return res, nil
}
