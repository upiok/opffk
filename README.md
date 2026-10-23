# opffk — OpenFlux exit node for Keenetic (Entware)

**English · [Русский](README.ru.md)**

A browser-managed [OpenFlux](https://github.com/p1neappleXpress/OpenFlux)
exit-node deployment for Keenetic routers running Entware. One command to
install, a small web UI to configure, start, stop, update and uninstall.

opffk does **not** reimplement anything from OpenFlux: it installs the
upstream core binary (hash-pinned from upstream releases), writes the
upstream `.conf` format, supervises the process, and reads live status from
the core's own IPC socket.

```
┌────────────────────────── Keenetic router (Entware, /opt) ──────────────────┐
│  S99opffk ──▶ opffk-web (Go, ~8 MB, stdlib-only, embedded UI)               │
│                 │  spawn / SIGTERM / crash backoff                          │
│                 ▼                                                           │
│               openflux (upstream core, --role=exit --mode=l4)               │
│                 │  status frames (Unix socket, upstream IPC protocol)       │
│                 ▼                                                           │
│               carriers (direct / yandex / vyandex / mailru / …)             │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Installation

### Prerequisites

- A Keenetic router with **Entware installed** (Keenetic web UI →
  *Other installations* → *Entware*, on USB storage or internal flash) and
  SSH access as `root`.
- **Supported CPU architecture** (`uname -m` over SSH):
  `aarch64`, `armv7l` — see [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).
  MIPS-based Keenetics are **not supported** (upstream publishes no MIPS
  binaries).
- **RAM**: 128 MB free minimum, 256 MB recommended.
- **Storage**: 150 MB free under `/opt` (the upstream core is ~25 MB).
- No systemd, bash, Docker, Python or Node.js required. The installer uses
  POSIX `sh`, `curl` or `wget` (HTTPS-capable), `tar` and a `sha256` tool —
  exactly what Entware already ships or can `opkg install` in one step.

### One command

> **Security warning — read this.** Piping a shell script from the internet
> into `sh` executes code before you (or anyone) has looked at it. Inspect
> the script first, or use the manual method below. Do not pipe untrusted
> URLs.

```sh
curl -fsSL https://raw.githubusercontent.com/YOUR-USER/YOUR-REPO/main/install.sh \
  | OPFFK_REPO=YOUR-USER/YOUR-REPO sh
```

The `OPFFK_REPO` variable tells the installer which repository hosts the
release artifacts. The installer downloads a pinned release tarball plus its
SHA-256 checksum file, verifies both, and only then installs — it never
pipes downloaded content into a shell and never disables TLS verification.

### Manual install (recommended)

```sh
# on the router, as root
cd /tmp
curl -fsSLO https://github.com/YOUR-USER/YOUR-REPO/releases/download/vX.Y.Z/opffk-vX.Y.Z-linux-arm64.tar.gz
curl -fsSLO https://github.com/YOUR-USER/YOUR-REPO/releases/download/vX.Y.Z/opffk-vX.Y.Z-linux-arm64.tar.gz.sha256
sha256sum -c opffk-vX.Y.Z-linux-arm64.tar.gz.sha256
tar -xzf opffk-vX.Y.Z-linux-arm64.tar.gz
sh opffk-vX.Y.Z-linux-arm64/install.sh --from-dir opffk-vX.Y.Z-linux-arm64
```

(Replace `arm64` with `arm` for armv7 devices. Check the checksum output
says **OK** before running anything.)

### What the installer does

1. Detects Entware, architecture, RAM, storage and required tools; refuses
   anything unsupported with actionable diagnostics.
2. Downloads and verifies the release (unless `--from-dir`).
3. Backs up any previous configuration (`/opt/var/lib/opffk/`).
4. Installs binaries, the init script (`/opt/etc/init.d/S99opffk`) and the
   hash-pinned upstream `manifest.json`.
5. Creates an unprivileged service account `openflux` in
   `/opt/etc/passwd` (the Entware passwd file persists across reboots on
   Keenetic, the firmware `/etc/passwd` does not). The backend drops root
   to this user at startup.
6. Starts the service, waits for readiness, and prints the **initial web
   password once**.

### First login

1. Open `http://<router-LAN-IP>:8791` from a device on your LAN.
2. Log in with the printed one-time password (it is invalidated after the
   first login; `sh install.sh reset-password` regenerates one if lost).
3. Change the password under *Settings*.

The web UI binds **only to the router's LAN address** and is not reachable
from the WAN. opffk never modifies Keenetic firewall, port-forwarding,
routing or DNS settings — see [docs/SECURITY.md](docs/SECURITY.md).

## Configuration (web UI)

- **Exit mode**: `l4` (userspace gVisor — supported, no root needed).
  `l3` exists upstream but is gated off: it needs root, iptables and an
  explicit `allow_l3` in `ui.json`, and conflicts with Keenetic's own NAT.
- **Transports**: `direct` (self-hosted, always works, recommended first)
  plus the carrier types upstream supports (`yandex`, `vyandex`, `mailru`,
  `cupsonline`, `boards`, `avito`, `telemost`, `oneme`). At least one
  transport is required; the encryption key is generated and managed
  automatically.
- Every change is validated against the pinned upstream rules *before*
  being written; invalid input never touches the core config.
- **Apply** writes `/opt/etc/opffk/openflux.conf` (upstream INI format);
  **Apply & restart** also restarts the core.
- **Client link** generates an `openflux://` link via the core's own
  `--make-link` for pasting into OpenFlux client apps. The link contains
  the encryption key — treat it as a secret. *Rotate encryption key*
  invalidates all existing links.

Example of the generated config (also see
[docs/openflux.conf.example](docs/openflux.conf.example)):

```ini
Role = exit
Mode = l4
EncryptionKeyFile = /opt/etc/opffk/encryption-key
CookieStore = /opt/var/lib/opffk/cookies.json
Debug = 2
Codec = batched

[Transport direct]
Type = direct
Priority = 50
Listen = 0.0.0.0:7891
```

### Making the exit reachable from the internet (optional)

The exit node listens on `0.0.0.0:<port>` (transport `direct`). For
clients outside your LAN you must create a **TCP port-forward** in the
Keenetic web UI yourself (*Network rules → Port forwarding*, WAN → your
chosen port → router IP, port 7891). opffk deliberately does not do this.

## Updates, rollback, uninstall

- **Core updates** (*Core updates* card): downloads the pinned upstream
  release named in the configured manifest, verifies the SHA-256, swaps
  the binary, restarts and health-checks; a failed health check rolls back
  automatically. *Roll back* returns to the previous binary any time.
  Configure `update_manifest_url` in `ui.json` (or install fresh releases
  with the installer) to enable checks.
- **Backend updates**: re-run the installer (`install.sh upgrade` — plain
  `install` is idempotent and preserves configuration).
- **Uninstall**: the web UI's gated action (type `UNINSTALL`) or
  `sh install.sh uninstall`. Both back up the configuration first. Logs
  and backups under `/opt/var/log/opffk` and `/opt/var/lib/opffk` are kept
  and listed for manual removal.

## Service lifecycle (CLI)

```sh
/opt/etc/init.d/S99opffk {start|stop|restart|status}
sh install.sh status          # install state summary
sh install.sh reset-password  # new one-time web password
```

Logs: `/opt/var/log/opffk/core.log` (+`.1` rotation), `web.log`,
`console.log`.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `architecture ... is not supported` | MIPS or armv6 device — upstream publishes no binary for it; see docs/COMPATIBILITY.md |
| `no Entware found at /opt` | Install Entware via the Keenetic UI first |
| `web UI port ... already in use` | Re-run the installer with `--port N` |
| UI unreachable from LAN | Wrong bind address: re-run with `--listen-ip <router-LAN-IP>`; check `/opt/var/log/opffk/console.log` |
| Core exits / restarts forever | `cat /opt/var/log/opffk/core.log`; usually bad transport URL/token or no RAM — check `MemoryAvailable` |
| Client cannot connect from WAN | Port forwarding not configured (you must do it in the Keenetic UI), or ISP CGNAT |
| Lost web password | `sh install.sh reset-password` |
| `service account "openflux" not found` | The Entware passwd file was restored/trimmed — re-run the installer |

## Security

Summary (full model in [docs/SECURITY.md](docs/SECURITY.md)):

- Authentication required on every route; PBKDF2-HMAC-SHA256 (120k
  iterations) credential hash only; login rate limiting; one-time initial
  password displayed once.
- CSRF protection (double-submit + Origin check) on all state-changing
  routes; strict CSP; no user input is ever passed to a shell (the two
  external processes the backend talks to take JSON on stdin or a fixed
  config file path).
- Secrets (`encryption-key`, `ui.json`) are `0600`; configs `0640`.
- Privilege drop to the `openflux` service account; the core runs
  unprivileged (possible because `l4` is userspace).
- UI bound to the LAN only; no automatic changes to Keenetic settings.

## Verification status

Everything is verified on the development host (x86_64, Fedora); **no
Keenetic device has been tested** — claims about devices are marked
"expected" in [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md). The test
suites and how to run them: [docs/VERIFICATION.md](docs/VERIFICATION.md),
including a full end-to-end tunnel test against the real upstream core.

Publishing guide (fork OpenFlux, publish your own `node-v*` core, release
opffk — in Russian): [docs/PUBLISHING.ru.md](docs/PUBLISHING.ru.md).

## License / attribution

opffk is GPL-3.0-or-later (see `LICENSE`). OpenFlux is
[p1neappleXpress/OpenFlux](https://github.com/p1neappleXpress/OpenFlux),
GPL-3.0 — opffk installs and supervises upstream binaries but ships none of
its code. See `NOTICE`.
