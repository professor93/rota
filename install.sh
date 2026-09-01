#!/bin/sh
# rota installer — https://github.com/professor93/rota
#
#   curl -fsSL https://raw.githubusercontent.com/professor93/rota/main/install.sh | bash
#
# Downloads the release binary for this machine, verifies its checksum, and
# installs it as `rota` on your PATH (/usr/local/bin, or ~/.local/bin when
# /usr/local/bin is not writable and sudo is unavailable).
#
# Options via environment:
#   ROTA_VERSION      install a specific version, "1.0.0" or "v1.0.0" alike
#                     (default: the latest release)
#   ROTA_INSTALL_DIR  install somewhere else (skips the sudo fallback)
#
# The whole script is a function called on the last line, so a download cut
# off mid-stream runs nothing rather than half of something.
set -eu

main() {
	REPO="professor93/rota"

	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "unsupported OS $(uname -s): rota releases cover macOS and Linux" ;;
	esac
	case "$(uname -m)" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) fail "unsupported architecture $(uname -m)" ;;
	esac

	version="${ROTA_VERSION:-}"
	version="${version#v}" # v1.0.0 and 1.0.0 both work
	if [ -z "$version" ]; then
		version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
			grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/') ||
			fail "could not determine the latest version; set ROTA_VERSION and retry"
	fi
	[ -n "$version" ] || fail "could not determine the latest version"

	asset="rota-${os}-${arch}"
	base="https://github.com/$REPO/releases/download/v${version}"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	say "downloading rota ${version} (${os}/${arch})..."
	curl -fsSL -o "$tmp/rota" "$base/$asset" || fail "download failed: $base/$asset"
	curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "checksums download failed"

	want=$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
	[ -n "$want" ] || fail "no checksum recorded for $asset"
	if command -v shasum >/dev/null 2>&1; then
		got=$(shasum -a 256 "$tmp/rota" | cut -d' ' -f1)
	else
		got=$(sha256sum "$tmp/rota" | cut -d' ' -f1)
	fi
	[ "$want" = "$got" ] || fail "checksum mismatch: expected $want, got $got"
	chmod 0755 "$tmp/rota"

	dir="${ROTA_INSTALL_DIR:-}"
	if [ -n "$dir" ]; then
		mkdir -p "$dir"
		mv "$tmp/rota" "$dir/rota" || fail "could not write to $dir"
	elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
		mv "$tmp/rota" "$dir/rota"
	elif command -v sudo >/dev/null 2>&1; then
		dir=/usr/local/bin
		say "installing to /usr/local/bin (sudo may prompt for your password)"
		sudo mkdir -p "$dir" && sudo mv "$tmp/rota" "$dir/rota" ||
			fail "could not write to /usr/local/bin"
	else
		dir="$HOME/.local/bin"
		mkdir -p "$dir"
		mv "$tmp/rota" "$dir/rota"
	fi

	say "installed: $dir/rota"

	# An older rota elsewhere on PATH would silently shadow this one.
	existing=$(command -v rota 2>/dev/null || true)
	case "$existing" in
	"" | "$dir/rota") : ;;
	*) say "note: your shell currently resolves rota to $existing"
		say "      remove it, or put $dir earlier in PATH, or the old one keeps answering" ;;
	esac

	case ":$PATH:" in
	*":$dir:"*) "$dir/rota" --version >&2 ;;
	*) say "note: $dir is not on your PATH; add it, e.g."
		say "  export PATH=\"$dir:\$PATH\"" ;;
	esac
}

say() { printf '%s\n' "$*" >&2; }
fail() {
	say "install failed: $*"
	exit 1
}

main "$@"
