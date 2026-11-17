#!/usr/bin/env bash
# 构建全部平台的发布包，产物在 dist/。
# 用法: ./scripts/build-release.sh [版本号]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo 0.1.1)}"
VERSION="${VERSION#v}"
OUT="$ROOT/dist"

rm -rf "$OUT"
mkdir -p "$OUT"

targets=(
  linux/amd64
  linux/arm64
  linux/386
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
  windows/386
  freebsd/amd64
)

echo "vshell-cli $VERSION"
for t in "${targets[@]}"; do
  goos="${t%/*}"
  goarch="${t#*/}"
  ext=""
  [ "$goos" = "windows" ] && ext=".exe"
  bin="vshell-cli_${VERSION}_${goos}_${goarch}${ext}"

  echo "==> $bin"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/$bin" .

  if [ "$goos" = "windows" ]; then
    (cd "$OUT" && zip -q "${bin%.exe}.zip" "$bin")
  else
    (cd "$OUT" && tar -czf "${bin}.tar.gz" "$bin")
  fi
done

# 只保留压缩包和校验和
for f in "$OUT"/vshell-cli_"${VERSION}"_*; do
  case "$f" in
    *.tar.gz|*.zip) ;;
    *) rm -f "$f" ;;
  esac
done

cd "$OUT"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum ./*.tar.gz ./*.zip > SHA256SUMS.txt
else
  shasum -a 256 ./*.tar.gz ./*.zip > SHA256SUMS.txt
fi

echo
echo "完成，产物在 $OUT"
ls -lh ./*.tar.gz ./*.zip SHA256SUMS.txt
