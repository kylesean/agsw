#!/bin/sh
# agsw installer
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/kylesean/agsw/main/install.sh | sh
#
# Custom options:
#   AGSW_VERSION=v0.3.0 BINDIR=/usr/local/bin curl -fsSL https://raw.githubusercontent.com/kylesean/agsw/main/install.sh | sh

set -eu

REPO="kylesean/agsw"

# Colors for terminal output
if [ -t 1 ]; then
  BOLD="$(printf '\033[1m')"
  GREEN="$(printf '\033[32m')"
  BLUE="$(printf '\033[34m')"
  YELLOW="$(printf '\033[33m')"
  RED="$(printf '\033[31m')"
  RESET="$(printf '\033[0m')"
else
  BOLD=""
  GREEN=""
  BLUE=""
  YELLOW=""
  RED=""
  RESET=""
fi

info() {
  printf "${BLUE}${BOLD}[+]${RESET} %s\n" "$1"
}

success() {
  printf "${GREEN}${BOLD}[✓]${RESET} %s\n" "$1"
}

warn() {
  printf "${YELLOW}${BOLD}[!]${RESET} %s\n" "$1"
}

err() {
  printf "${RED}${BOLD}[x]${RESET} %s\n" "$1" >&2
  exit 1
}

# 1. Detect OS
OS_RAW="$(uname -s)"
case "$OS_RAW" in
  Linux*)  OS="linux" ;;
  Darwin*) OS="darwin" ;;
  *) err "不支持的操作系统: $OS_RAW。Windows 用户请前往 GitHub Releases 下载 zip 包。" ;;
esac

# 2. Detect Architecture
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) err "不支持的系统架构: $ARCH_RAW" ;;
esac

info "检测到运行环境: ${OS}/${ARCH}"

# 3. Resolve Version
VERSION="${AGSW_VERSION:-}"
if [ -z "$VERSION" ]; then
  info "正在获取最新版本号..."
  # Try redirect location first (bypasses GitHub API rate limit)
  TAG="$(curl -fsSI "https://github.com/${REPO}/releases/latest" 2>/dev/null | tr -d '\r' | awk -F'/' '/[Ll]ocation:/{print $NF}' || true)"
  if [ -n "$TAG" ]; then
    VERSION="$TAG"
  else
    # Fallback to GitHub API
    TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)"
    if [ -n "$TAG" ]; then
      VERSION="$TAG"
    else
      err "获取最新版本号失败，请检查网络或通过环境变量指定版本: AGSW_VERSION=v0.3.0"
    fi
  fi
fi

# Strip leading 'v' for archive naming (e.g. v0.3.0 -> 0.3.0)
CLEAN_VER="${VERSION#v}"
ARCHIVE_NAME="agsw_${CLEAN_VER}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

# 4. Resolve Target Directory
if [ -n "${BINDIR:-}" ]; then
  INSTALL_DIR="$BINDIR"
elif [ -w "/usr/local/bin" ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="${HOME}/.local/bin"
fi

mkdir -p "$INSTALL_DIR" || err "创建安装目录失败: $INSTALL_DIR"

info "准备安装版本 ${BOLD}${VERSION}${RESET} 到 ${BOLD}${INSTALL_DIR}${RESET}..."

# 5. Download and Verify
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

info "下载 $ARCHIVE_NAME ..."
curl -fL --progress-bar "$DOWNLOAD_URL" -o "${TMP_DIR}/${ARCHIVE_NAME}" || err "下载失败: $DOWNLOAD_URL"

# Checksum verification if tool available
if curl -fsSL "$CHECKSUMS_URL" -o "${TMP_DIR}/checksums.txt" 2>/dev/null; then
  info "校验 SHA256 完整性..."
  EXPECTED_SHA="$(grep "${ARCHIVE_NAME}" "${TMP_DIR}/checksums.txt" | awk '{print $1}')"
  if [ -n "$EXPECTED_SHA" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      ACTUAL_SHA="$(sha256sum "${TMP_DIR}/${ARCHIVE_NAME}" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      ACTUAL_SHA="$(shasum -a 256 "${TMP_DIR}/${ARCHIVE_NAME}" | awk '{print $1}')"
    else
      ACTUAL_SHA=""
    fi
    if [ -n "$ACTUAL_SHA" ]; then
      if [ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]; then
        err "校验码不匹配！\n期望值: $EXPECTED_SHA\n实际值: $ACTUAL_SHA"
      fi
      success "校验通过"
    fi
  fi
fi

# 6. Extract and Install
tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "$TMP_DIR" agsw || err "解压失败"
mv "${TMP_DIR}/agsw" "${INSTALL_DIR}/agsw"
chmod +x "${INSTALL_DIR}/agsw"

success "安装成功: ${INSTALL_DIR}/agsw"

# 7. Check PATH
case ":${PATH}:" in
  *:"${INSTALL_DIR}":*) ;;
  *)
    warn "注意: ${INSTALL_DIR} 尚未包含在你的 PATH 环境变量中。"
    echo "  请将以下配置追加到你的终端配置文件（~/.bashrc 或 ~/.zshrc）："
    echo "    export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac

echo ""
printf "${GREEN}${BOLD}agsw ${VERSION} 已就绪！${RESET}\n"
echo "快速开始:"
echo "  agsw        # 启动 Gateway 并拉起 agy"
echo "  agsw list   # 查看当前账号池状态"
echo "  agsw usage  # 实时查询账号真实额度"
echo "  agsw -h     # 查看完整帮助"
