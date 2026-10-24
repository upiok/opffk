package main

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// This file mirrors the upstream OpenFlux .conf format (conf.go, INI-like,
// values terminated by end of line, '#' or ';') and the validation rules of
// upstream main.go for role=exit. It must stay compatible with the pinned
// upstream core (manifest.json → core.version).

// Transport types the UI may configure. The script transport is
// deliberately excluded (v1): managing signed-JS trust from a web UI is a
// separate review problem; it can still be configured by hand in the
// openflux.conf file.
var allowedTransportTypes = map[string]bool{
	"direct":     true,
	"yandex":     true,
	"vyandex":    true,
	"mailru":     true,
	"cupsonline": true,
	"boards":     true,
	"avito":      true,
	"telemost":   true,
	"oneme":      true,
}

// transportNeedsURL: types whose configuration is a document/weblink URL.
var transportNeedsURL = map[string]bool{
	"yandex": true, "vyandex": true, "mailru": true, "cupsonline": true,
	"boards": true, "avito": true, "telemost": true,
}

// confSafe reports whether s can be written into an upstream .conf value:
// no comment terminators, no newlines, no control characters.
func confSafe(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 2048 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return !strings.ContainsAny(s, "#;\r\n")
}

func validateHostPort(s string, requireHost bool) error {
	if !confSafe(s) {
		return fmt.Errorf("address contains invalid characters")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("want host:port")
	}
	if host == "" {
		if requireHost {
			return fmt.Errorf("host part is empty")
		}
	} else if ip := net.ParseIP(host); ip == nil {
		if !validHostname(host) {
			return fmt.Errorf("host %q is not an IP or hostname", host)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q out of range", port)
	}
	return nil
}

func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}

// validateEgressProxy checks an --egress-proxy URL against what the
// fork core accepts: socks5:// or http://, host:port, optional
// user:pass@. The value goes into the .conf file, so it must also be
// conf-safe (no '#', ';', newlines — such characters cannot appear in a
// proxy password set through the UI).
func validateEgressProxy(s string) error {
	if !confSafe(s) {
		return fmt.Errorf("contains invalid characters (a proxy password cannot contain # or ; here)")
	}
	if len(s) > 512 {
		return fmt.Errorf("too long")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URL")
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http":
	default:
		return fmt.Errorf("scheme %q: want socks5 or http", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return fmt.Errorf("want host:port")
	}
	if n, err := strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q out of range", u.Port())
	}
	if u.Hostname() != "localhost" && net.ParseIP(u.Hostname()) == nil && !validHostname(u.Hostname()) {
		return fmt.Errorf("host %q is not an IP or hostname", u.Hostname())
	}
	return nil
}

// validate_key_hex accepts exactly 64 hex characters — the shape upstream's
// node-install.sh writes for channel keys (32 random bytes).
func validate_key_hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !ok {
			return false
		}
	}
	return true
}

func validateHTTPURL(s string) error {
	if !confSafe(s) {
		return fmt.Errorf("URL contains invalid characters")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("URL has no host")
	}
	return nil
}

// ValidateSettings checks user-editable OpenFlux settings against upstream
// rules and returns all problems found (empty list = valid).
func ValidateSettings(s *OpenFluxSettings, allowL3 bool) []string {
	var errs []string
	add := func(format string, args ...interface{}) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	switch s.Mode {
	case "l4":
	case "l3":
		if !allowL3 {
			add("mode l3 is not enabled on this installation (requires root privileges, iptables and an explicit allow_l3=true in ui.json; l4 is the supported mode)")
		}
	case "":
		add("mode must be l4 (or l3 when explicitly enabled)")
	default:
		add("unknown mode %q (upstream supports l3|l4)", s.Mode)
	}

	switch s.Codec {
	case "batched", "legacy":
	default:
		add("codec must be batched or legacy")
	}

	if s.Debug < 0 || s.Debug > 3 {
		add("debug must be 0..3")
	}

	if s.SessionContext != "" && !confSafe(s.SessionContext) {
		add("session context contains invalid characters")
	}

	if s.EgressProxy != "" {
		if err := validateEgressProxy(s.EgressProxy); err != nil {
			add("egress proxy: %v", err)
		}
		if s.Mode == "l3" {
			add("egress proxy cannot work in l3 mode (raw-socket SNAT has no proxy path); use l4")
		}
	}

	if len(s.Transports) == 0 {
		add("at least one transport is required")
	}
	seen := map[string]bool{}
	for i, t := range s.Transports {
		label := fmt.Sprintf("transport #%d", i+1)
		if !allowedTransportTypes[t.Type] {
			add("%s: type %q is not supported (allowed: direct, yandex, vyandex, mailru, cupsonline, boards, avito, telemost, oneme)", label, t.Type)
			continue
		}
		label = "transport " + t.Type
		if seen[t.Type] {
			add("%s: duplicated (each transport type may appear once)", label)
		}
		seen[t.Type] = true
		if t.Priority < 1 || t.Priority > 100 {
			add("%s: priority must be 1..100", label)
		}
		switch t.Type {
		case "direct":
			if t.Listen == "" {
				add("%s: listen address is required on the exit node (e.g. 0.0.0.0:7891)", label)
			} else if err := validateHostPort(t.Listen, false); err != nil {
				add("%s: listen: %v", label, err)
			}
			if t.Dial != "" {
				if err := validateHostPort(t.Dial, true); err != nil {
					add("%s: dial: %v", label, err)
				}
			}
		case "oneme":
			if !confSafe(t.Token) || len(t.Token) < 8 {
				add("%s: token missing or too short", label)
			}
			if !confSafe(t.UID) || len(t.UID) < 1 {
				add("%s: uid missing", label)
			}
		default:
			if err := validateHTTPURL(t.URL); err != nil {
				add("%s: URL: %v", label, err)
			}
		}
	}
	return errs
}

// GenerateKey returns a fresh 32-byte encryption key, hex encoded — the
// same shape upstream's node-install.sh writes (64 hex characters).
func GenerateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hexEnc(b), nil
}

// WriteCoreConfig renders and atomically writes the upstream .conf file for
// the given validated settings, and ensures the encryption key file exists.
// The caller is responsible for validation (ValidateSettings) and for
// restarting the core.
func WriteCoreConfig(p *Paths, s *OpenFluxSettings) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# OpenFlux exit node — managed by opffk (web UI)\n")
	fmt.Fprintf(&b, "# Format: upstream OpenFlux .conf (see upstream docs/cli-reference.md, --config).\n")
	fmt.Fprintf(&b, "# Hand edits are overwritten when settings are applied from the web UI.\n")
	fmt.Fprintf(&b, "Role = exit\n")
	fmt.Fprintf(&b, "Mode = %s\n", s.Mode)
	fmt.Fprintf(&b, "EncryptionKeyFile = %s\n", p.KeyFile())
	fmt.Fprintf(&b, "CookieStore = %s\n", p.CookieStore())
	if s.SessionContext != "" {
		fmt.Fprintf(&b, "URL = %s\n", s.SessionContext)
	}
	if s.EgressProxy != "" {
		fmt.Fprintf(&b, "EgressProxy = %s\n", s.EgressProxy)
	}
	fmt.Fprintf(&b, "Debug = %d\n", s.Debug)
	switch s.Codec {
	case "legacy":
		fmt.Fprintf(&b, "Codec = legacy\n")
	default:
		fmt.Fprintf(&b, "Codec = batched\n")
	}
	for _, t := range s.Transports {
		fmt.Fprintf(&b, "\n[Transport %s]\n", t.Type)
		fmt.Fprintf(&b, "Type = %s\n", t.Type)
		fmt.Fprintf(&b, "Priority = %d\n", t.Priority)
		if t.URL != "" {
			fmt.Fprintf(&b, "URL = %s\n", t.URL)
		}
		switch t.Type {
		case "direct":
			if t.Listen != "" {
				fmt.Fprintf(&b, "Listen = %s\n", t.Listen)
			}
			if t.Dial != "" {
				fmt.Fprintf(&b, "Dial = %s\n", t.Dial)
			}
		case "oneme":
			if t.Token != "" {
				fmt.Fprintf(&b, "Token = %s\n", t.Token)
			}
			if t.UID != "" {
				fmt.Fprintf(&b, "UID = %s\n", t.UID)
			}
		}
	}

	if err := writeFileAtomic(p.ConfFile(), []byte(b.String()), 0o640); err != nil {
		return err
	}
	// Ensure a key exists; keep the existing one otherwise.
	if _, err := os.Stat(p.KeyFile()); os.IsNotExist(err) {
		key, err := GenerateKey()
		if err != nil {
			return err
		}
		if err := writeFileAtomic(p.KeyFile(), []byte(key+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}
