#!/bin/sh
# opffk — OpenFlux exit node for Keenetic (Entware) — installer.
#
# One-command form (see README for the security note on piping scripts):
#
#   curl -fsSL https://raw.githubusercontent.com/YOUR-REPOSITORY/main/install.sh | OPFFK_REPO=YOUR-USER/YOUR-REPO sh
#
# Manual (recommended) form:
#
#   curl -fsSLO https://github.com/YOUR-REPOSITORY/releases/download/vX.Y.Z/opffk-vX.Y.Z-linux-arm64.tar.gz
#   curl -fsSLO https://github.com/YOUR-REPOSITORY/releases/download/vX.Y.Z/opffk-vX.Y.Z-linux-arm64.tar.gz.sha256
#   sha256sum -c opffk-vX.Y.Z-linux-arm64.tar.gz.sha256
#   tar -xzf opffk-vX.Y.Z-linux-*.tar.gz && sh install.sh --from-dir opffk-vX.Y.Z-linux-*
#
# The installer itself never pipes downloads into a shell: it fetches a
# pinned release tarball to a staging directory, verifies its SHA-256
# against the published checksum file, and only then executes files from
# disk. TLS verification is never disabled.
#
# Commands:
#   install            (default) install or upgrade in place
#   uninstall          stop and remove opffk (backs up the config first)
#   status             show whether the service is installed and running
#   reset-password     invalidate the web password and print a fresh one
#
# Flags:
#   --repo USER/NAME   repository hosting the opffk releases (or env OPFFK_REPO)
#   --version V        release tag to install (or env OPFFK_VERSION, default pinned below)
#   --base-url URL     override the release download base URL
#   --from-dir DIR     install from a local, already-downloaded release
#                      directory instead of downloading (offline install)
#   --prefix DIR       Entware prefix (default /opt; do not change on Keenetic)
#   --listen-ip IP     web UI bind address (default: detected LAN address)
#   --port N           web UI port (default 8791)
#   --no-start         do not start the service after installing
#   --dry-run          show what would be done without changing anything
#
# Environment overrides used by the automated tests (documented, not secret):
#   OPFFK_TEST_MODE=1  skip root/Entware assertions for test sandboxes
#   OPFFK_UNAME=M      override uname -m for architecture mapping tests

set -u

# ---- pinned defaults (see manifest.json; updated together with it) ---------
OPFFK_VERSION_DEFAULT="v0.1.3"

OPFFK_PREFIX="${OPFFK_PREFIX:-/opt}"
OPFFK_REPO="${OPFFK_REPO:-}"
OPFFK_VERSION="${OPFFK_VERSION:-$OPFFK_VERSION_DEFAULT}"
OPFFK_BASE_URL="${OPFFK_BASE_URL:-}"
OPFFK_FROM_DIR=""
OPFFK_LISTEN_IP=""
OPFFK_PORT=""
OPFFK_NO_START=0
OPFFK_DRY_RUN=0
OPFFK_TEST_MODE="${OPFFK_TEST_MODE:-0}"
OPFFK_CMD="install"

# ---- output helpers ---------------------------------------------------------

if [ -t 1 ]; then
    C_G="\033[32m"; C_Y="\033[33m"; C_R="\033[31m"; C_B="\033[1m"; C_0="\033[0m"
else
    C_G=""; C_Y=""; C_R=""; C_B=""; C_0=""
fi

info()  { printf '%b[opffk]%b %s\n' "$C_G" "$C_0" "$1"; }
warn()  { printf '%b[opffk warn]%b %s\n' "$C_Y" "$C_0" "$1"; }
err()   { printf '%b[opffk error]%b %s\n' "$C_R" "$C_0" "$1" >&2; }
die()   { err "$1"; printf '%s\n' "$2" >&2 2>/dev/null || true; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

# ---- testable helpers -------------------------------------------------------

# detect_arch UNAME_M: map uname -m to the upstream release architecture.
# Only architectures with published upstream binaries are supported; the
# third field of docs/COMPATIBILITY.md explains why (grep MIPS there).
detect_arch() {
    _m="${1:-$(uname -m)}"
    case "$_m" in
        x86_64|amd64)   echo "amd64" ;;
        aarch64|arm64)  echo "arm64" ;;
        armv7l|armv7hl|armv7ve) echo "arm" ;;
        *) echo "unsupported:$_m" ;;
    esac
}

validate_port() {
    case "$1" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

validate_ipv4() {
    _ip="$1"
    case "$_ip" in
        ''|*[!0-9.]*|.*|*.) return 1 ;;
    esac
    _n=0
    _oldIFS="$IFS"; IFS='.'
    for _o in $_ip; do
        IFS="$_oldIFS"
        case "$_o" in ''|*[!0-9]*) return 1 ;; esac
        [ "$_o" -le 255 ] || return 1
        _n=$((_n+1))
    done
    IFS="$_oldIFS"
    [ "$_n" -eq 4 ]
}

validate_key_hex() {
    case "$1" in
        ''|*[!0-9a-fA-F]*) return 1 ;;
    esac
    [ "${#1}" -eq 64 ]
}

# mem_available_kb: MemAvailable from /proc/meminfo, 0 when unreadable.
mem_available_kb() {
    _ma=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo 2>/dev/null)
    [ -n "${_ma:-}" ] || _ma=$(awk '/^MemFree:|^Buffers:|^Cached:/ {s+=$2} END {print s}' /proc/meminfo 2>/dev/null)
    [ -n "${_ma:-}" ] || _ma=0
    echo "$_ma"
}

# disk_free_kb DIR: free space on the filesystem holding DIR.
disk_free_kb() {
    df -Pk "$1" 2>/dev/null | awk 'NR==2 {print $4}'
}

# port_is_busy N: best-effort check with busybox/proc tools.
port_is_busy() {
    if have netstat; then
        netstat -tln 2>/dev/null | grep -q "[:.]$1 "
        return $?
    fi
    if [ -r /proc/net/tcp ]; then
        _hex=$(printf '%04X' "$1" 2>/dev/null) || return 1
        awk '{print $2}' /proc/net/tcp 2>/dev/null | grep -qi ":$_hex\$"
        return $?
    fi
    return 1  # unknown; assume free rather than blocking the install
}

# fetch URL DST: download over HTTPS with verification enabled.
fetch() {
    _url="$1"; _dst="$2"
    if [ -n "${OPFFK_FETCH_CMD:-}" ]; then
        # Test hook: run the provided command instead of a downloader.
        eval "$OPFFK_FETCH_CMD" \"$_url\" \"$_dst\"
        return $?
    fi
    if have curl; then
        curl -fsSL --proto '=https' --tlsv1.2 -o "$_dst" "$_url"
    elif have wget; then
        wget -q -O "$_dst" "$_url" || return 1
    else
        err "neither curl nor wget is available; install one with: opkg install curl"
        return 1
    fi
}

# sha256_of FILE
sha256_of() {
    if have sha256sum; then
        sha256sum "$1" | awk '{print $1}'
    elif have shasum; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif have busybox && busybox sha256sum "$1" >/dev/null 2>&1; then
        busybox sha256sum "$1" | awk '{print $1}'
    else
        err "no sha256 tool found (opkg install coreutils-sha256sum)"
        return 1
    fi
}

# verify_sha256 FILE EXPECTED_HEX
verify_sha256() {
    _got=$(sha256_of "$1") || return 1
    [ "$_got" = "$(printf '%s' "$2" | tr 'A-F' 'a-f')" ]
}

# ---- environment checks -----------------------------------------------------

require_entware() {
    if [ "$OPFFK_TEST_MODE" = "1" ]; then
        info "test mode: skipping Entware assertion (prefix $OPFFK_PREFIX)"
        return 0
    fi
    if [ -x "$OPFFK_PREFIX/bin/opkg" ] || [ -f "$OPFFK_PREFIX/etc/opkg.conf" ] || [ -f "$OPFFK_PREFIX/etc/entware-release" ]; then
        info "Entware prefix detected at $OPFFK_PREFIX"
        return 0
    fi
    die "no Entware found at $OPFFK_PREFIX" \
"opffk targets Keenetic routers with Entware (opkg) installed.
Install Entware first (Keenetic web UI: Other installations -> Entware),
then re-run this installer. See https://help.keenetic.com (search 'Entware')."
}

require_root() {
    if [ "$OPFFK_TEST_MODE" = "1" ]; then
        return 0
    fi
    if [ "$(id -u)" = "0" ]; then
        return 0
    fi
    die "root is required to install into $OPFFK_PREFIX" \
"Log in to the router over SSH as root and re-run the installer."
}

check_environment() {
    _arch_uname=$(uname -m)
    _arch=$(detect_arch "$_arch_uname")
    case "$_arch" in
        unsupported:*)
            die "CPU architecture ${_arch_uname} is not supported" \
"Upstream OpenFlux publishes Linux binaries for amd64, arm64 and armv7 only.
This device reports uname -m = ${_arch_uname} (typically MIPS Keenetic models).
A source build for mipsle compiles but has no upstream release and no device
testing behind it — see docs/COMPATIBILITY.md for the details and options.
Refusing to install an unverified binary."
            ;;
    esac
    info "architecture: ${_arch_uname} -> upstream binary arch '$_arch'"

    _memkb=$(mem_available_kb)
    if [ "$_memkb" -lt 131072 ] 2>/dev/null; then
        die "not enough RAM: $((_memkb/1024)) MB available, need at least 128 MB" \
"The OpenFlux core (gVisor userspace networking) and the web backend both
need memory. Free RAM by disabling unused Keenetic components or use a
model with more memory."
    fi
    if [ "$_memkb" -lt 262144 ] 2>/dev/null; then
        warn "only $((_memkb/1024)) MB RAM available; the exit node will work but expect tight margins (GOMEMLIMIT is capped at 128 MiB)"
    fi

    _freekb=$(disk_free_kb "$OPFFK_PREFIX")
    if [ "$_freekb" -lt 153600 ] 2>/dev/null; then
        die "not enough storage: $((_freekb/1024)) MB free under $OPFFK_PREFIX, need at least 150 MB" \
"The core binary alone is ~25 MB; the backend ~8 MB; plus logs and backups.
Install Entware on USB storage if internal flash is too small."
    fi
    if [ "$_freekb" -lt 409600 ] 2>/dev/null; then
        warn "only $((_freekb/1024)) MB free under $OPFFK_PREFIX — watch log growth"
    fi

    if ! have tar; then
        die "tar is missing" "Install it with: opkg install tar"
    fi
    if ! sha256_of /dev/null >/dev/null 2>&1; then
        die "no sha256 tool available" "Install one with: opkg install coreutils-sha256sum"
    fi
    info "environment checks passed"
}

# ---- staging & downloads ------------------------------------------------------

STAGE=""
STAGE_REMOVE=0   # only directories the installer created are removed

cleanup_stage() {
    if [ "$STAGE_REMOVE" = "1" ] && [ -n "$STAGE" ] && [ -d "$STAGE" ]; then
        rm -rf "$STAGE"
    fi
    STAGE=""
}

trap cleanup_stage EXIT INT TERM

new_stage() {
    STAGE=$(mktemp -d "${TMPDIR:-/tmp}/opffk-install.XXXXXX") || die "mktemp failed" ""
    chmod 700 "$STAGE"
    STAGE_REMOVE=1
}

# stage_from_dir DIR: validate a local release directory.
stage_from_dir() {
    _dir="$1"
    [ -d "$_dir" ] || die "--from-dir: no such directory: $_dir" ""
    for _f in install.sh manifest.json; do
        [ -f "$_dir/$_f" ] || die "--from-dir: $_dir lacks $_f (not a release directory?)" ""
    done
    _arch=$(detect_arch "$(uname -m)")
    [ -f "$_dir/opffk-web-$_arch" ] || die "release directory lacks opffk-web-$_arch for this architecture" ""
    STAGE=$(cd "$_dir" && pwd)
    info "using local release directory: $STAGE"
}

# stage_from_release: download the pinned tarball and verify it.
stage_from_release() {
    if [ -z "$OPFFK_REPO" ] && [ -z "$OPFFK_BASE_URL" ]; then
        die "download source unknown" \
"Set the repository hosting the opffk release, then re-run:

  curl -fsSL .../install.sh | OPFFK_REPO=YOUR-USER/YOUR-REPO sh
  (or: sh install.sh --repo YOUR-USER/YOUR-REPO)

Releases are expected at:
  https://github.com/\$REPO/releases/download/$OPFFK_VERSION/opffk-$OPFFK_VERSION-linux-<arch>.tar.gz"
    fi
    if [ -z "$OPFFK_BASE_URL" ]; then
        OPFFK_BASE_URL="https://github.com/$OPFFK_REPO/releases/download/$OPFFK_VERSION"
    fi
    _arch=$(detect_arch "$(uname -m)")
    _tgz="opffk-$OPFFK_VERSION-linux-$_arch.tar.gz"
    info "downloading $OPFFK_BASE_URL/$_tgz"
    new_stage
    fetch "$OPFFK_BASE_URL/$_tgz" "$STAGE/$_tgz" || die "download failed: $_tgz" \
"Check network connectivity from the router. Entware wget must support HTTPS
(opkg install wget-ssl or curl)."
    fetch "$OPFFK_BASE_URL/$_tgz.sha256" "$STAGE/$_tgz.sha256" || die "download failed: $_tgz.sha256" ""
    (
        cd "$STAGE" || exit 1
        # The checksum file is "<hex>  <name>"; verify strictly.
        _line=$(cat "$_tgz.sha256")
        _want=${_line%%[[:space:]]*}
        _got=$(sha256_of "$_tgz") || exit 1
        if [ "$_got" != "$(printf '%s' "$_want" | tr 'A-F' 'a-f')" ]; then
            err "SHA-256 mismatch for $_tgz"
            err "  expected: $_want"
            err "  got:      $_got"
            exit 1
        fi
        info "tarball SHA-256 verified"
    ) || die "tarball verification failed" "Do not retry blindly: if the mismatch persists, the release may be corrupted or tampered with."
    tar -xzf "$STAGE/$_tgz" -C "$STAGE" || die "tar extraction failed" ""
    # The release tarball roots at a single opffk-<version>-linux-<arch>/
    # directory (so a manual extraction gives the user a folder). Flatten
    # it into the staging dir; flat tarballs are left as they are.
    _inner="$STAGE/opffk-$OPFFK_VERSION-linux-$_arch"
    if [ -d "$_inner" ] && [ -f "$_inner/install.sh" ]; then
        mv "$_inner"/* "$STAGE"/ || die "cannot unpack release directory" ""
        rmdir "$_inner" 2>/dev/null || true
    fi
    [ -f "$STAGE/install.sh" ] && [ -f "$STAGE/opffk-web-$_arch" ] || die "tarball content invalid" \
"Expected install.sh and opffk-web-$_arch after extraction. The tarball may
be from a different release layout - download a fresh release."
    info "release unpacked to staging"
}

# verify_core_binary: cross-check the core against the bundled manifest.
verify_core_binary() {
    _arch="$1"
    [ -f "$STAGE/manifest.json" ] || die "release lacks manifest.json" ""
    _core="$STAGE/openflux-linux-$_arch"
    [ -f "$_core" ] || return 0  # release may not bundle the core; installer downloads it later
    _want=$(sed -n "s/.*\"$_arch\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/\1/p" "$STAGE/manifest.json" | head -n 1)
    [ -n "$_want" ] || die "manifest.json has no sha256 for arch $_arch" ""
    verify_sha256 "$_core" "$_want" || die "core binary fails manifest verification — refusing to install" \
"Expected sha256 $_want for $_arch (pinned in manifest.json)."
    info "core binary verified against pinned manifest"
}

# ---- install -----------------------------------------------------------------

prefix_paths() {
    BIN_DIR="$OPFFK_PREFIX/bin"
    ETC_DIR="$OPFFK_PREFIX/etc/opffk"
    INIT_DIR="$OPFFK_PREFIX/etc/init.d"
    STATE_DIR="$OPFFK_PREFIX/var/lib/opffk"
    LOG_DIR="$OPFFK_PREFIX/var/log/opffk"
    RUN_DIR="$OPFFK_PREFIX/var/run"
    SHARE_DIR="$OPFFK_PREFIX/share/opffk"
    INIT_SCRIPT="$INIT_DIR/S99opffk"
    WEB_BIN="$BIN_DIR/opffk-web"
    CORE_BIN="$BIN_DIR/openflux"
    SERVICE_USER="openflux"
}

is_installed() {
    [ -x "$WEB_BIN" ] && [ -f "$OPFFK_PREFIX/share/opffk/manifest.json" ]
}

service_ctl() {
    _action="$1"
    if [ "$OPFFK_TEST_MODE" = "1" ] && [ ! -x "$INIT_SCRIPT" ]; then
        return 0
    fi
    if [ -x "$INIT_SCRIPT" ]; then
        "$INIT_SCRIPT" "$_action" >/dev/null 2>&1
        return $?
    fi
    return 0
}

service_pid_alive() {
    [ -f "$RUN_DIR/opffk-web.pid" ] || return 1
    _pid=$(cat "$RUN_DIR/opffk-web.pid" 2>/dev/null)
    [ -n "$_pid" ] || return 1
    kill -0 "$_pid" 2>/dev/null
}

stop_service() {
    if service_pid_alive; then
        info "stopping opffk-web (pid $(cat "$RUN_DIR/opffk-web.pid"))"
        service_ctl stop
        _i=0
        while service_pid_alive && [ "$_i" -lt 10 ]; do
            sleep 1
            _i=$((_i+1))
        done
        if service_pid_alive; then
            warn "service did not stop cleanly; sending SIGKILL"
            kill -9 "$(cat "$RUN_DIR/opffk-web.pid")" 2>/dev/null || true
        fi
    fi
    service_ctl stop 2>/dev/null || true
}

backup_config() {
    [ -d "$ETC_DIR" ] || return 0
    have tar || return 0
    mkdir -p "$STATE_DIR"
    _f="$STATE_DIR/opffk-backup-$(date -u '+%Y%m%dT%H%M%SZ').tar.gz"
    tar -czf "$_f" -C "$OPFFK_PREFIX/etc" opffk 2>/dev/null || {
        warn "backup failed (continuing; config is preserved in place on upgrade anyway)"
        return 0
    }
    chmod 600 "$_f" 2>/dev/null || true
    info "configuration backed up to $_f"
    # keep the newest 5
    ls -1t "$STATE_DIR"/opffk-backup-*.tar.gz 2>/dev/null | awk 'NR>5' | while IFS= read -r _old; do
        rm -f "$_old"
    done
}

ensure_service_user() {
    if [ "$OPFFK_TEST_MODE" = "1" ]; then
        info "test mode: skipping service user creation"
        return 0
    fi
    if grep -q "^${SERVICE_USER}:" "$OPFFK_PREFIX/etc/passwd" 2>/dev/null; then
        info "service user '$SERVICE_USER' already present"
        return 0
    fi
    _uid=$(next_free_uid "$OPFFK_PREFIX/etc/passwd")
    echo "${SERVICE_USER}:x:${_uid}:${_uid}:opffk service:/nonexistent:/bin/false" >> "$OPFFK_PREFIX/etc/passwd"
    echo "${SERVICE_USER}:x:${_uid}:${SERVICE_USER}" >> "$OPFFK_PREFIX/etc/group" 2>/dev/null || true
    info "created service user '$SERVICE_USER' (uid $_uid) in $OPFFK_PREFIX/etc/passwd"
    info "note: the account lives in the Entware passwd file, which persists across reboots (unlike the firmware /etc/passwd)"
}

install_files() {
    _arch="$1"
    [ "$OPFFK_DRY_RUN" = "1" ] && { info "dry-run: would install binaries and scripts"; return 0; }

    mkdir -p "$BIN_DIR" "$ETC_DIR" "$INIT_DIR" "$STATE_DIR" "$LOG_DIR" "$RUN_DIR" "$SHARE_DIR"

    # Binaries: install to a temp name and rename, so an interrupted copy
    # never leaves a half-written executable.
    install -m 0755 "$STAGE/opffk-web-$_arch" "$WEB_BIN.new" 2>/dev/null || {
        cp "$STAGE/opffk-web-$_arch" "$WEB_BIN.new" && chmod 0755 "$WEB_BIN.new"
    }
    mv -f "$WEB_BIN.new" "$WEB_BIN"

    if [ -f "$STAGE/openflux-linux-$_arch" ]; then
        cp "$STAGE/openflux-linux-$_arch" "$CORE_BIN.new" && chmod 0755 "$CORE_BIN.new"
        mv -f "$CORE_BIN.new" "$CORE_BIN"
        # Record the core version from the manifest for the update check.
        _ver=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\(node-v[^"]*\)".*/\1/p' "$STAGE/manifest.json" | head -n 1)
        [ -n "$_ver" ] && printf '%s\n' "$_ver" > "$SHARE_DIR/VERSION"
    fi
    if [ ! -x "$CORE_BIN" ]; then
        die "no OpenFlux core binary available for $_arch" \
"The release tarball should bundle openflux-linux-$_arch (hash-pinned in
manifest.json). Download a fresh release or install with --from-dir."
    fi

    cp "$STAGE/manifest.json" "$SHARE_DIR/manifest.json" && chmod 0644 "$SHARE_DIR/manifest.json"
    [ -f "$STAGE/VERSION" ] && cp "$STAGE/VERSION" "$SHARE_DIR/opffk-VERSION" 2>/dev/null || true

    # Init script with the prefix substituted.
    sed "s|@PREFIX@|$OPFFK_PREFIX|g" "$STAGE/S99opffk" > "$INIT_SCRIPT.new" || die "cannot render init script" ""
    chmod 0755 "$INIT_SCRIPT.new"
    mv -f "$INIT_SCRIPT.new" "$INIT_SCRIPT"

    # State and log directories belong to the service user.
    if ! grep -q "^${SERVICE_USER}:" "$OPFFK_PREFIX/etc/passwd" 2>/dev/null; then
        : # user created later by ensure_service_user; chown again there
    fi
    if have chown && grep -q "^${SERVICE_USER}:" "$OPFFK_PREFIX/etc/passwd" 2>/dev/null; then
        _uid=$(grep "^${SERVICE_USER}:" "$OPFFK_PREFIX/etc/passwd" | head -n 1 | cut -d: -f3)
        chown -R "${_uid}:${_uid}" "$STATE_DIR" "$LOG_DIR" "$RUN_DIR" 2>/dev/null || true
        chown "${_uid}:${_uid}" "$ETC_DIR" 2>/dev/null || true
    fi
    chmod 0755 "$STATE_DIR" "$LOG_DIR" "$RUN_DIR" 2>/dev/null || true

    info "binaries and scripts installed under $OPFFK_PREFIX"
}

write_ui_overrides() {
    # The backend generates ui.json on first start. The installer's only
    # overrides are the listen address and port, passed as flags on the
    # init script command line (persisted in the init script itself).
    return 0
}

wait_ready_and_report() {
    _port="$1"
    _i=0
    while [ "$_i" -lt 30 ]; do
        if have curl; then
            curl -fsS -o /dev/null "http://127.0.0.1:$_port/login" 2>/dev/null && break
        elif have wget; then
            wget -q -O /dev/null "http://127.0.0.1:$_port/login" 2>/dev/null && break
        else
            break  # no downloader; assume OK and let the user check
        fi
        sleep 1
        _i=$((_i+1))
    done
    _downloader=0
    if have curl || have wget; then _downloader=1; fi
    if [ "$_downloader" = "0" ] || curl -fsS -o /dev/null "http://127.0.0.1:$_port/login" 2>/dev/null || wget -q -O /dev/null "http://127.0.0.1:$_port/login" 2>/dev/null; then
        info "web UI is answering on port $_port"
    else
        die "service did not start" \
"Inspect the logs:
  cat $LOG_DIR/console.log
  cat $LOG_DIR/core.log
Common causes: port already in use (change --port), wrong --listen-ip,
or not enough RAM. Re-run the installer after fixing; the configuration
is preserved."
    fi
    # Show the one-time credential if this was a fresh install.
    if [ -f "$ETC_DIR/initial-password" ]; then
        _pw=$(cat "$ETC_DIR/initial-password" 2>/dev/null || true)
        if [ -n "$_pw" ]; then
            printf '\n'
            printf '%b======================================================%b\n' "$C_B" "$C_0"
            printf '  Initial web password: %b%s%b\n' "$C_B" "$_pw" "$C_0"
            printf '  Shown once. It disappears after the first login.\n'
            printf '%b======================================================%b\n' "$C_B" "$C_0"
            printf '\n'
        fi
    fi
}

do_install() {
    prefix_paths
    require_root
    require_entware
    check_environment

    if [ -n "$OPFFK_PORT" ]; then
        validate_port "$OPFFK_PORT" || die "invalid --port $OPFFK_PORT" ""
    fi
    if [ -n "$OPFFK_LISTEN_IP" ]; then
        case "$OPFFK_LISTEN_IP" in
            0.0.0.0|127.0.0.1|::1) ;;
            *) validate_ipv4 "$OPFFK_LISTEN_IP" || die "invalid --listen-ip $OPFFK_LISTEN_IP (use a router LAN IPv4 address, 0.0.0.0, or 127.0.0.1)" "" ;;
        esac
    fi

    if [ -n "$STAGE" ]; then
        : # staged already
    fi

    _arch=$(detect_arch "$(uname -m)")
    case "$_arch" in unsupported:*) exit 1 ;; esac  # already reported

    verify_core_binary "$_arch"

    _upgrading=0
    if is_installed; then
        _upgrading=1
        info "existing installation detected — upgrading (configuration is preserved)"
        backup_config
        stop_service
    fi

    if [ "$OPFFK_DRY_RUN" = "1" ]; then
        info "dry-run: environment OK, would install opffk $OPFFK_VERSION (arch $_arch) into $OPFFK_PREFIX"
        return 0
    fi

    ensure_service_user
    install_files "$_arch"
    render_init_overrides

    if [ "$OPFFK_NO_START" = "1" ]; then
        info "--no-start given; start later with: $INIT_SCRIPT start"
        return 0
    fi

    _port="${OPFFK_PORT:-8791}"
    if port_is_busy "$_port" && ! service_pid_alive; then
        die "web UI port $_port is already in use" \
"Choose another port: re-run with --port N (1..65535)."
    fi
    info "starting service"
    service_ctl start
    wait_ready_and_report "$_port"

    printf '\n'
    if [ "$_upgrading" = "1" ]; then
        info "upgrade complete. Open the web UI on http://<router-LAN-IP>:$_port (your existing password still works)."
    else
        info "install complete. Open the web UI:  http://<router-LAN-IP>:$_port"
        info "The UI binds only to the LAN side. It is NOT reachable from the WAN."
        info "Next steps: log in with the password above, review Configuration, Apply & restart, then generate a client link."
    fi
    info "Service lifecycle: $INIT_SCRIPT {start|stop|restart|status}"
    info "Uninstall: sh $(basename "$0") uninstall   (or from the web UI, gated)"
}

# render_init_overrides bakes --listen-ip/--port into the init script.
render_init_overrides() {
    [ "$OPFFK_DRY_RUN" = "1" ] && return 0
    _extra=""
    if [ -n "$OPFFK_LISTEN_IP" ]; then
        _extra="$_extra -listen $OPFFK_LISTEN_IP"
    fi
    if [ -n "$OPFFK_PORT" ]; then
        _extra="$_extra -port $OPFFK_PORT"
    fi
    _extra=${_extra# }
    if [ -n "$_extra" ]; then
        _tmp="$INIT_SCRIPT.tmp"
        sed "s|^OPFFK_WEB_ARGS=.*|OPFFK_WEB_ARGS=\"$_extra\"|" "$INIT_SCRIPT" > "$_tmp" || die "cannot bake UI flags" ""
        # Only rewrite when the marker exists (guards against template drift).
        if grep -q '^OPFFK_WEB_ARGS=' "$INIT_SCRIPT"; then
            mv -f "$_tmp" "$INIT_SCRIPT"
            chmod 0755 "$INIT_SCRIPT"
        else
            rm -f "$_tmp"
            warn "init script lacks OPFFK_WEB_ARGS; listen-ip/port flags not baked in"
        fi
    fi
}

# ---- uninstall ------------------------------------------------------------------

do_uninstall() {
    prefix_paths
    require_root
    if [ ! -x "$WEB_BIN" ]; then
        info "opffk is not installed (no $WEB_BIN); nothing to do"
        return 0
    fi
    info "uninstalling opffk from $OPFFK_PREFIX"
    backup_config
    stop_service

    [ "$OPFFK_DRY_RUN" = "1" ] && { info "dry-run: would remove files"; return 0; }

    rm -f "$INIT_SCRIPT"
    rm -f "$WEB_BIN" "$CORE_BIN" "$CORE_BIN.previous" "$CORE_BIN.new" "$WEB_BIN.new"
    rm -rf "$SHARE_DIR"
    rm -rf "$ETC_DIR"
    rm -f "$RUN_DIR/opffk-web.pid" "$RUN_DIR/opffk-core.pid" "$RUN_DIR/opffk-core.sock"

    info "removed binaries, init script and configuration"
    info "kept for inspection (delete by hand if unwanted):"
    info "  $STATE_DIR   (backups, cookie store)"
    info "  $LOG_DIR     (logs)"
    info "The service account '$SERVICE_USER' in $OPFFK_PREFIX/etc/passwd was left in place; remove its line if you want."
    info "Uninstall complete."
}

# ---- status / reset-password ----------------------------------------------------

do_status() {
    prefix_paths
    if [ ! -x "$WEB_BIN" ]; then
        echo "opffk: not installed"
        return 0
    fi
    echo "opffk: installed"
    echo "  web binary: $WEB_BIN"
    echo "  core binary: $CORE_BIN $([ -f "$SHARE_DIR/VERSION" ] && echo "$(cat "$SHARE_DIR/VERSION")")"
    if service_pid_alive; then
        echo "  service: running (pid $(cat "$RUN_DIR/opffk-web.pid"))"
    else
        echo "  service: stopped"
    fi
    if [ -f "$SHARE_DIR/opffk-VERSION" ]; then
        echo "  opffk release: $(cat "$SHARE_DIR/opffk-VERSION")"
    fi
}

do_reset_password() {
    prefix_paths
    require_root
    if [ ! -x "$WEB_BIN" ]; then
        die "opffk is not installed" ""
    fi
    stop_service
    rm -f "$ETC_DIR/ui.json" "$ETC_DIR/initial-password"
    info "credential cleared; starting service to generate a fresh password"
    service_ctl start
    wait_ready_and_report "${OPFFK_PORT:-8791}"
}

# next_free_uid PASSWD_FILE: the next free uid at or above 65000.
next_free_uid() {
    _file="$1"
    if [ ! -r "$_file" ]; then
        echo 65001
        return 0
    fi
    awk -F: '{ if ($3 ~ /^[0-9]+$/ && $3+0 > m) m = $3+0 } END { print (m >= 65000 ? m+1 : 65001) }' "$_file"
}

# ---- argument parsing -------------------------------------------------------------

usage() {
    if [ -f "$0" ]; then
        sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
    else
        # piped via curl | sh: $0 is not this file
        printf '%s\n' \
"usage: sh install.sh [install|uninstall|status|reset-password] [flags]" \
"flags: --repo USER/NAME  --version V  --base-url URL  --from-dir DIR" \
"       --prefix DIR  --listen-ip IP  --port N  --no-start  --dry-run" \
"see README.md for the full documentation"
    fi
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            install|upgrade) OPFFK_CMD="install" ;;
            uninstall) OPFFK_CMD="uninstall" ;;
            status) OPFFK_CMD="status" ;;
            reset-password) OPFFK_CMD="reset-password" ;;
            --repo) [ $# -ge 2 ] || die "--repo needs a value" ""; OPFFK_REPO="$2"; shift ;;
            --version) [ $# -ge 2 ] || die "--version needs a value" ""; OPFFK_VERSION="$2"; shift ;;
            --base-url) [ $# -ge 2 ] || die "--base-url needs a value" ""; OPFFK_BASE_URL="$2"; shift ;;
            --from-dir) [ $# -ge 2 ] || die "--from-dir needs a value" ""; OPFFK_FROM_DIR="$2"; shift ;;
            --prefix) [ $# -ge 2 ] || die "--prefix needs a value" ""; OPFFK_PREFIX="${2%/}"; shift ;;
            --listen-ip) [ $# -ge 2 ] || die "--listen-ip needs a value" ""; OPFFK_LISTEN_IP="$2"; shift ;;
            --port) [ $# -ge 2 ] || die "--port needs a value" ""; OPFFK_PORT="$2"; shift ;;
            --no-start) OPFFK_NO_START=1 ;;
            --dry-run) OPFFK_DRY_RUN=1 ;;
            -h|--help) usage; exit 0 ;;
            *) die "unknown argument: $1 (see --help)" "" ;;
        esac
        shift
    done
}

main() {
    parse_args "$@"
    case "$OPFFK_CMD" in
        install)
            if [ -n "$OPFFK_FROM_DIR" ]; then
                stage_from_dir "$OPFFK_FROM_DIR"
            else
                stage_from_release
            fi
            do_install
            ;;
        uninstall) do_uninstall ;;
        status) do_status ;;
        reset-password) do_reset_password ;;
    esac
}

# The automated tests source this file to exercise the helper functions
# directly; OPFFK_SOURCE_ONLY=1 skips the main dispatch.
if [ -z "${OPFFK_SOURCE_ONLY:-}" ]; then
    main "$@"
fi
