#!/bin/sh
# Installs the code flower launcher from its latest GitHub release:
#
#   curl -fsSL https://raw.githubusercontent.com/lutrarutra/codeflower-launcher/main/install.sh | sh
#
# CODEFLOWER_VERSION=v0.1.0 installs that release instead, and
# CODEFLOWER_INSTALL_DIR chooses the folder (default ~/.local/bin).
set -eu

repo=lutrarutra/codeflower-launcher
version=${CODEFLOWER_VERSION:-latest}
directory=${CODEFLOWER_INSTALL_DIR:-$HOME/.local/bin}

fail() {
  echo "codeflower install: $*" >&2
  exit 1
}

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "this script supports Linux and macOS; on Windows, download codeflower-windows-amd64.zip from https://github.com/$repo/releases" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "no build for $(uname -m)" ;;
esac
# A shell under Rosetta reports x86_64 on Apple silicon; the native build is faster.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ]; then
  arch=arm64
fi

if [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/$version"
fi
archive="codeflower-$os-$arch.tar.gz"

if command -v curl >/dev/null 2>&1; then
  download() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  download() { wget -q -O "$2" "$1"; }
else
  fail "needs curl or wget"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

echo "Downloading $archive ($version)…"
download "$base/$archive" "$work/$archive" || fail "could not download $base/$archive"
download "$base/SHA256SUMS" "$work/SHA256SUMS" || fail "could not download the checksums"

expected=$(awk -v name="$archive" '$2 == name { print $1 }' "$work/SHA256SUMS")
[ -n "$expected" ] || fail "$archive is not in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$work/$archive" | awk '{ print $1 }')
else
  actual=$(shasum -a 256 "$work/$archive" | awk '{ print $1 }')
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $archive"

tar -xzf "$work/$archive" -C "$work" codeflower
mkdir -p "$directory"
# Replaced in one step, so a running codeflower keeps its file.
mv -f "$work/codeflower" "$directory/codeflower"
chmod 755 "$directory/codeflower"

echo "Installed $("$directory/codeflower" --version) in $directory"
case ":$PATH:" in
  *":$directory:"*) ;;
  *)
    echo
    echo "$directory is not on your PATH. Add it, then open a new terminal:"
    case $(basename "${SHELL:-}") in
      zsh) rc='~/.zshrc' ;;
      bash) rc='~/.bashrc' ;;
      *) rc='~/.profile' ;;
    esac
    echo "  echo 'export PATH=\"$directory:\$PATH\"' >> $rc"
    ;;
esac
echo
echo "Next: codeflower check"
