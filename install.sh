#!/usr/bin/env bash
#
# install.sh — curl-pipeable installer for the Sandbar CLI.
#
#   curl -fsSL <repo>/install.sh | bash
#
# Downloads the release archive for the detected platform, verifies it against
# the goreleaser checksums file, unpacks, and installs a single static binary.
#
# Environment:
#   SANDBAR_OWNER        GitHub owner of the sandbar repository.
#                        Default "aetherbird".
#   SANDBAR_VERSION      Version to install: "latest" (default) or a tag like
#                        "v0.1.0" / "0.1.0".
#   BIN_DIR              Install directory (default: $HOME/.local/bin).
#   SANDBAR_RELEASES_URL Base ".../releases" URL. Default:
#                        https://github.com/$SANDBAR_OWNER/sandbar/releases.
#                        Exists so mirrors and local smoke tests can serve the
#                        same layout without GitHub.
#
# Flags:
#   --dry-run            Print the resolved asset URLs and install path
#                        without touching the network or the filesystem.
#   -h, --help           Show this help.
set -euo pipefail

owner="${SANDBAR_OWNER:-aetherbird}"
version_request="${SANDBAR_VERSION:-latest}"
bin_dir="${BIN_DIR:-$HOME/.local/bin}"
releases_url="${SANDBAR_RELEASES_URL:-https://github.com/${owner}/sandbar/releases}"
checksums_name="sandbar_checksums.txt" # must match checksum.name_template in .goreleaser.yaml

err() { printf 'install.sh: %s\n' "$*" >&2; }
die() { err "$*"; exit 1; }

usage() {
	cat <<'EOF'
Usage: install.sh [--dry-run]

Environment:
  SANDBAR_OWNER        GitHub owner of the sandbar repository
                       (default: "aetherbird")
  SANDBAR_VERSION      Version to install: "latest" (default) or "v0.1.0"
  BIN_DIR              Install directory (default: $HOME/.local/bin)
  SANDBAR_RELEASES_URL Base ".../releases" URL (default:
                       https://github.com/$SANDBAR_OWNER/sandbar/releases)
EOF
}

dry_run=false
for arg in "$@"; do
	case "$arg" in
	--dry-run) dry_run=true ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument: $arg (supported: --dry-run)" ;;
	esac
done

# ── Platform detection ─────────────────────────────────────────────────────────
os="$(uname -s)"
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
FreeBSD) os=freebsd ;;
MINGW* | MSYS* | CYGWIN*)
	die "Windows detected: download the .zip archive from $releases_url and unpack sandbar.exe (scoop bucket planned)"
	;;
*) die "unsupported operating system: $(uname -s)" ;;
esac

arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
i386 | i486 | i586 | i686 | x86 | 386)
	die "32-bit x86 (386) builds are not published; sandbar targets amd64/arm64 only"
	;;
*) die "unsupported architecture: $(uname -m)" ;;
esac

# ── Version → URL resolution ───────────────────────────────────────────────────
# Asset names embed the goreleaser version, which is the tag minus its leading
# "v". "latest" is resolved by following the releases/latest redirect — no
# GitHub API call or token involved.
archive_for() { # $1 = version without leading v
	printf 'sandbar_%s_%s_%s.tar.gz' "$1" "$os" "$arch"
}

if [ "$version_request" != "latest" ]; then
	tag="v${version_request#v}"
	ver="${version_request#v}"
	archive_url="$releases_url/download/$tag/$(archive_for "$ver")"
	checksums_url="$releases_url/download/$tag/$checksums_name"
elif [ "$dry_run" = true ]; then
	# No network in dry-run: the versionless checksums URL is exact, the
	# archive URL shows where the redirect-resolved version lands.
	ver="latest (resolved from the $releases_url/latest redirect when installing)"
	archive_url="$releases_url/latest/download/sandbar_<version>_${os}_${arch}.tar.gz"
	checksums_url="$releases_url/latest/download/$checksums_name"
else
	redirect="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$releases_url/latest")"
	tag="${redirect##*/tag/}"
	case "$tag" in
	v*) ver="${tag#v}" ;;
	*) die "could not resolve the latest release tag from $releases_url/latest (redirected to: $redirect)" ;;
	esac
	archive_url="$releases_url/download/$tag/$(archive_for "$ver")"
	checksums_url="$releases_url/latest/download/$checksums_name"
fi

if [ "$dry_run" = true ]; then
	printf 'os/arch:     %s/%s\n' "$os" "$arch"
	printf 'owner:       %s\n' "$owner"
	printf 'version:     %s\n' "$ver"
	printf 'archive:     %s\n' "$archive_url"
	printf 'checksums:   %s\n' "$checksums_url"
	printf 'install to:  %s/sandbar\n' "$bin_dir"
	exit 0
fi

# ── Download + verify + unpack ─────────────────────────────────────────────────
archive_file="$(archive_for "$ver")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '==> Downloading %s\n' "$archive_url"
curl -fsSL -o "$tmp/$archive_file" "$archive_url"
printf '==> Downloading %s\n' "$checksums_url"
curl -fsSL -o "$tmp/$checksums_name" "$checksums_url"

expected="$(awk -v f="$archive_file" '$2 == f {print $1}' "$tmp/$checksums_name")"
if [ -z "$expected" ]; then
	die "no checksum entry for $archive_file in $checksums_name"
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$tmp/$archive_file" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$tmp/$archive_file" | awk '{print $1}')"
else
	die "need sha256sum (Linux) or shasum (macOS) to verify the download"
fi
if [ "$actual" != "$expected" ]; then
	die "checksum mismatch for $archive_file
  expected $expected
  actual   $actual"
fi

tar -xzf "$tmp/$archive_file" -C "$tmp"
if [ ! -f "$tmp/sandbar" ]; then
	die "archive $archive_file did not contain a top-level sandbar binary"
fi

# ── Install ────────────────────────────────────────────────────────────────────
mkdir -p "$bin_dir"
cp "$tmp/sandbar" "$bin_dir/sandbar"
chmod 0755 "$bin_dir/sandbar"

case ":$PATH:" in
*":$bin_dir:"*) ;;
*)
	printf 'warning: %s is not on your PATH.\n' "$bin_dir" >&2
	printf 'Add it to your shell profile:\n\n  export PATH="%s:$PATH"\n\n' "$bin_dir" >&2
	;;
esac

printf '==> Installed sandbar %s to %s/sandbar\n' "$ver" "$bin_dir"
printf '    Verify with: %s/sandbar --version\n' "$bin_dir"
