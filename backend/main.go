package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// opffk-web — the OpenFlux-for-Keenetic management backend.
//
// One static binary that serves the web UI, owns the upstream openflux core
// child process (start/stop/crash backoff), reads its status over the core's
// IPC socket, manages the core's .conf and encryption key, and implements
// hash-verified core updates with automatic rollback.
//
// Paths all hang off the Entware prefix (/opt), overridable with OPFFK_ROOT
// for the test sandbox.

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "opffk-web: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	listenFlag := flag.String("listen", "", "IP address for the web UI (overrides ui.json)")
	portFlag := flag.Int("port", 0, "port for the web UI (overrides ui.json)")
	autostartFlag := flag.Bool("autostart", true, "start the core when the backend starts")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("opffk-web " + version)
		return nil
	}

	paths := NewPaths()
	if err := paths.EnsureDirs(); err != nil {
		return err
	}

	// ---- configuration (creates the first-run credential when absent) ----
	cfg, err := LoadUIConfig(paths)
	initialPassword := ""
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = DefaultUIConfig()
		if initialPassword, err = firstRun(paths, cfg); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("load %s: %w", paths.UIConfigFile(), err)
	}
	if cfg.Auth.Hash == "" || cfg.Auth.Salt == "" {
		return fmt.Errorf("%s has no credential hash — delete it to re-run first-run setup", paths.UIConfigFile())
	}

	if *listenFlag != "" {
		if net.ParseIP(*listenFlag) == nil {
			return fmt.Errorf("-listen: %q is not an IP address", *listenFlag)
		}
		cfg.ListenIP = *listenFlag
	}
	if *portFlag != 0 {
		if *portFlag < 1 || *portFlag > 65535 {
			return fmt.Errorf("-port: out of range")
		}
		cfg.Port = *portFlag
	}

	// ---- core config: materialize from settings when missing ----
	if errs := ValidateSettings(&cfg.OpenFlux, cfg.AllowL3); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "opffk-web: stored settings invalid: %s\n", e)
		}
		return errors.New("stored OpenFlux settings are invalid; fix ui.json or delete it for a fresh setup")
	}
	if !fileExists(paths.ConfFile()) {
		if err := WriteCoreConfig(paths, &cfg.OpenFlux); err != nil {
			return fmt.Errorf("write core config: %w", err)
		}
	}
	if !fileExists(paths.KeyFile()) {
		key, err := GenerateKey()
		if err != nil {
			return err
		}
		if err := writeFileAtomic(paths.KeyFile(), []byte(key+"\n"), 0o600); err != nil {
			return err
		}
	}

	// ---- hand first-run files to the service user, then drop root ----
	if !TestMode() && cfg.RunAs != "" && os.Geteuid() == 0 {
		if uid, gid, lerr := lookupUser(paths.PasswdFile, cfg.RunAs); lerr == nil {
			for _, f := range []string{paths.UIConfigFile(), paths.InitialPassFile(),
				paths.ConfFile(), paths.KeyFile(), paths.CoreLogFile(), paths.WebLogFile()} {
				_ = chownTree(f, uid, gid)
			}
		}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenIP, strconv.Itoa(cfg.Port)))
	if err != nil {
		return fmt.Errorf("listen on %s:%d: %w (is the address correct and the port free?)", cfg.ListenIP, cfg.Port, err)
	}
	if err := dropPrivileges(paths, cfg); err != nil {
		listener.Close()
		return fmt.Errorf("privilege drop failed: %w (re-run the installer to create the service account, or set run_as=\"\" in ui.json)", err)
	}

	// ---- runtime ----
	if err := os.MkdirAll(filepath.Dir(paths.PIDFile()), 0o755); err != nil {
		return err
	}
	_ = os.WriteFile(paths.PIDFile(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)

	app := NewApp(paths, cfg, version)
	app.sup.installSignalHandler()
	app.sup.startIPCWatcher()

	url := fmt.Sprintf("http://%s:%d", displayHost(cfg.ListenIP), cfg.Port)
	fmt.Printf("opffk-web %s: listening on %s\n", version, url)
	if initialPassword != "" {
		fmt.Printf("\n")
		fmt.Printf("  Initial web password: %s\n", initialPassword)
		fmt.Printf("  (shown once; also stored in %s until the first login)\n", paths.InitialPassFile())
		fmt.Printf("  Open %s and change the password under Settings.\n\n", url)
	}

	if *autostartFlag && cfg.AutoStart {
		if err := app.sup.Start(); err != nil {
			app.logWebEvent("autostart failed: " + err.Error())
			fmt.Printf("note: core not started: %v (start it from the web UI)\n", err)
		}
	}

	server := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    8192,
	}
	return server.Serve(listener)
}

// displayHost renders wildcard bind addresses readably in URLs.
func displayHost(ip string) string {
	switch ip {
	case "0.0.0.0", "::":
		return "<router-LAN-IP>"
	default:
		return ip
	}
}
