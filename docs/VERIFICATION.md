# Verification plan and results

Four automated suites run on the development host. They are the
authoritative statement of what works; anything not covered here is
documented as "expected, unverified" in
[COMPATIBILITY.md](COMPATIBILITY.md).

## Suites

| Suite | Command | What it covers |
|---|---|---|
| Go unit tests | `(cd backend && go test ./...)` | PBKDF2 against published RFC test vectors, password/session/lockout logic, CSRF (header, form, Origin), OpenFlux settings validation (injection, ranges, gating), generated `.conf` re-parsed with an upstream-compatible parser, IPC frame codec, manifest parsing, version comparison, log ring + rotation, atomic writes, arch mapping |
| Installer shell tests | `sh tests/installer_test.sh` | Architecture mapping, port/IP/hex validators, busy-port detection, SHA-256 verification, sandboxed install layout, init-script flag baking, idempotent upgrade with backup, status, uninstall (files gone, backup kept); regression: a `--from-dir` install must **not** delete the release directory |
| Stack e2e (fake core) | `sh tests/e2e_smoke.sh` | Real installer → real init script → real backend with a stand-in core that speaks the upstream IPC contract: one-time credential, login, CSRF rejection, autostart, stop/start, IPC status surfaced in the API, **crash detection and supervisor restart**, log ring, update check, manual rollback swap, config apply + restart, invalid-config rejection without file damage, client link generation, password change + re-login, gated uninstall with backup |
| Real-tunnel e2e | `OPFFK_UPSTREAM=<checkout> sh tests/tunnel_e2e.sh` | Builds the **real upstream core**, runs it as the exit node **under the backend** (config + key written by the backend), generates the client link through the core's `--make-link`, runs the same core as a SOCKS5 client, and carries a real HTTP request through the tunnel. Validates the generated config/key/session-context against the actual upstream protocol, not a mock |

Status at the time of this writing: **all four suites pass** on
x86_64/Linux (Fedora, Go 1.27, dash/bash as `sh`).

```sh
# run everything
(cd backend && go test ./...)
sh tests/installer_test.sh
sh tests/e2e_smoke.sh
git clone --depth 50 https://github.com/p1neappleXpress/OpenFlux /tmp/OpenFlux
OPFFK_UPSTREAM=/tmp/OpenFlux sh tests/tunnel_e2e.sh
```

## Static checks

```sh
(cd backend && go vet ./... && test -z "$(gofmt -l .)")
sh -n install.sh tests/*.sh scripts/*.sh packaging/S99opffk
```

All clean. `shellcheck` is run when available; the scripts avoid the
constructs shellcheck flags as unsafe (unquoted expansions, `eval` on
user data — the single `eval` in `fetch` is a documented test hook that is
empty in production).

## What is intentionally NOT verified

- No physical Keenetic/Entware device was available. Device-specific
  claims (rc.unslung boot integration, service-user persistence across
  Keenetic reboots, real-world memory headroom) are **expectations** until
  the checklist below has been run on hardware.
- qemu aarch64 runs proved binary startup + flag handling of the upstream
  arm64 release, not full tunnel throughput on ARM hardware.
- MIPS: a source cross-compile succeeds, which is explicitly NOT
  sufficient to claim support.

## Device checklist (run before trusting "expected" rows)

1. `uname -m` on the device → confirm the arch row in COMPATIBILITY.md.
2. `sh install.sh ...` → installer environment checks pass (RAM/storage).
3. `/opt/etc/init.d/S99opffk stop && start` and a **reboot** → service
   comes back via Entware boot scripts.
4. Check `/opt/etc/passwd` still contains `openflux` after reboot; UI
   still logs in (credential persisted).
5. Start the core with the `direct` transport; connect a real client from
   the LAN; confirm traffic in the UI status card.
6. Web update check + rollback button.
7. Uninstall from the UI; confirm binaries/config removed and backup kept.

## Runtime verification facts recorded during development

- Upstream `node-v1.3.1` `openflux-linux-arm64` and `-arm` binaries
  downloaded from the upstream release and matched the SHA-256 pins in
  upstream `deploy/node-install.sh` (2026-10-23, commit `edcf77a`).
- Upstream source (go 1.26.6 in `go.mod`, built with Go 1.27.3)
  cross-compiles cleanly with `CGO_ENABLED=0` for `linux/arm` (GOARM=7),
  `linux/arm64`, and — as evidence, not support — `linux/mipsle
  GOMIPS=softfloat`.
- The backend-generated `openflux.conf` was accepted by the real core,
  which reported `Running as EXIT NODE` and listened on the configured
  direct port.
- A real HTTP request traversed: curl → SOCKS5 (real core, client mode) →
  transport → real core exit (l4/proxy) → re-dial → local HTTP server,
  returning the expected payload byte-for-byte.
