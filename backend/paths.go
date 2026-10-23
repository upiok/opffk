package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Paths holds every filesystem location the backend uses. All of them hang
// off one prefix: "/opt" on a real Entware installation, a test directory
// when OPFFK_ROOT is set (used by the automated tests; privilege drop and
// process supervision are also relaxed there).
type Paths struct {
	Root        string // e.g. /opt
	BinDir      string // /opt/bin            — opffk-web, openflux
	EtcDir      string // /opt/etc/opffk      — ui.json, openflux.conf, encryption-key
	StateDir    string // /opt/var/lib/opffk  — cookie store, backups, uninstall flag
	LogDir      string // /opt/var/log/opffk  — core.log, web.log
	RunDir      string // /opt/var/run        — pid files, IPC socket
	ShareDir    string // /opt/share/opffk    — VERSION, manifest.json
	InitScript  string // /opt/etc/init.d/S99opffk
	PasswdFile  string // /opt/etc/passwd     — service user lives here (persistent on Keenetic)
	GroupFile   string // /opt/etc/group
	CoreBin     string // /opt/bin/openflux
	CorePrevBin string // /opt/bin/openflux.previous (kept for rollback)
	WebBin      string // /opt/bin/opffk-web
}

func defaultRoot() string {
	root := os.Getenv("OPFFK_ROOT")
	if root == "" {
		root = "/opt"
	}
	return strings.TrimSuffix(root, "/")
}

func NewPaths() *Paths {
	r := defaultRoot()
	p := &Paths{
		Root:        r,
		BinDir:      r + "/bin",
		EtcDir:      r + "/etc/opffk",
		StateDir:    r + "/var/lib/opffk",
		LogDir:      r + "/var/log/opffk",
		RunDir:      r + "/var/run",
		ShareDir:    r + "/share/opffk",
		InitScript:  r + "/etc/init.d/S99opffk",
		PasswdFile:  r + "/etc/passwd",
		GroupFile:   r + "/etc/group",
		CoreBin:     r + "/bin/openflux",
		CorePrevBin: r + "/bin/openflux.previous",
		WebBin:      r + "/bin/opffk-web",
	}
	return p
}

// TestMode reports whether the backend runs against an OPFFK_ROOT sandbox
// instead of a real Entware prefix. Several root-only behaviors (privilege
// drop, init script management) are skipped in this mode so the automated
// tests can exercise everything else on a development host.
func TestMode() bool { return defaultRoot() != "/opt" }

// ensureDirs creates the directory layout with restrictive permissions.
// Called before the privilege drop, when running as root on a real install.
func (p *Paths) EnsureDirs() error {
	for _, d := range []string{p.BinDir, p.EtcDir, p.StateDir, p.LogDir, p.RunDir, p.ShareDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// coreVersionFile holds the version of the installed openflux core.
func (p *Paths) CoreVersionFile() string { return filepath.Join(p.ShareDir, "VERSION") }

func (p *Paths) ManifestFile() string    { return filepath.Join(p.ShareDir, "manifest.json") }
func (p *Paths) UIConfigFile() string    { return filepath.Join(p.EtcDir, "ui.json") }
func (p *Paths) ConfFile() string        { return filepath.Join(p.EtcDir, "openflux.conf") }
func (p *Paths) KeyFile() string         { return filepath.Join(p.EtcDir, "encryption-key") }
func (p *Paths) InitialPassFile() string { return filepath.Join(p.EtcDir, "initial-password") }
func (p *Paths) CookieStore() string     { return filepath.Join(p.StateDir, "cookies.json") }
func (p *Paths) CoreLogFile() string     { return filepath.Join(p.LogDir, "core.log") }
func (p *Paths) WebLogFile() string      { return filepath.Join(p.LogDir, "web.log") }
func (p *Paths) IPCSocket() string       { return filepath.Join(p.RunDir, "opffk-core.sock") }
func (p *Paths) PIDFile() string         { return filepath.Join(p.RunDir, "opffk-web.pid") }
func (p *Paths) CorePIDFile() string     { return filepath.Join(p.RunDir, "opffk-core.pid") }

// archToUpstream maps uname -m values to the upstream release binary suffix.
// Supported upstream channels publish amd64, arm64 and arm (GOARM=7) only.
func archToUpstream(unameM string) (string, bool) {
	switch unameM {
	case "x86_64", "amd64":
		return "amd64", true
	case "aarch64", "arm64":
		return "arm64", true
	case "armv7l", "armv7hl", "armv7ve":
		return "arm", true
	default:
		// armv5/armv6 (armv6l), mips, mipsel, riscv, ... are not published
		// upstream; a source build may exist but is not supported by the
		// installer (see docs/COMPATIBILITY.md).
		return "", false
	}
}

func hostArch() string { return runtime.GOARCH }

// hostUnameM reports the uname -m value the running binary corresponds to.
// The backend is a static binary built for exactly one GOARCH, so this is
// accurate without shelling out to uname.
func hostUnameM() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "arm":
		return "armv7l"
	default:
		return runtime.GOARCH
	}
}
