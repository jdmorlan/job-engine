#!/bin/sh
# je installer.
#
#   curl -fsSL https://raw.githubusercontent.com/jdmorlan/job-engine/main/install.sh | sh
#
# Downloads the release build for this machine, checks it against the published
# SHA-256, and puts it somewhere on your PATH. Nothing else: nothing is
# started, nothing is added to your shell config, and no directory outside the
# install location is touched. Starting the engine is a separate, explicit
# step -- `je quickstart` to try it, `docker compose up -d` to keep it running.
#
# Environment:
#   JE_VERSION      release tag to install (default: latest)
#   JE_INSTALL_DIR  where to put the binary (default: ~/.local/bin, or
#                   /usr/local/bin when running as root)
#   JE_REPO         GitHub repository (default: jdmorlan/job-engine)
#   JE_API_BASE     releases API host (default: https://api.github.com)
#   JE_DOWNLOAD_BASE  where release assets live, for mirrors and air-gapped
#                     installs (default: the repo's GitHub releases)

set -eu

REPO="${JE_REPO:-jdmorlan/job-engine}"
VERSION="${JE_VERSION:-latest}"
API_BASE="${JE_API_BASE:-https://api.github.com}"

say()  { printf '%s\n' "$*"; }
fail() { printf 'install: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || fail "$1 is required but not installed"
}

# --- where does it go -------------------------------------------------------
# Default to a directory the user owns, so the common path needs no sudo. Root
# gets the conventional system location, since that is presumably deliberate.
if [ -n "${JE_INSTALL_DIR:-}" ]; then
    INSTALL_DIR="$JE_INSTALL_DIR"
elif [ "$(id -u)" = "0" ]; then
    INSTALL_DIR="/usr/local/bin"
else
    INSTALL_DIR="$HOME/.local/bin"
fi

# --- what are we running on -------------------------------------------------
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    darwin|linux) ;;
    *) fail "unsupported operating system: $OS" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *) fail "unsupported architecture: $ARCH" ;;
esac

need curl
need tar
need mktemp

# --- resolve the version ----------------------------------------------------
if [ "$VERSION" = "latest" ]; then
    say "looking up the latest release of $REPO"
    # Deliberately no jq: this script runs on machines we know nothing about,
    # and requiring a JSON parser to install a static binary is exactly the
    # friction it exists to avoid.
    VERSION="$(curl -fsSL "$API_BASE/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n 1)"
    [ -n "$VERSION" ] || fail "could not determine the latest release of $REPO"
fi

BARE="${VERSION#v}"
ASSET="je_${BARE}_${OS}_${ARCH}.tar.gz"
BASE="${JE_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download/$VERSION}"

say "installing je $VERSION for $OS/$ARCH"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

# --- download and verify ----------------------------------------------------
curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET" \
    || fail "no build published for $OS/$ARCH in $VERSION"
curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt" \
    || fail "release $VERSION publishes no checksums; refusing to install unverified"

EXPECTED="$(grep " \*\{0,1\}$ASSET\$" "$TMP/checksums.txt" | awk '{print $1}')"
[ -n "$EXPECTED" ] || fail "checksums.txt does not list $ASSET"

if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$TMP/$ASSET" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL="$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')"
else
    fail "need sha256sum or shasum to verify the download"
fi

# This downloads an executable and is about to put it on your PATH. A mismatch
# is never worth continuing through.
[ "$EXPECTED" = "$ACTUAL" ] || fail "checksum mismatch
  expected $EXPECTED
  got      $ACTUAL"

say "verified sha256 $(printf '%s' "$ACTUAL" | cut -c1-16)"

# --- install ----------------------------------------------------------------
tar -xzf "$TMP/$ASSET" -C "$TMP" je || fail "could not extract je from $ASSET"

mkdir -p "$INSTALL_DIR" || fail "could not create $INSTALL_DIR"
# Written alongside and moved into place, so an interrupted install never
# leaves a half-written binary on your PATH.
cp "$TMP/je" "$INSTALL_DIR/.je.new" || fail "could not write to $INSTALL_DIR
  Try:  JE_INSTALL_DIR=\$HOME/.local/bin sh install.sh"
chmod 755 "$INSTALL_DIR/.je.new"
mv "$INSTALL_DIR/.je.new" "$INSTALL_DIR/je"

say ""
say "installed je $VERSION to $INSTALL_DIR/je"

# --- is it usable -----------------------------------------------------------
case ":$PATH:" in
    *":$INSTALL_DIR:"*)
        say ""
        say "Try:  je demo"
        ;;
    *)
        # Saying this is the difference between a tool that works and a tool
        # that appears not to have installed.
        say ""
        say "$INSTALL_DIR is not on your PATH. Add it:"
        say ""
        say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc"
        say "  exec \$SHELL"
        say ""
        say "Or run it directly:  $INSTALL_DIR/je demo"
        ;;
esac
