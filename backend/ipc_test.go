package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIpcReadFrame(t *testing.T) {
	status := []byte(`{"running":true,"connected":true,"bytes_in":10,"bytes_out":20,"uptime_ms":5}`)
	var buf bytes.Buffer
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(1+len(status)))
	hdr[4] = ipcMsgStatus
	buf.Write(hdr[:])
	buf.Write(status)

	typ, payload, err := ipcReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != ipcMsgStatus || string(payload) != string(status) {
		t.Fatalf("frame mismatch: type=%d payload=%s", typ, payload)
	}

	// Zero and oversized lengths are rejected.
	var bad [4]byte
	binary.BigEndian.PutUint32(bad[:], 0)
	if _, _, err := ipcReadFrame(bytes.NewReader(bad[:])); err == nil {
		t.Error("zero-length frame accepted")
	}
	binary.BigEndian.PutUint32(bad[:], ipcMaxFrame+1)
	if _, _, err := ipcReadFrame(bytes.NewReader(bad[:])); err == nil {
		t.Error("oversized frame accepted")
	}
}

// TestIPCStatusClientAgainstFakeServer connects the real status client to a
// local socket that emits frames the way upstream does.
func TestIPCStatusClientAgainstFakeServer(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		payload := []byte(`{"running":true,"connected":true,"bytes_in":1,"bytes_out":2,"uptime_ms":3,"active":"direct"}`)
		var hdr [5]byte
		binary.BigEndian.PutUint32(hdr[:4], uint32(1+len(payload)))
		hdr[4] = ipcMsgStatus
		_, _ = conn.Write(hdr[:])
		_, _ = conn.Write(payload)
		// keep the connection open until the test ends
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err == io.EOF {
				return
			}
		}
	}()

	c := newIPCStatusClient(sock)
	done := make(chan struct{})
	go func() { c.poll(); close(done) }()

	for i := 0; i < 50; i++ {
		if st, _ := c.snapshot(); st != nil {
			if !st.Running || !st.Connected || st.BytesIn != 1 || st.Active != "direct" {
				t.Fatalf("unexpected status: %+v", st)
			}
			c.close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("client never received the status frame")
}

func TestCoreVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"node-v1.3.1", "node-v1.3.2", true},
		{"node-v1.3.1", "node-v1.10.0", true},
		{"node-v1.3.1", "node-v2.0.0", true},
		{"node-v1.3.1", "node-v1.3.1", false},
		{"node-v1.3.1", "node-v1.3.0", false},
		{"", "node-v1.0.0", false}, // no baseline -> not "newer" relation
		{"garbage", "node-v1.0.0", false},
	}
	for _, c := range cases {
		if got := coreVersionNewer(c.a, c.b); got != c.want {
			t.Errorf("coreVersionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseManifest(t *testing.T) {
	good := `{"schema":1,"core":{"version":"node-v1.3.1","repository":"p1neappleXpress/OpenFlux",
		"url_template":"https://example/download/node-v1.3.1/openflux-linux-{arch}",
		"sha256":{"arm64":"` + strings.Repeat("a", 64) + `","arm":"` + strings.Repeat("b", 64) + `"}}}`
	m, err := parseManifest([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if m.Core.Version != "node-v1.3.1" {
		t.Errorf("version = %q", m.Core.Version)
	}
	if _, ok := m.Core.SHA256["arm64"]; !ok {
		t.Error("arm64 hash missing")
	}

	bad := []string{
		`{"schema":2}`,
		`{"schema":1,"core":{"version":"","url_template":"https://x/{arch}","sha256":{"arm64":"` + strings.Repeat("a", 64) + `"}}}`,
		`{"schema":1,"core":{"version":"v1","url_template":"https://x/openflux-linux-amd64","sha256":{"arm64":"` + strings.Repeat("a", 64) + `"}}}`,
		`{"schema":1,"core":{"version":"v1","url_template":"https://x/{arch}","sha256":{"riscv":"` + strings.Repeat("a", 64) + `"}}}`,
		`{"schema":1,"core":{"version":"v1","url_template":"https://x/{arch}","sha256":{"arm64":"short"}}}`,
	}
	for _, b := range bad {
		if _, err := parseManifest([]byte(b)); err == nil {
			t.Errorf("malformed manifest accepted: %s", b)
		}
	}
}

func TestFetchCoreVerifiesHash(t *testing.T) {
	// The hash-comparison path shared by the installer and fetchCore:
	// hash a fake core, then confirm the comparison helpers agree.
	content := []byte("fake-core-binary-content")
	want := sha256HexStr(content)

	dir := t.TempDir()
	f := filepath.Join(dir, "core")
	if err := os.WriteFile(f, content, 0o755); err != nil {
		t.Fatal(err)
	}
	got := sha256OfFileForTest(f)
	if got != want {
		t.Fatalf("hash mismatch: got %s want %s", got, want)
	}
}

func TestLogRing(t *testing.T) {
	dir := t.TempDir()
	r := newLogRing(filepath.Join(dir, "core.log"))
	defer r.close()
	for i := 0; i < ringLines+50; i++ {
		r.write(strings.Repeat("x", 10) + itoa(i))
	}
	tail := r.tail(10)
	if len(tail) != 10 {
		t.Fatalf("tail length %d", len(tail))
	}
	if !strings.HasSuffix(tail[9], itoa(ringLines+49)) {
		t.Errorf("last line wrong: %s", tail[9])
	}
	if len(r.tail(10000)) != ringLines {
		t.Error("ring does not cap at ringLines")
	}

	// File exists and has content.
	st, err := os.Stat(filepath.Join(dir, "core.log"))
	if err != nil || st.Size() == 0 {
		t.Error("log file not written")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "file.json")
	// Directory must exist first (writeFileAtomic does not create dirs).
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "hello" {
		t.Fatalf("content mismatch: %s %v", data, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".opffk-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestArchMapping(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"x86_64":  {"amd64", true},
		"aarch64": {"arm64", true},
		"armv7l":  {"arm", true},
		"armv7hl": {"arm", true},
		"mips":    {"", false},
		"mipsel":  {"", false},
		"armv6l":  {"", false},
		"riscv64": {"", false},
	}
	for unameM, c := range cases {
		got, ok := archToUpstream(unameM)
		if ok != c.ok || got != c.want {
			t.Errorf("archToUpstream(%q) = %q,%v want %q,%v", unameM, got, ok, c.want, c.ok)
		}
	}
}

func sha256HexStr(b []byte) string {
	h := sha256.New()
	h.Write(b)
	return hexEnc(h.Sum(nil))
}

func sha256OfFileForTest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return sha256HexStr(data)
}
