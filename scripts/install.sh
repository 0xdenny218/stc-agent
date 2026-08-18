#!/bin/sh
# stc-agent 一键安装：从 GitHub Releases 下载对应系统的包，sha256 校验
# 后安装到 ~/.local/bin（STC_AGENT_INSTALL_DIR 可覆盖；或首个参数指定
# 版本，如 install.sh v0.2.0）。macOS/Linux；Windows 请从 releases 页
# 直接下载 .zip。
#
#   curl -fsSL https://raw.githubusercontent.com/0xdenny218/stc-agent/main/scripts/install.sh | sh
set -eu

repo="0xdenny218/stc-agent"
install_dir="${STC_AGENT_INSTALL_DIR:-$HOME/.local/bin}"
version="${1:-}"

os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) echo "install.sh: unsupported OS: $os (Windows: grab the .zip from https://github.com/$repo/releases)" >&2; exit 1 ;;
esac
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  aarch64 | arm64) arch="arm64" ;;
  *) echo "install.sh: unsupported arch: $arch" >&2; exit 1 ;;
esac

# 版本缺省取最新 release 的 tag（一次 API 调用；限流时提示手动指定）。
if [ -z "$version" ]; then
  version="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
    sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  if [ -z "$version" ]; then
    echo "install.sh: cannot resolve latest release (API rate limit?); pass an explicit version: install.sh v0.2.0" >&2
    exit 1
  fi
fi

asset="stc-agent_${version}_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "==> downloading $asset"
base="https://github.com/$repo/releases/download/$version"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

# sha256 校验（macOS 无 sha256sum，用 shasum）。
want="$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
if [ -z "$want" ]; then
  echo "install.sh: $asset missing from checksums.txt" >&2
  exit 1
fi
if command -v sha256sum > /dev/null 2>&1; then
  got="$(sha256sum "$tmp/$asset" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)"
fi
if [ "$got" != "$want" ]; then
  echo "install.sh: checksum mismatch for $asset (want $want, got $got)" >&2
  exit 1
fi
echo "==> checksum ok"

mkdir -p "$install_dir"
tar -xzf "$tmp/$asset" -C "$tmp"
mv -f "$tmp/stc-agent" "$install_dir/stc-agent"
chmod +x "$install_dir/stc-agent"
echo "==> installed $install_dir/stc-agent ($version, $os/$arch)"

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "==> note: $install_dir is not on your PATH; add it with:
      export PATH=\"$install_dir:\$PATH\"" ;;
esac
