# Compatibility matrix

**Honesty first:** the statements below distinguish what was *tested*,
what is *expected* (based on verified build facts but no device run), and
what is *unsupported*. No Keenetic device was available during
development; the device rows in the "expected" tier must be re-verified by
the first user who tries — the installer's own checks on the device are
authoritative and will refuse anything it cannot support.

## Architectures (what the installer checks: `uname -m`)

| `uname -m` | Upstream binary used | Tier | Evidence |
|---|---|---|---|
| `x86_64` / `amd64` | `openflux-linux-amd64` (node-v1.3.1) | **Tested** (dev host + qemu) | Full test suite + real-protocol tunnel test ran on amd64; sha256 verified |
| `aarch64` | `openflux-linux-arm64` (node-v1.3.1) | **Tested (build/runtime) / expected (device)** | Release binary sha256 verified against upstream pin; upstream source cross-compiles and runs under `qemu-aarch64` (runtime + flag parsing verified). No ARM device run. |
| `armv7l` / `armv7hl` / `armv7ve` | `openflux-linux-arm` (GOARM=7, node-v1.3.1) | **Expected** | Release binary sha256 verified against upstream pin; upstream CI builds this target. Static binary (CGO off) → no libc/float-ABI dependency on the Entware armv7sf/armv7hf userspace. No device run. |
| `mips`, `mipsel` (most classic Keenetic models, e.g. MT7621/MT7628-based) | none published upstream | **Unsupported by the installer** | Upstream release workflows build only amd64/arm64/arm(7). A source cross-compile for `mipsle/softfloat` *does succeed* (verified with Go 1.27 — gVisor's tcpip is pure Go), but there is no upstream release, no device evidence, and exit-node memory pressure on 128–256 MB MIPS devices is unknown. `scripts/build.sh --mipsle` can produce an experimental build; it is never installed by the installer. |
| `armv6l`, `riscv64`, others | none | **Unsupported** | No upstream binary; Go/gVisor support absent or unproven |

## Entware / system environment

| Component | Tier | Notes |
|---|---|---|
| Entware on `/opt` (Keenetic "Entware" / "Entware on USB") | **Expected** | Installer requires `/opt/bin/opkg` or `/opt/etc/opkg.conf`; Keenetic's opkg boot integration (`rc.unslung`) runs `S99opffk` |
| Init: `/opt/etc/init.d/` scripts (no systemd) | **Tested** (sandbox + script logic) | Pure POSIX init script, pidfile-based |
| busybox `ash` as `/bin/sh` | **Expected** | Installer avoids bashisms; verified with `sh -n` and dash/ash semantics; POSIX sh on dev host tested |
| Service user in `/opt/etc/passwd` | **Expected** | Chosen because Keenetic regenerates firmware `/etc` on boot; Entware prefix persists. Privilege drop via `setgid/setuid` exercised in unit tests; not on a live Keenetic yet |
| `curl` or `wget` with HTTPS | **Required** | `opkg install curl` if missing; TLS verification never disabled |
| `sha256sum` (coreutils or busybox) / `shasum` | **Required** | Installer checks and tells you the `opkg` command |
| `tar` | **Required** | `opkg install tar` |
| RAM ≥ 128 MB free (256 MB recommended) | **Hard requirement** | Installer measures `MemAvailable`; hard-fails below 128 MB, warns below 256 MB (GOMEMLIMIT caps the core at 128 MiB) |
| Storage ≥ 150 MB free on `/opt` | **Hard requirement** | Installer measures `df` on the prefix |

## OpenFlux exit modes

| Mode | Tier | Notes |
|---|---|---|
| `l4` (gVisor userspace proxy) | **Supported / tested against the real core** | No root needed; upstream's own node installer defaults to it; a full HTTP request was carried through the real l4 exit + SOCKS5 client in `tests/tunnel_e2e.sh` |
| `l3` (SNAT/DNAT) | **Gated off** | Requires root + iptables + a scoped outbound-RST drop; conflicts with Keenetic's own NAT/firewall and would require firmware settings changes opffk refuses to make. Unlock only by setting `allow_l3: true` in `ui.json` on a device you have verified has root+iptables; **not recommended on Keenetic** |

## Transport types (exit side)

| Type | Tier | Notes |
|---|---|---|
| `direct` | **Supported / tested** | Self-hosted TCP carrier; requires the encryption key (always generated); needs a port-forward for WAN clients |
| `yandex`, `vyandex`, `mailru`, `cupsonline`, `boards`, `avito`, `telemost` | **Expected** | Validated per upstream rules (https URL); needs working account cookies/URLs; cookie store persisted under `/opt/var/lib/opffk` |
| `oneme` (MAX) | **Expected** | Token + UID validated; tokens are per-account (never included in client links) |
| `script` (signed JS) | **Not exposed in v1** | Upstream supports it; managing script trust from a web UI is a separate review. Can be configured by hand in `openflux.conf` (the backend preserves hand edits until the next UI apply) |

## Known limitations

- Backend self-update is not in the web UI by design (a running service
  replacing its own binary is exactly where rollback bugs live); use
  `install.sh upgrade`.
- The `cupsonline` exit learns its room list only at runtime, so client
  links for it are only correct while it runs (upstream behavior, not an
  opffk limitation).
- No IPv6 listener for the web UI (bind is a single IPv4 address; the
  core's carriers are IPv4 as upstream on these platforms).

## How to re-verify on your device

See [VERIFICATION.md](VERIFICATION.md) § "Device checklist". Run it and
open an issue with the output before relying on a model marked only
"expected".
