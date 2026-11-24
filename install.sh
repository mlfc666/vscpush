#!/bin/sh
# vscpush 安装脚本：自动识别平台，从 GitHub Release 下载对应二进制。
#
#   curl -fsSL https://raw.githubusercontent.com/mlfc666/vscpush/main/install.sh | sh
#
# 可选环境变量：
#   VSCPUSH_VERSION  指定版本（默认取最新 Release），例如 v0.4.0
#   VSCPUSH_BIN_DIR  安装目录（默认 /usr/local/bin，不可写时回退 ~/.local/bin）
set -eu

REPO="mlfc666/vscpush"
BIN="vscpush"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m警告:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "需要 $1，请先安装"
}
need uname
need tar
need curl

# ---- 识别平台 ----
os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "不支持的系统: $os（Windows 请直接下载 .zip）" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
i386 | i486 | i586 | i686) arch=386 ;;
armv7l | armv7) arch=armv7 ;;
armv6l | armv6) arch=armv6 ;;
*) die "不支持的架构: $arch" ;;
esac

# ---- 解析版本 ----
version="${VSCPUSH_VERSION:-}"
if [ -z "$version" ]; then
	info "查询最新版本…"
	# 优先用 API；匿名访问可能被限流（403），此时回退到 releases/latest 的重定向地址
	version=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
	if [ -z "$version" ]; then
		version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" 2>/dev/null |
			sed -n 's#.*/tag/\(.*\)$#\1#p')
	fi
	[ -n "$version" ] || die "无法获取最新版本（GitHub API 可能被限流），请用 VSCPUSH_VERSION 指定，例如 VSCPUSH_VERSION=v0.4.0"
fi
ver_no_v=$(printf '%s' "$version" | sed 's/^v//')

# armv6/armv7 的产物名是 linux_armv6 / linux_armv7，其余是 os_arch
case "$arch" in
armv6 | armv7) asset="${BIN}_${ver_no_v}_${os}_${arch}.tar.gz" ;;
*) asset="${BIN}_${ver_no_v}_${os}_${arch}.tar.gz" ;;
esac

url="https://github.com/${REPO}/releases/download/${version}/${asset}"
info "下载 ${asset}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

curl -fsSL "$url" -o "$tmp/$asset" || die "下载失败: $url"

# 有 checksums.txt 就校验
if curl -fsSL "https://github.com/${REPO}/releases/download/${version}/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$tmp" && grep " ${asset}\$" checksums.txt | sha256sum -c -) || die "校验和不匹配"
		info "sha256 校验通过"
	elif command -v shasum >/dev/null 2>&1; then
		expected=$(grep " ${asset}\$" "$tmp/checksums.txt" | awk '{print $1}')
		actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
		[ "$expected" = "$actual" ] || die "校验和不匹配"
		info "sha256 校验通过"
	fi
fi

tar -xzf "$tmp/$asset" -C "$tmp"
# 压缩包内是 <name>/vscpush 结构，用 find 兜底，避免依赖具体目录名
binpath=$(find "$tmp" -type f -name "$BIN" -perm -u+x | head -1)
[ -n "$binpath" ] || binpath=$(find "$tmp" -type f -name "$BIN" | head -1)
[ -n "$binpath" ] || die "压缩包里没有 $BIN"

# ---- 选择安装目录 ----
dir="${VSCPUSH_BIN_DIR:-/usr/local/bin}"
# 目录可能还不存在：先尝试创建，再判断可写性
if [ ! -d "$dir" ]; then
	mkdir -p "$dir" 2>/dev/null || true
fi
if [ ! -w "$dir" ]; then
	if [ "$(id -u)" = "0" ]; then
		mkdir -p "$dir"
	else
		warn "$dir 不可写，改装到 ~/.local/bin"
		dir="$HOME/.local/bin"
		mkdir -p "$dir"
	fi
fi

install -m 0755 "$binpath" "$dir/$BIN"
info "已安装到 $dir/$BIN"

case ":$PATH:" in
*":$dir:"*) ;;
*) warn "$dir 不在 PATH 中，请自行加入" ;;
esac

"$dir/$BIN" -version
