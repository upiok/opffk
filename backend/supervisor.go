package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// supervisor owns the OpenFlux core child process: start, graceful stop,
// crash backoff and liveness. The core is always started as
//
//	openflux --config <conf> --ipc-socket <sock>
//
// with the role and mode coming from the config file (upstream node-wizard
// deployments run the same way). The child runs with the backend's
// (already-dropped) privileges — the backend never escalates.
type supervisor struct {
	paths *Paths
	log   *logRing
	ipc   *ipcStatusClient

	mu         sync.Mutex
	cmd        *exec.Cmd
	desired    bool // user wants the core running
	manualStop bool // last stop was requested (no auto-restart)
	restarts   int
	startedAt  time.Time
	lastExit   error
	lastExitAt time.Time
	wg         sync.WaitGroup
}

func newSupervisor(p *Paths, log *logRing) *supervisor {
	s := &supervisor{paths: p, log: log}
	s.ipc = newIPCStatusClient(p.IPCSocket())
	return s
}

func (s *supervisor) startIPCWatcher() {
	go s.ipc.run()
}

// Start launches the core if it is not running. The config file must
// already be written and valid.
func (s *supervisor) Start() error {
	s.mu.Lock()
	if s.cmd != nil && s.cmd.Process != nil {
		s.mu.Unlock()
		return errors.New("core is already running")
	}
	if !fileExists(s.paths.CoreBin) {
		s.mu.Unlock()
		return fmt.Errorf("core binary %s is missing (install or update it from the web UI)", s.paths.CoreBin)
	}
	if !fileExists(s.paths.ConfFile()) {
		s.mu.Unlock()
		return fmt.Errorf("core config %s is missing — apply the settings first", s.paths.ConfFile())
	}
	// l3 uses raw sockets (IPPROTO_RAW + raw TCP/UDP/ICMP) and needs root
	// or CAP_NET_RAW. The backend drops privileges when run_as is set, so
	// an l3 start under a dropped backend can only fail — refuse with the
	// exact fix instead.
	if cfg, err := LoadUIConfig(s.paths); err == nil && cfg.OpenFlux.Mode == "l3" {
		if !TestMode() && os.Geteuid() != 0 {
			s.mu.Unlock()
			return errors.New("l3 mode needs root or CAP_NET_RAW: set \"run_as\": \"\" in ui.json " +
				"and restart the backend (/opt/etc/init.d/S99opffk restart); " +
				"see docs/COMPATIBILITY.md for the l3 caveats")
		}
	}

	cmd := exec.Command(s.paths.CoreBin,
		"--config", s.paths.ConfFile(),
		"--ipc-socket", s.paths.IPCSocket(),
	)
	cmd.Stdout = &logWriter{ring: s.log}
	cmd.Stderr = &logWriter{ring: s.log}
	cmd.Env = append(os.Environ(),
		// Upstream: "Set GOMEMLIMIT in the environment for a hard soft-cap."
		// Keep the exit node's heap bounded on small routers (overridable).
		"GOMEMLIMIT="+memLimit(),
		"GOGC=20",
	)
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("start %s: %w", s.paths.CoreBin, err)
	}
	_ = os.WriteFile(s.paths.CorePIDFile(), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	s.cmd = cmd
	s.desired = true
	s.manualStop = false
	s.startedAt = time.Now()
	s.lastExit = nil
	s.mu.Unlock()

	s.log.write("supervisor: core started (pid " + strconv.Itoa(cmd.Process.Pid) + ")")

	s.wg.Add(1)
	go s.waitLoop(cmd)
	return nil
}

// memLimit returns the GOMEMLIMIT value for the core child.
func memLimit() string {
	if v := os.Getenv("OPFFK_CORE_GOMEMLIMIT"); v != "" {
		return v
	}
	return "128MiB"
}

func (s *supervisor) waitLoop(cmd *exec.Cmd) {
	defer s.wg.Done()
	err := cmd.Wait()
	now := time.Now()

	s.mu.Lock()
	wasDesired := s.desired
	wasManual := s.manualStop
	s.cmd = nil
	s.lastExit = err
	s.lastExitAt = now
	if wasDesired && !wasManual {
		s.restarts++
	} else {
		s.restarts = 0
	}
	plan := s.restarts
	s.mu.Unlock()

	_ = os.Remove(s.paths.CorePIDFile())
	_ = os.Remove(s.paths.IPCSocket()) // stale socket
	if err == nil {
		s.log.write("supervisor: core exited cleanly")
	} else {
		s.log.write("supervisor: core exited: " + err.Error())
	}

	if wasDesired && !wasManual {
		// Crash backoff: 5s, 15s, then every 60s.
		delay := 5 * time.Second
		switch {
		case plan >= 3:
			delay = 60 * time.Second
		case plan == 2:
			delay = 15 * time.Second
		}
		s.log.write(fmt.Sprintf("supervisor: restarting core in %s (restart #%d)", delay, plan))
		time.Sleep(delay)
		s.mu.Lock()
		stillDesired := s.desired && !s.manualStop && s.cmd == nil
		s.mu.Unlock()
		if stillDesired {
			if err := s.Start(); err != nil {
				s.log.write("supervisor: restart failed: " + err.Error())
			}
		}
	}
}

// Stop terminates the core: SIGTERM, 10s grace, then SIGKILL.
func (s *supervisor) Stop() error {
	s.mu.Lock()
	cmd := s.cmd
	s.desired = false
	s.manualStop = true
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return terminate(cmd)
}

// Restart = Stop + Start (Start validates that config exists).
func (s *supervisor) Restart() error {
	if err := s.Stop(); err != nil {
		return err
	}
	// Give the kernel a moment to release the listen port.
	for i := 0; i < 20; i++ {
		s.mu.Lock()
		gone := s.cmd == nil
		s.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return s.Start()
}

// terminate sends SIGTERM and escalates to SIGKILL after 10 seconds.
// Process liveness is probed with signal 0 — the reaping itself stays in
// waitLoop (exec.Cmd.Wait may only be called once).
func terminate(cmd *exec.Cmd) error {
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return nil // exited; waitLoop reaps it
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cmd.Process.Kill()
}

// Snapshot is what the status endpoint renders.
type Snapshot struct {
	Desired    bool   `json:"desired"`
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	UptimeSec  int64  `json:"uptime_sec,omitempty"`
	Connected  *bool  `json:"connected,omitempty"`
	BytesIn    uint64 `json:"bytes_in,omitempty"`
	BytesOut   uint64 `json:"bytes_out,omitempty"`
	Active     string `json:"active,omitempty"`
	Mode       string `json:"mode,omitempty"`
	LastExit   string `json:"last_exit,omitempty"`
	Restarts   int    `json:"restarts"`
	IPCError   string `json:"ipc_error,omitempty"`
	CoreBin    string `json:"core_bin"`
	CoreBinary bool   `json:"core_binary_present"`
}

func (s *supervisor) Snapshot() Snapshot {
	s.mu.Lock()
	snap := Snapshot{
		Desired:  s.desired,
		LastExit: "",
		Restarts: s.restarts,
	}
	if s.cmd != nil && s.cmd.Process != nil {
		snap.Running = true
		snap.PID = s.cmd.Process.Pid
		snap.UptimeSec = int64(time.Since(s.startedAt).Seconds())
	}
	if s.lastExit != nil {
		snap.LastExit = s.lastExit.Error() + " at " + s.lastExitAt.Format(time.RFC3339)
	}
	snap.CoreBin = s.paths.CoreBin
	snap.CoreBinary = fileExists(s.paths.CoreBin)
	s.mu.Unlock()

	st, ipcErr := s.ipc.snapshot()
	if st != nil && snap.Running {
		connected := st.Connected
		snap.Connected = &connected
		snap.BytesIn = st.BytesIn
		snap.BytesOut = st.BytesOut
		snap.Active = st.Active
	}
	if ipcErr != "" {
		snap.IPCError = ipcErr
	}
	if cfg, err := LoadUIConfig(s.paths); err == nil {
		snap.Mode = cfg.OpenFlux.Mode
	}
	return snap
}

// handleSignal performs a clean shutdown on SIGTERM/SIGINT: stop the core,
// close the IPC watcher and the log file.
func (s *supervisor) handleSignal(sig os.Signal) {
	s.log.write("supervisor: received " + sig.String() + ", shutting down")
	_ = s.Stop()
	s.ipc.close()
	s.log.close()
}

// installSignalHandler wires SIGTERM/SIGINT to a clean shutdown.
func (s *supervisor) installSignalHandler() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-ch
		s.handleSignal(sig)
		os.Exit(0)
	}()
}

// logWriter adapts the logRing to io.Writer for child stdout/stderr,
// splitting on newlines.
type logWriter struct {
	ring *logRing
	buf  []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			line := string(w.buf)
			w.buf = w.buf[:0]
			if line != "" {
				w.ring.write(line)
			}
			continue
		}
		w.buf = append(w.buf, b)
		if len(w.buf) > 8192 { // pathological no-newline output
			w.ring.write(string(w.buf))
			w.buf = w.buf[:0]
		}
	}
	return len(p), nil
}
