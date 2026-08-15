#!/usr/bin/env bash
# Bootstraps the Go toolchain into .toolchain/ without root.
# Prefers China-accessible mirrors (aliyun / google.cn), verifies the tarball
# against the official checksum manifest when reachable.
#
# Usage: scripts/bootstrap-toolchain.sh [go-version]   (default: go1.26.6)
set -euo pipefail

VERSION="${1:-go1.26.6}"

case "$(uname -s)" in
  Linux)  GOOS=linux ;;
  Darwin) GOOS=darwin ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) GOARCH=amd64 ;;
  arm64|aarch64) GOARCH=arm64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

cd "$(dirname "$0")/.."
mkdir -p .toolchain
cd .toolchain

TARBALL="${VERSION}.${GOOS}-${GOARCH}.tar.gz"
for base in \
  "https://mirrors.aliyun.com/golang" \
  "https://golang.google.cn/dl" \
  "https://go.dev/dl"; do
  url="${base}/${TARBALL}"
  echo "trying ${url} ..."
  if curl -fsSL -m 600 -o go.tar.gz "${url}"; then
    break
  fi
done
if [ ! -s go.tar.gz ]; then
  echo "failed to download the Go toolchain" >&2
  exit 1
fi

# Verify against the official manifest when reachable (best effort).
SHA=$(curl -fsSL -m 15 "https://golang.google.cn/dl/?mode=json&include=all" 2>/dev/null \
  | python3 -c "import json,sys; [print(f['sha256']) for r in json.load(sys.stdin) for f in r['files'] if f['filename']=='${TARBALL}']" 2>/dev/null || true)
if [ -n "$SHA" ]; then
  echo "$SHA  go.tar.gz" | sha256sum -c - >/dev/null || { echo "checksum MISMATCH, aborting" >&2; exit 1; }
  echo "checksum verified"
else
  echo "WARNING: checksum manifest unreachable, skipping verification"
fi

tar -xzf go.tar.gz
rm -f go.tar.gz

cat > env.sh <<'EOF'
# Source this file to use the bundled Go toolchain.
_TC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export GOROOT="$_TC_DIR/go"
export PATH="$_TC_DIR/go/bin:$PATH"
export GOCACHE="$_TC_DIR/gocache"
export GOPATH="$_TC_DIR/gopath"
export GOPROXY="https://goproxy.cn"
export GOSUMDB="sum.golang.google.cn"
export GOFLAGS="-mod=mod"
unset _TC_DIR
EOF

./go/bin/go version
echo "toolchain ready under .toolchain/ — source .toolchain/env.sh to use it"
