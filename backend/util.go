package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// writeFileAtomic writes data to path via a temp file in the same
// directory, then renames — a crash mid-write never leaves a truncated
// config or key file. Permissions are applied to the final file; on a real
// install the ownership is fixed up by the installer (chown to the service
// user happens before the backend starts).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opffk-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed away; nothing to clean up
	return nil
}

// streamVerify copies src into dst while hashing; when wantSHA is
// non-empty the final digest must match (case-insensitive). Returns the
// hex digest and the number of bytes written.
func streamVerify(dst io.Writer, src io.Reader, limit int64, wantSHA string) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, limit))
	if err != nil {
		return "", n, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && got != strings.ToLower(wantSHA) {
		return got, n, fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA)
	}
	return got, n, nil
}

// copyFile copies src to dst via a temp file + rename (atomic on the
// same filesystem, which both paths share by design).
func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data, mode)
}

// writeFileInPlace overwrites an existing file we own without touching
// its directory (no temp file, no rename). Used for the init script:
// /opt/etc/init.d is root-owned and shared with other services, so the
// backend cannot create files there — but the script file itself belongs
// to the service user. Atomicity is traded for writability here; the
// canonical copy is kept in the share directory for recovery.
func writeFileInPlace(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// fileExists is a small helper used across the update/uninstall paths.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// formatBytes renders a byte count for the status panel.
func formatBytes(n uint64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
