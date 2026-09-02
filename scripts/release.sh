#!/bin/sh
# Builds the release binaries into ./dist:
#   rota-darwin-arm64  rota-darwin-amd64  rota-linux-arm64  rota-linux-amd64
#   rota-windows-arm64.exe  rota-windows-amd64.exe  checksums.txt
# install.sh fetches the darwin and linux ones; Windows takes the .exe.
#
# Run from the repo root:  sh scripts/release.sh
# Then attach every file in dist/ to the GitHub release for tag v<version>.
set -eu

cd "$(dirname "$0")/.."
version=$(go run ./cmd/rota --version 2>/dev/null | awk '{print $2}') || version=dev
echo "building rota ${version} into dist/" >&2

rm -rf dist
mkdir -p dist
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/arm64 windows/amd64; do
	os=${target%/*}
	arch=${target#*/}
	out="dist/rota-${os}-${arch}"
	[ "$os" = windows ] && out="$out.exe"
	echo "  ${os}/${arch}" >&2
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -trimpath -ldflags='-s -w' -o "$out" ./cmd/rota
done

cd dist
if command -v shasum >/dev/null 2>&1; then
	shasum -a 256 rota-* >checksums.txt
else
	sha256sum rota-* >checksums.txt
fi
cat checksums.txt >&2
