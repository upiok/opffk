package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// The privilege model on Keenetic/Entware:
//
// Keenetic regenerates its firmware /etc on boot, so a service account in
// /etc/passwd does not survive a reboot. Entware keeps its own persistent
// <prefix>/etc/passwd, so opffk defines its service user there and the
// backend drops root → that user after it has bound its listener and
// written its first-run files.
//
// The core child inherits the dropped privileges; nothing in this backend
// escalates afterwards. When the backend is started by a non-root user
// (development, test sandboxes), the drop is skipped and that is logged.

// lookupUser scans an Entware passwd file for a user name and returns uid/gid.
func lookupUser(passwdPath, name string) (uid, gid int, err error) {
	data, err := os.ReadFile(passwdPath)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", passwdPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 3 || fields[0] != name {
			continue
		}
		uid, err1 := strconv.Atoi(fields[2])
		gid := 0
		if len(fields) > 3 {
			gid, _ = strconv.Atoi(fields[3])
		}
		if err1 != nil {
			return 0, 0, fmt.Errorf("%s: uid for %s is not numeric", passwdPath, name)
		}
		return uid, gid, nil
	}
	return 0, 0, fmt.Errorf("user %q not found in %s (re-run the installer to create it)", name, passwdPath)
}

// ensureServiceUser appends the service user to the Entware passwd/group
// files when it is missing. Runs as root during installation only.
func ensureServiceUser(p *Paths, name string) (created bool, uid, gid int, err error) {
	if _, _, lerr := lookupUser(p.PasswdFile, name); lerr == nil {
		uid, gid, err = lookupUser(p.PasswdFile, name)
		return false, uid, gid, err
	}
	uid, err = nextFreeUID(p.PasswdFile, 65000)
	if err != nil {
		return false, 0, 0, err
	}
	gid, err = nextFreeGID(p.GroupFile, 65000)
	if err != nil {
		return false, 0, 0, err
	}
	f, err := os.OpenFile(p.PasswdFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, 0, 0, fmt.Errorf("append %s: %w", p.PasswdFile, err)
	}
	defer f.Close()
	if _, err = fmt.Fprintf(f, "%s:x:%d:%d:opffk service:/nonexistent:/bin/false\n", name, uid, gid); err != nil {
		return false, 0, 0, err
	}
	g, err := os.OpenFile(p.GroupFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		defer g.Close()
		_, _ = fmt.Fprintf(g, "%s:x:%d:%s\n", name, gid, name)
	}
	return true, uid, gid, nil
}

func nextFreeUID(path string, base int) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	used := map[int]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 2 {
			if n, err := strconv.Atoi(fields[2]); err == nil {
				used[n] = true
			}
		}
	}
	for id := base; id < base+500; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free uid at or above %d in %s", base, path)
}

func nextFreeGID(path string, base int) (int, error) { return nextFreeUID(path, base) }

// dropPrivileges switches to the service user (persistent across the whole
// process, including the core child). Root-owned files created during
// first-run must be chowned BEFORE this is called.
func dropPrivileges(p *Paths, cfg *UIConfig) error {
	if TestMode() {
		return nil
	}
	if cfg.RunAs == "" {
		return nil
	}
	if os.Geteuid() != 0 {
		return nil // started unprivileged (dev host); nothing to drop
	}
	uid, gid, err := lookupUser(p.PasswdFile, cfg.RunAs)
	if err != nil {
		return err
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups(%d): %w", gid, err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid(%d): %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid(%d): %w", uid, err)
	}
	return nil
}

// chownTree recursively chowns path to uid:gid (best effort, used to hand
// the freshly created config/state/log files to the service user).
func chownTree(path string, uid, gid int) error {
	return syscall.Chown(path, uid, gid)
}
