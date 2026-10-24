package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Full self-update: instead of touching only the pinned OpenFlux core,
// the backend can update the whole opffk deployment — its own binary,
// the core binary, the init script, the manifest and the version marker
// — from a GitHub release of the opffk repository.
//
// Trust chain: the release tarball is downloaded over HTTPS together
// with its .sha256 file and verified (the same trust level as the
// one-command installer); the core binary inside is additionally
// cross-checked against the manifest.json bundled in that tarball.
//
// Applying the update: files are staged, the new backend binary is
// pre-flighted (run with -version), the core is stopped, every file is
// swapped with atomic renames, the previous backend is kept at
// <prefix>/bin/opffk-web.previous, and the running process finally
// syscall.Exec()s the new binary with the same arguments — the PID
// survives, so the init script's pidfile stays valid, the new backend
// rebinds the listener and autostarts the core. The web session is lost
// (in-memory), so the user logs in again.

const (
	maxSelfTarballBytes = 96 << 20
	selfUpdateTimeout   = 5 * time.Minute
)

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// opffkReleaseInfo is what the check/apply endpoints report.
type opffkReleaseInfo struct {
	OK         bool   `json:"ok"`
	Installed  string `json:"installed"` // opffk version marker on disk
	Latest     string `json:"latest"`    // latest release tag
	UpToDate   bool   `json:"up_to_date"`
	TarballURL string `json:"tarball_url,omitempty"`
	ShaURL     string `json:"sha_url,omitempty"`
	Message    string `json:"message,omitempty"`
}

// fetchLatestOpffkRelease queries the GitHub releases API for the newest
// opffk release and picks the tarball + checksum for this architecture.
func fetchLatestOpffkRelease(repo, apiBase string) (*opffkReleaseInfo, string, string, error) {
	if !strings.HasPrefix(apiBase, "https://") && !TestMode() {
		return nil, "", "", errors.New("update API base must be https")
	}
	arch, ok := archToUpstream(hostUnameM())
	if !ok {
		return nil, "", "", fmt.Errorf("architecture %s is not supported", hostUnameM())
	}
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", apiBase+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("releases/latest: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return nil, "", "", err
	}
	rel := &ghRelease{}
	if err := json.Unmarshal(body, rel); err != nil {
		return nil, "", "", fmt.Errorf("parse release: %w", err)
	}
	if rel.TagName == "" {
		return nil, "", "", errors.New("release has no tag")
	}
	want := fmt.Sprintf("opffk-%s-linux-%s.tar.gz", rel.TagName, arch)
	var tarURL, shaURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			tarURL = a.BrowserDownloadURL
		case want + ".sha256":
			shaURL = a.BrowserDownloadURL
		}
	}
	if tarURL == "" || shaURL == "" {
		return nil, "", "", fmt.Errorf("release %s has no %s(.sha256) asset for this architecture", rel.TagName, want)
	}
	return &opffkReleaseInfo{Latest: rel.TagName, TarballURL: tarURL, ShaURL: shaURL}, tarURL, shaURL, nil
}

// parseShaLine takes the content of a "<hex>  <file>" checksum file.
func parseShaLine(data []byte) (string, error) {
	line := strings.TrimSpace(string(data))
	if line == "" {
		return "", errors.New("empty checksum file")
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", errors.New("malformed checksum file")
	}
	return strings.ToLower(fields[0]), nil
}

// opffkVersionInstalled reads the release marker written by the
// installer/updater; "" when unknown.
func opffkVersionInstalled(p *Paths) string {
	data, err := os.ReadFile(filepath.Join(p.ShareDir, "opffk-VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func versionTagNewer(installed, candidate string) bool {
	return coreVersionNewer(installed, candidate) // tolerant of v/node-v prefixes
}

// CheckSelfUpdate reports whether a newer opffk release exists.
func (app *App) CheckSelfUpdate() (*opffkReleaseInfo, error) {
	res := &opffkReleaseInfo{Installed: opffkVersionInstalled(app.paths)}
	repo := app.cfg.UpdateRepo
	if repo == "" {
		repo = "upiok/opffk"
	}
	apiBase := app.cfg.UpdateAPIBase
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	rel, _, _, err := fetchLatestOpffkRelease(repo, apiBase)
	if err != nil {
		return nil, err
	}
	res.Latest = rel.Latest
	res.TarballURL = rel.TarballURL
	res.ShaURL = rel.ShaURL
	if res.Installed == "" {
		res.Message = "installed version unknown; updating is still possible"
		return res, nil
	}
	res.UpToDate = !versionTagNewer(res.Installed, rel.Latest)
	if res.UpToDate {
		res.Message = "up to date"
	} else {
		res.Message = "update available: " + rel.Latest
	}
	return res, nil
}

// ApplySelfUpdate downloads and verifies the release tarball, stages it,
// pre-flights the new backend binary, swaps every file and re-execs.
func (app *App) ApplySelfUpdate() (*opffkReleaseInfo, error) {
	res := &opffkReleaseInfo{Installed: opffkVersionInstalled(app.paths)}
	repo := app.cfg.UpdateRepo
	if repo == "" {
		repo = "upiok/opffk"
	}
	apiBase := app.cfg.UpdateAPIBase
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	rel, tarURL, shaURL, err := fetchLatestOpffkRelease(repo, apiBase)
	if err != nil {
		return nil, err
	}
	res.Latest = rel.Latest
	if res.Installed != "" && !versionTagNewer(res.Installed, rel.Latest) {
		res.UpToDate = true
		res.Message = "nothing to do (installed " + res.Installed + ")"
		return res, nil
	}

	// ---- download + verify ------------------------------------------------
	client := &http.Client{Timeout: selfUpdateTimeout}
	sum, err := httpGetBody(client, shaURL, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("checksum download: %w", err)
	}
	wantSum, err := parseShaLine(sum)
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(app.paths.StateDir, "selfupdate-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	tarPath := filepath.Join(stage, "release.tar.gz")
	if err := httpGetToFile(client, tarURL, tarPath, maxSelfTarballBytes, wantSum); err != nil {
		return nil, fmt.Errorf("tarball download/verify: %w", err)
	}
	if err := exec.Command("tar", "-xzf", tarPath, "-C", stage).Run(); err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	// the tarball roots at opffk-<tag>-linux-<arch>/; find it
	entries, err := os.ReadDir(stage)
	if err != nil {
		return nil, err
	}
	var inner string
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(stage, e.Name(), "install.sh")) {
			inner = filepath.Join(stage, e.Name())
		}
	}
	if inner == "" {
		return nil, errors.New("tarball layout unexpected: no release directory")
	}
	newBackend := filepath.Join(inner, "opffk-web-"+hostArch())
	newCore := filepath.Join(inner, "openflux-linux-"+archNameForManifest(hostArch()))
	newInit := filepath.Join(inner, "S99opffk")
	newManifest := filepath.Join(inner, "manifest.json")
	newVer := filepath.Join(inner, "VERSION")
	for _, f := range []string{newBackend, newCore, newInit, newManifest, newVer} {
		if !fileExists(f) {
			return nil, fmt.Errorf("release tarball lacks %s", filepath.Base(f))
		}
	}
	// cross-check the bundled core against the bundled manifest
	bundled, err := loadManifestFrom(newManifest)
	if err != nil {
		return nil, fmt.Errorf("bundled manifest: %w", err)
	}
	coreSum, ok := bundled.Core.SHA256[archNameForManifest(hostArch())]
	if !ok {
		return nil, errors.New("bundled manifest lacks this architecture")
	}
	gotSum, err := fileSHA256(newCore)
	if err != nil {
		return nil, err
	}
	if gotSum != strings.ToLower(coreSum) {
		return nil, errors.New("bundled core fails the bundled manifest verification (refusing)")
	}

	// ---- pre-flight the new backend ----------------------------------------
	preflight := exec.Command(newBackend, "-version")
	out, err := preflight.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("new backend pre-flight failed: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// ---- apply --------------------------------------------------------------
	// Order matters for recoverability: the init script goes first (the
	// only step that can fail on permissions — a root-owned rc directory
	// shared with other Entware services). If it fails, nothing else has
	// been touched and a retry is clean.
	app.log.write("self-update: applying " + rel.Latest + " (backend, core, init script, manifest)")
	if err := app.sup.Stop(); err != nil {
		return nil, fmt.Errorf("stop core: %w", err)
	}

	// init script: canonical copy in the share dir, then in-place write
	// (writeFileInPlace — the file is service-user-owned; the directory
	// is not ours and must stay root-owned for the other services).
	initData, err := os.ReadFile(newInit)
	if err != nil {
		return nil, err
	}
	shareInit := filepath.Join(app.paths.ShareDir, "S99opffk")
	if err := writeFileAtomic(shareInit, initData, 0o755); err != nil {
		return nil, fmt.Errorf("stage init script copy: %w", err)
	}
	if err := writeFileInPlace(app.paths.InitScript, initData, 0o755); err != nil {
		return nil, fmt.Errorf("install init script: %w (if this persists, run the installer once over SSH: "+
			"curl -fsSL https://raw.githubusercontent.com/upiok/opffk/main/install.sh | OPFFK_REPO=upiok/opffk sh)", err)
	}

	// keep the previous backend for manual recovery
	_ = os.Remove(app.paths.WebBin + ".previous")
	if fileExists(app.paths.WebBin) {
		if err := os.Rename(app.paths.WebBin, app.paths.WebBin+".previous"); err != nil {
			return nil, fmt.Errorf("backup current backend: %w", err)
		}
	}
	if err := copyFile(newBackend, app.paths.WebBin+".new", 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(app.paths.WebBin+".new", app.paths.WebBin); err != nil {
		return nil, fmt.Errorf("install backend: %w", err)
	}
	if err := copyFile(newCore, app.paths.CoreBin+".new", 0o755); err != nil {
		return nil, err
	}
	if fileExists(app.paths.CorePrevBin) {
		_ = os.Remove(app.paths.CorePrevBin)
	}
	if fileExists(app.paths.CoreBin) {
		_ = os.Rename(app.paths.CoreBin, app.paths.CorePrevBin)
	}
	if err := os.Rename(app.paths.CoreBin+".new", app.paths.CoreBin); err != nil {
		return nil, fmt.Errorf("install core: %w", err)
	}
	if err := copyFile(newManifest, app.paths.ManifestFile()+".new", 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(app.paths.ManifestFile()+".new", app.paths.ManifestFile()); err != nil {
		return nil, err
	}
	if err := copyFile(newVer, app.paths.ShareDir+"/opffk-VERSION.new", 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(app.paths.ShareDir+"/opffk-VERSION.new", app.paths.ShareDir+"/opffk-VERSION"); err != nil {
		return nil, err
	}
	coreVer, err := os.ReadFile(newVer)
	if err == nil {
		// the tarball's VERSION is the opffk version; the core version
		// comes from the bundled manifest
		_ = os.WriteFile(app.paths.CoreVersionFile(), []byte(bundled.Core.Version+"\n"), 0o644)
		_ = coreVer
	}

	if TestMode() {
		// sandbox: no exec (that would replace the test process); the
		// caller asserts the swapped files instead.
		app.log.write("self-update: test mode — files swapped, exec skipped")
		res.OK = true
		res.Installed = rel.Latest
		res.Message = "files updated (test mode: backend re-exec skipped)"
		return res, nil
	}

	res.OK = true
	res.Installed = rel.Latest
	res.Message = "updated to " + rel.Latest + "; the backend is restarting — log in again in a few seconds"
	app.log.write("self-update: re-execing the new backend")
	app.sup.ipc.close()
	app.log.close()

	// Same PID, new image: the pidfile stays valid, the init script keeps
	// working, the fresh process rebinds the listener and autostarts the
	// core. The web session dies with the old image — by design.
	exe, err := os.Executable()
	if err != nil {
		exe = app.paths.WebBin
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return nil, fmt.Errorf("re-exec failed (the old backend is gone; start it with %s restart): %w",
			app.paths.InitScript, err)
	}
	return res, nil
}

func archNameForManifest(hostArch string) string {
	// build.sh labels binaries by release arch; manifest keys match
	switch hostArch {
	case "arm":
		return "arm"
	default:
		return hostArch
	}
}

func httpGetBody(c *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func httpGetToFile(c *http.Client, url, dst string, limit int64, wantSHA string) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	h, written, err := streamVerify(f, resp.Body, limit, wantSHA)
	f.Close()
	if err != nil {
		_ = os.Remove(dst)
		return err
	}
	if written > limit {
		_ = os.Remove(dst)
		return errors.New("download exceeds size limit")
	}
	_ = h
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h, _, err := streamVerify(io.Discard, f, 1<<62, "")
	return h, err
}
