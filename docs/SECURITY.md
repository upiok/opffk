# Security model

## Threat model

A home router's management plane is valuable: it sits inside the LAN,
holds credentials, and controls a process that terminates other people's
traffic. The assumed attackers are:

1. **Another device on the LAN** (guest IoT, a compromised laptop) that
   can reach the router's ports but should not control the exit node.
2. **A passive or active network observer** between the browser and the
   router (LAN Wi-Fi), and between the router and release servers.
3. **The exit-node clients** — people who receive an `openflux://` link —
   who must never gain more than the tunnel itself.
4. Not assumed: a full router compromise (root on the device). opffk
   reduces blast radius but cannot defend a rooted box.

## Authentication

- Single-user web login. The credential is stored **only** as
  PBKDF2-HMAC-SHA256 (120 000 iterations, 16-byte salt, constant-time
  compare) in `ui.json` (0600). The plaintext is never persisted.
- The initial password is 20 characters from an unambiguous 58-char
  alphabet, generated with `crypto/rand`, printed **once** by the
  installer, mirrored to `initial-password` (0600) and deleted after the
  first successful login.
- Login rate limiting: 5 failures → 60 s lockout. Sessions: random 32-byte
  tokens, 12 h expiry, capped at 8 concurrent, `HttpOnly` +
  `SameSite=Strict` cookies.
- `reset-password` (CLI) clears the credential and prints a fresh one-time
  password; it requires root on the router.

## CSRF and browser hardening

- All state-changing endpoints (`POST /api/*`) require the session's CSRF
  token in the `X-Opffk-CSRF` header (or `csrf_token` form field) and
  reject cross-`Origin` requests. The login form uses a separate
  single-use pre-login token (cookie + form) so login CSRF is covered too.
- Response headers on everything: CSP `default-src 'self'` (no inline
  script, no third-party origins), `X-Frame-Options: DENY`,
  `frame-ancestors 'none'`, `nosniff`, `Referrer-Policy: no-referrer`,
  `Cache-Control: no-store`.
- The UI's JavaScript uses only `textContent`/DOM APIs — no
  `innerHTML` with server data — so log lines or transport names cannot
  become HTML.

## Injection

The backend runs exactly two external programs, and neither ever sees
user input on a command line:

1. `openflux --config <fixed path> --ipc-socket <fixed path>` — all
   settings travel through the validated `.conf` file. Values are
   rejected if they contain `#`, `;`, CR/LF or control characters (the
   upstream conf parser treats `#`/`;` as comment starts — this is what a
   config-injection attempt would abuse).
2. `openflux --make-link -` — the share JSON arrives on **stdin**.

There is no other `exec`. The init script takes no arguments from the
web layer. Log values are displayed as text only.

## Privilege model

- The installer creates `openflux` (`/bin/false`, no home) in
  `/opt/etc/passwd` — the Entware file, because Keenetic regenerates the
  firmware `/etc` at boot.
- `opffk-web` binds its listener, writes first-run files, chowns state to
  the service user, then `setgroups`/`setgid`/`setuid` to it. The core
  child inherits the dropped identity; nothing re-escalates. `l4` needs
  no capabilities, so this loses no functionality.
- Files: `encryption-key` 0600, `ui.json` 0600, `openflux.conf` 0640,
  logs 0640, state dirs 0755 owned by the service user.
- If the service account is missing the backend **refuses to start**
  (fails closed) with recovery instructions instead of running as root.

## Secrets

- `encryption-key`: 32 random bytes, hex, in its own 0600 file. Rotation
  is explicit (UI button) and invalidates client links.
- The egress proxy URL may contain `user:pass@` credentials; it is
  written to `openflux.conf` (0640) and shown masked in logs (the core
  prints only the scheme). A proxy password containing `#` or `;` cannot
  be entered from the web UI (the conf format ends values there) — this
  is a documented format limit, not an escaping bug.
- `openflux://` links contain the key by design (upstream behavior). The
  UI shows them only on an explicit action and labels them as secrets.
- Logs never include the key: the core is never run with `--sensitive`
  and the UI's debug-level choices cap at 2 (operational logs; hexdumps
  of frames are level 3 and not offered).

## Network exposure

- The UI binds **one address**: the router's LAN IPv4 (or an explicit
  `--listen-ip`). It is not on `0.0.0.0` by default and not on the WAN
  interface. No TLS is terminated (plain HTTP on the LAN; add your own
  reverse proxy with auth if you need remote access — do not port-forward
  the UI).
- The exit's `direct` transport listens on `0.0.0.0:<port>` by nature.
  WAN reachability requires a port-forward that **you** create in the
  Keenetic UI. opffk never touches Keenetic firewall rules, port
  forwarding, routing or DNS. The client side of the tunnel is
  AES-256-GCM encrypted by the mandatory shared key.
- The core's IPC socket is a Unix socket under `/opt/var/run` — not a
  network port — and the backend only reads status frames.

## Supply chain

- The upstream core is pinned in `manifest.json`: version
  (`node-v1.3.1`), per-architecture SHA-256, and the release URL. The
  installer and the update flow refuse any binary whose hash differs.
  Hashes were verified against the actual upstream release files and
  cross-checked against the pins in upstream's `deploy/node-install.sh`.
- Releases of opffk itself ship a `.sha256` file per tarball; the
  installer verifies the tarball before extracting and re-verifies the
  core against the manifest after extraction.
- No `curl | sh` for privileged steps: downloads land in a staging dir,
  are verified, and executed from disk. TLS verification is never
  disabled (curl `--proto '=https'`, no `-k` anywhere).

## Self-update

The full self-update (web UI → Settings → Core updates → *Update opffk*)
downloads the opffk release tarball over HTTPS together with its
published `.sha256`, verifies both, cross-checks the bundled core
against the bundled manifest, pre-flights the new backend binary, swaps
files atomically and re-execs the backend in place (same PID). Trust
level equals the one-command installer path (HTTPS + the release's own
checksum file). To make this possible the installer hands ownership of
the binaries, the init script and the share directory to the service
user — an accepted trade-off: a compromised web session could replace
the deployment's binaries, but a compromised web session could also
reconfigure the exit node entirely, so the boundary does not move. The
previous backend binary is kept at `<prefix>/bin/opffk-web.previous`
for manual recovery — kept in the state directory (`var/lib/opffk`), the one place the service user can always write.

## What opffk will not do

- Modify Keenetic firewall, port forwarding, routing or DNS settings.
- Run the core as root (outside the gated, discouraged `l3` path).
- Send telemetry, or contact any host except the pinned release URLs and
  the carriers you configure.
