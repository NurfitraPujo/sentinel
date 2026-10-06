/**
 * Distribution of the `sentinel` CLI from the dashboard itself (GET /install.sh, /cli/download/<os>/<arch>,
 * /cli/checksums.txt). The binaries are cross-compiled in apps/dashboard-web/Dockerfile's `cli` stage and
 * copied to /app/cli (override with SENTINEL_CLI_DIR). These routes are deliberately unauthenticated — an
 * engineer needs them BEFORE they have a key — so everything here is allow-listed and nothing is derived
 * from request input except the (validated) public origin.
 */
import path from 'node:path';

export const CLI_PLATFORMS = [
	{ os: 'linux', arch: 'amd64' },
	{ os: 'linux', arch: 'arm64' },
	{ os: 'darwin', arch: 'amd64' },
	{ os: 'darwin', arch: 'arm64' },
] as const;

/** Name of the binary file for a platform inside the CLI dir (`sentinel-<os>-<arch>`). */
export function cliBinaryName(os: string, arch: string): string {
	return `sentinel-${os}-${arch}`;
}

/** Allow-list lookup: only the four exact pairs resolve; anything else (incl. `..`) is null. */
export function resolvePlatform(os: string, arch: string): { os: string; arch: string } | null {
	return CLI_PLATFORMS.find((p) => p.os === os && p.arch === arch) ?? null;
}

export function cliDir(): string {
	return process.env.SENTINEL_CLI_DIR || path.resolve(process.cwd(), 'cli');
}

export const CLI_MISSING_MESSAGE =
	'The sentinel CLI binaries are not present in this build (looked in ' +
	'$SENTINEL_CLI_DIR, default ./cli). They are produced by the `cli` stage of apps/dashboard-web/Dockerfile: ' +
	'`podman build -f apps/dashboard-web/Dockerfile --build-arg SENTINEL_VERSION=<tag> .` from the repo root, ' +
	'or build them by hand: `cd tools/sentinel && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ' +
	'<SENTINEL_CLI_DIR>/sentinel-linux-amd64 .` plus a `checksums.txt` (sha256sum format).\n';

/**
 * The public origin to embed in install.sh. Prefers the operator-configured AUTH_URL (the documented,
 * externally-visible dashboard URL) so an arbitrary Host/X-Forwarded-* header can never redirect a
 * `curl | sh` to another server; falls back to the request origin (adapter-node resolves it from ORIGIN /
 * the configured protocol+host headers, same as every other absolute URL in the app) when unset.
 * Returns null when the result is not a plain http(s) origin that is safe to put inside a shell script.
 */
export function publicOrigin(requestOrigin: string, authUrl: string | undefined = process.env.AUTH_URL): string | null {
	for (const candidate of [authUrl, requestOrigin]) {
		if (!candidate) continue;
		let u: URL;
		try {
			u = new URL(candidate);
		} catch {
			continue;
		}
		if (u.protocol !== 'http:' && u.protocol !== 'https:') continue;
		const origin = u.origin;
		if (/^https?:\/\/[A-Za-z0-9.\-:[\]]+$/.test(origin)) return origin;
	}
	return null;
}

/**
 * Name of the CLI config profile this deployment logs in as (helm `config.dashboard.cliProfile` →
 * SENTINEL_CLI_PROFILE). Same rule as the CLI's profile names; anything else is treated as unset so it can
 * never inject into the generated script. Empty string = no --profile in the printed commands.
 */
export function cliProfileName(raw: string | undefined = process.env.SENTINEL_CLI_PROFILE): string {
	return raw && /^[a-z0-9][a-z0-9_-]{0,31}$/.test(raw) ? raw : '';
}

/** The `sentinel login` command line the installer / Agents page prints for `origin`. */
export function loginCommand(origin: string, profile: string): string {
	return `sentinel login --url ${origin}${profile ? ` --profile ${profile}` : ''}`;
}

/** POSIX sh installer. `origin` must come from publicOrigin() (shell-safe by construction). */
export function renderInstallScript(origin: string, profile: string = ''): string {
	const login = loginCommand('$SENTINEL_ORIGIN', profile);
	return `#!/bin/sh
# Sentinel CLI installer, served by ${origin}
#   curl -fsSL ${origin}/install.sh | sh
# Env: SENTINEL_INSTALL_DIR (default $HOME/.local/bin). No sudo. Downloads only from ${origin}.
set -eu

# Everything runs inside main() and main is invoked on the LAST line, so a truncated curl-pipe-sh
# download (which cuts the script mid-function) executes nothing.
main() {
  SENTINEL_ORIGIN='${origin}'
  INSTALL_DIR="\${SENTINEL_INSTALL_DIR:-\$HOME/.local/bin}"

  say() { printf '%s\\n' "$*"; }
  die() { printf 'install.sh: error: %s\\n' "$*" >&2; exit 1; }

  # --- platform ---------------------------------------------------------------------------------------
  case "$(uname -s)" in
    Linux) OS=linux ;;
    Darwin) OS=darwin ;;
    *) die "unsupported OS '$(uname -s)' (supported: Linux, Darwin)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported architecture '$(uname -m)' (supported: x86_64/amd64, aarch64/arm64)" ;;
  esac

  # --- tools ------------------------------------------------------------------------------------------
  if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1" -o "$2"; }
  elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
  else
    die "need curl or wget"
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    sha256() { sha256sum "$1" | cut -d' ' -f1; }
  elif command -v shasum >/dev/null 2>&1; then
    sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
  else
    die "need sha256sum or shasum to verify the download"
  fi

  TMP="$(mktemp -d "\${TMPDIR:-/tmp}/sentinel-install.XXXXXX")"
  # Also drops the half-copied target if cp/mv fails (set -e exits through the EXIT trap).
  trap 'rm -rf "$TMP"; rm -f "$INSTALL_DIR/.sentinel.new.$$"' EXIT INT TERM

  NAME="sentinel-$OS-$ARCH"
  say "Downloading $NAME from $SENTINEL_ORIGIN ..."
  fetch "$SENTINEL_ORIGIN/cli/download/$OS/$ARCH" "$TMP/sentinel" || die "download failed (is $SENTINEL_ORIGIN reachable, and does it serve the CLI?)"
  fetch "$SENTINEL_ORIGIN/cli/checksums.txt" "$TMP/checksums.txt" || die "could not download checksums.txt"

  # --- verify -----------------------------------------------------------------------------------------
  WANT="$(awk -v n="$NAME" '$2 == n || $2 == "*" n { print $1; exit }' "$TMP/checksums.txt")"
  [ -n "$WANT" ] || die "no checksum for $NAME in checksums.txt"
  GOT="$(sha256 "$TMP/sentinel")"
  if [ "$WANT" != "$GOT" ]; then
    die "checksum mismatch for $NAME (expected $WANT, got $GOT) — refusing to install"
  fi
  say "sha256 ok ($GOT)"

  # --- install (atomic: copy next to the target, then rename) -----------------------------------------
  mkdir -p "$INSTALL_DIR"
  chmod 0755 "$TMP/sentinel"
  cp "$TMP/sentinel" "$INSTALL_DIR/.sentinel.new.$$"
  mv -f "$INSTALL_DIR/.sentinel.new.$$" "$INSTALL_DIR/sentinel"
  say "Installed $INSTALL_DIR/sentinel"

  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) say ""; say "warning: $INSTALL_DIR is not on your PATH. Add it, e.g.:"; say "  export PATH=\\"$INSTALL_DIR:\\$PATH\\"" ;;
  esac
  if OLD="$(command -v sentinel-cli 2>/dev/null)"; then
    say ""
    say "warning: an old 'sentinel-cli' is on your PATH ($OLD). The binary is now named 'sentinel'; remove the old one when convenient (not touched)."
  fi

  say ""
  "$INSTALL_DIR/sentinel" version
  say ""
  say "Next steps:"
  say "  ${login}    # paste your agent key when prompted"
  say "  sentinel agent install    # give Claude Code / Codex / agy the sentinel-agent skill"
}

main "$@"
`;
}
