package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// UIConfig is the backend's own configuration and state, stored as JSON at
// <prefix>/etc/opffk/ui.json. It holds the web listen address, the service
// account name, the credential hash (never the credential itself) and the
// OpenFlux settings the user manages from the web UI.
//
// The file is 0600 and owned by the service user; the encryption key lives
// in its own 0600 file next to the OpenFlux config.
type UIConfig struct {
	Schema int `json:"schema"`

	ListenIP string `json:"listen_ip"` // LAN address the web UI binds to
	Port     int    `json:"port"`      // web UI port (default 8791)

	RunAs   string `json:"run_as"`   // service user in <prefix>/etc/passwd ("" = run as-is)
	AllowL3 bool   `json:"allow_l3"` // gate for the experimental l3 exit mode

	AutoStart bool `json:"autostart"` // start the core when the backend starts

	UpdateManifestURL string `json:"update_manifest_url"` // https URL to our manifest.json ("" = updates disabled)

	// Credential = PBKDF2-HMAC-SHA256 of the web password.
	Auth struct {
		Hash       string `json:"hash"` // hex
		Salt       string `json:"salt"` // hex
		Iterations int    `json:"iterations"`
	} `json:"auth"`

	// OpenFlux settings edited from the UI. Validated by ofconf.go before
	// they are written into the core's openflux.conf.
	OpenFlux OpenFluxSettings `json:"openflux"`
}

// OpenFluxSettings is what the UI edits; everything else in the generated
// core config is managed (paths, Role) and not user-editable.
type OpenFluxSettings struct {
	Mode       string             `json:"mode"`  // "l4" (default); "l3" only when allow_l3
	Codec      string             `json:"codec"` // batched | legacy
	Transports []TransportSetting `json:"transports"`
	Debug      int                `json:"debug"` // 0..2 (3 = hexdumps, rejected)
	// SessionContext overrides the KDF context; "" lets the core derive it.
	SessionContext string `json:"session_context,omitempty"`
}

type TransportSetting struct {
	Type     string `json:"type"`     // direct | yandex | vyandex | mailru | cupsonline | boards | avito | telemost | oneme
	Priority int    `json:"priority"` // 1..100, higher = tried first
	URL      string `json:"url,omitempty"`
	// direct only:
	Listen string `json:"listen,omitempty"` // exit: listen address "0.0.0.0:port"
	Dial   string `json:"dial,omitempty"`   // client: exit address (informational in links)
	// oneme only:
	Token string `json:"token,omitempty"`
	UID   string `json:"uid,omitempty"`
}

func DefaultUIConfig() *UIConfig {
	c := &UIConfig{
		Schema:    1,
		ListenIP:  "0.0.0.0",
		Port:      8791,
		RunAs:     "openflux",
		AllowL3:   false,
		AutoStart: true,
		OpenFlux: OpenFluxSettings{
			Mode:  "l4",
			Codec: "batched",
			Transports: []TransportSetting{
				{Type: "direct", Priority: 50, Listen: "0.0.0.0:7891"},
			},
			Debug: 2,
		},
	}
	c.Auth.Iterations = pbkdf2DefaultIterations
	return c
}

// LoadUIConfig reads ui.json. Returns (nil, os.ErrNotExist) when absent.
func LoadUIConfig(p *Paths) (*UIConfig, error) {
	data, err := os.ReadFile(p.UIConfigFile())
	if err != nil {
		return nil, err
	}
	c := &UIConfig{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p.UIConfigFile(), err)
	}
	if c.Schema != 1 {
		return nil, fmt.Errorf("%s: unsupported schema %d", p.UIConfigFile(), c.Schema)
	}
	return c, nil
}

// Save writes ui.json with 0600 permissions.
func (c *UIConfig) Save(p *Paths) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.UIConfigFile(), data, 0o600)
}

// DetectListenIP picks a sensible default LAN bind address: the first
// non-loopback IPv4 address of the host. On a Keenetic that is the LAN
// address; binding to it keeps the UI unreachable from the WAN without
// touching the router firewall.
func DetectListenIP() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		// Prefer RFC1918 ranges; skip loopback and link-local.
		var fallback string
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
				continue
			}
			ip := ipnet.IP.To4()
			if ip.IsLinkLocalUnicast() {
				continue
			}
			if ip[0] == 10 || (ip[0] == 192 && ip[1] == 168) || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) {
				return ip.String()
			}
			if fallback == "" {
				fallback = ip.String()
			}
		}
		if fallback != "" {
			return fallback
		}
	}
	return "127.0.0.1"
}

// firstRun initializes the credential and one-time password file.
// The generated password is written to <prefix>/etc/opffk/initial-password
// (0600) for the installer to display once, and the file is removed after
// the first successful login (or by the installer, whichever comes first).
func firstRun(p *Paths, c *UIConfig) (initialPassword string, err error) {
	pw, err := generatePassword(20)
	if err != nil {
		return "", fmt.Errorf("generate initial password: %w", err)
	}
	salt, hash, err := pbkdf2Hash(pw, c.Auth.Iterations)
	if err != nil {
		return "", err
	}
	c.Auth.Salt = hexEnc(salt)
	c.Auth.Hash = hexEnc(hash)
	if err := c.Save(p); err != nil {
		return "", err
	}
	if err := os.WriteFile(p.InitialPassFile(), []byte(pw+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write one-time password file: %w", err)
	}
	// State dirs must be writable by the service user from now on.
	_ = filepath.Clean(p.StateDir)
	return pw, nil
}

// consumeInitialPassword returns and deletes the one-time password file,
// if it is still there (used by the installer capture path and by the
// backend after the first login).
func consumeInitialPassword(p *Paths) (string, bool) {
	data, err := os.ReadFile(p.InitialPassFile())
	if err != nil {
		return "", false
	}
	_ = os.Remove(p.InitialPassFile())
	return string(data), true
}
