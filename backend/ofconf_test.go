package main

import (
	"os"

	"strings"
	"testing"
)

// miniParseConf mirrors upstream conf.go's parseConf closely enough for
// output-format tests: sections, key=value, comment stripping at # and ;.
func miniParseConf(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{"": {}}
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.Trim(line, "[]")
			if _, ok := out[current]; !ok {
				out[current] = map[string]string{}
			}
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 {
			t.Fatalf("conf line without =: %q", line)
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		if j := strings.IndexAny(v, "#;"); j >= 0 {
			v = strings.TrimSpace(v[:j])
		}
		out[current][k] = v
	}
	return out
}

func validBaseSettings() OpenFluxSettings {
	return OpenFluxSettings{
		Mode:  "l4",
		Codec: "batched",
		Transports: []TransportSetting{
			{Type: "direct", Priority: 50, Listen: "0.0.0.0:7891"},
		},
		Debug: 2,
	}
}

func TestValidateSettingsOK(t *testing.T) {
	s := validBaseSettings()
	if errs := ValidateSettings(&s, false); len(errs) != 0 {
		t.Fatalf("expected valid, got: %v", errs)
	}
}

func TestValidateSettingsRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*OpenFluxSettings)
		contain string
	}{
		{"l3 gated", func(s *OpenFluxSettings) { s.Mode = "l3" }, "l3"},
		{"bad mode", func(s *OpenFluxSettings) { s.Mode = "l5" }, "unknown mode"},
		{"bad codec", func(s *OpenFluxSettings) { s.Codec = "zstd" }, "codec"},
		{"no transports", func(s *OpenFluxSettings) { s.Transports = nil }, "at least one"},
		{"bad type", func(s *OpenFluxSettings) {
			s.Transports[0].Type = "facebook"
		}, "not supported"},
		{"dup type", func(s *OpenFluxSettings) {
			s.Transports = append(s.Transports, TransportSetting{Type: "direct", Priority: 40, Listen: "0.0.0.0:1"})
		}, "duplicated"},
		{"priority high", func(s *OpenFluxSettings) { s.Transports[0].Priority = 101 }, "priority"},
		{"priority zero", func(s *OpenFluxSettings) { s.Transports[0].Priority = 0 }, "priority"},
		{"direct no listen", func(s *OpenFluxSettings) { s.Transports[0].Listen = "" }, "listen"},
		{"listen bad port", func(s *OpenFluxSettings) { s.Transports[0].Listen = "0.0.0.0:0" }, "listen"},
		{"listen comment injection", func(s *OpenFluxSettings) { s.Transports[0].Listen = "0.0.0.0:7891 # evil" }, "invalid characters"},
		{"url wrong scheme", func(s *OpenFluxSettings) {
			s.Transports[0] = TransportSetting{Type: "vyandex", Priority: 10, URL: "ftp://example.com/x"}
		}, "scheme"},
		{"url no host", func(s *OpenFluxSettings) {
			s.Transports[0] = TransportSetting{Type: "vyandex", Priority: 10, URL: "https://"}
		}, "no host"},
		{"url comment injection", func(s *OpenFluxSettings) {
			s.Transports[0] = TransportSetting{Type: "vyandex", Priority: 10, URL: "https://e.com/a;evil=1"}
		}, "invalid characters"},
		{"oneme no token", func(s *OpenFluxSettings) {
			s.Transports[0] = TransportSetting{Type: "oneme", Priority: 10}
		}, "token"},
		{"debug too high", func(s *OpenFluxSettings) { s.Debug = 9 }, "debug"},
		{"context injection", func(s *OpenFluxSettings) { s.SessionContext = "a\nb" }, "session context"},
	}
	for _, c := range cases {
		s := validBaseSettings()
		c.mutate(&s)
		errs := ValidateSettings(&s, false)
		found := false
		for _, e := range errs {
			if strings.Contains(e, c.contain) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected an error containing %q, got %v", c.name, c.contain, errs)
		}
	}
}

func TestValidateSettingsL3Allowed(t *testing.T) {
	s := validBaseSettings()
	s.Mode = "l3"
	if errs := ValidateSettings(&s, true); len(errs) != 0 {
		t.Fatalf("l3 should validate when allowL3=true, got %v", errs)
	}
}

func TestWriteCoreConfigUpstreamShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPFFK_ROOT", dir)
	p := NewPaths()
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	s := OpenFluxSettings{
		Mode: "l4", Codec: "legacy", Debug: 1, SessionContext: "https://docs.example/x",
		Transports: []TransportSetting{
			{Type: "vyandex", Priority: 100, URL: "https://disk.yandex.ru/i/doc"},
			{Type: "direct", Priority: 50, Listen: "0.0.0.0:7891"},
		},
	}
	if errs := ValidateSettings(&s, false); len(errs) != 0 {
		t.Fatalf("settings invalid: %v", errs)
	}
	if err := WriteCoreConfig(p, &s); err != nil {
		t.Fatal(err)
	}

	conf := miniParseConf(t, p.ConfFile())
	iface := conf[""]
	if iface["Role"] != "exit" {
		t.Errorf("Role = %q, want exit", iface["Role"])
	}
	if iface["Mode"] != "l4" {
		t.Errorf("Mode = %q, want l4", iface["Mode"])
	}
	if iface["Codec"] != "legacy" {
		t.Errorf("Codec = %q, want legacy", iface["Codec"])
	}
	if iface["EncryptionKeyFile"] != p.KeyFile() {
		t.Errorf("EncryptionKeyFile = %q, want %q", iface["EncryptionKeyFile"], p.KeyFile())
	}
	if iface["CookieStore"] != p.CookieStore() {
		t.Errorf("CookieStore = %q", iface["CookieStore"])
	}
	if iface["URL"] != "https://docs.example/x" {
		t.Errorf("URL = %q", iface["URL"])
	}
	vy := conf["Transport vyandex"]
	if vy == nil || vy["Type"] != "vyandex" || vy["Priority"] != "100" || vy["URL"] != "https://disk.yandex.ru/i/doc" {
		t.Errorf("[Transport vyandex] wrong: %v", vy)
	}
	di := conf["Transport direct"]
	if di == nil || di["Listen"] != "0.0.0.0:7891" {
		t.Errorf("[Transport direct] wrong: %v", di)
	}

	// Key file must exist and be 64 hex chars.
	key, err := os.ReadFile(p.KeyFile())
	if err != nil {
		t.Fatal(err)
	}
	if !validate_key_hex(strings.TrimSpace(string(key))) {
		t.Errorf("generated key malformed: %q", strings.TrimSpace(string(key)))
	}

	// Second call must keep the existing key.
	before := string(key)
	if err := WriteCoreConfig(p, &s); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p.KeyFile())
	if string(after) != before {
		t.Error("key was regenerated on config rewrite")
	}
}

func TestValidateKeyHex(t *testing.T) {
	if validate_key_hex("a") || validate_key_hex("") || validate_key_hex(strings.Repeat("g", 64)) ||
		validate_key_hex(strings.Repeat("a", 63)) {
		t.Error("malformed keys accepted")
	}
	if !validate_key_hex(strings.Repeat("aF09", 16)) {
		t.Error("valid 64-hex rejected")
	}
}
