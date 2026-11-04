#!/bin/sh
# The line between the two modules: the root, github.com/professor93/rota,
# which holds every package and requires nothing, and the program,
# github.com/professor93/rota/cmd/rota, which requires the root at a release.
#
#   sh scripts/module.sh check    the line holds; prints "module: ok"
#
# Works from any directory. A broken rule is one line on stderr and exit 1.
set -eu

cd "$(dirname "$0")/.."
root=$(pwd -P)

root_module=github.com/professor93/rota
app_module=$root_module/cmd/rota
release='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'

usage() {
	echo "usage: sh scripts/module.sh check | tidy | pin vX.Y.Z[-pre]" >&2
	exit 2
}

fail() {
	echo "module: $*" >&2
	exit 1
}

# pinned prints the version of the root that cmd/rota/go.mod requires.
pinned() {
	go mod edit -print cmd/rota/go.mod 2>/dev/null | awk -v m="$root_module" '
		$1 == "require" && $2 == m { print $3; exit }
		$1 == "require" && $2 == "(" { block = 1; next }
		block && $1 == ")" { block = 0; next }
		block && $1 == m { print $2; exit }
	'
}

# version prints wire.Version, the program's release number.
version() {
	sed -n 's/^const Version = "\(.*\)"$/\1/p' wire/wire.go
}

check() {
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	# 1. The root requires and replaces nothing.
	json=$(go mod edit -json go.mod) || fail "go.mod cannot be read"
	case $json in
	*'"Require"'* | *'"Replace"'*)
		fail "go.mod requires or replaces a module; the root requires nothing" ;;
	esac

	# 2. The program is its own module, pinned to a release of the root.
	json=$(go mod edit -json cmd/rota/go.mod 2>/dev/null) || json=
	path=$(printf '%s\n' "$json" | awk '
		/"Module": \{/ { in_module = 1; next }
		in_module && /"Path":/ { sub(/^[^:]*: *"/, ""); sub(/",?$/, ""); print; exit }
		in_module && /\}/ { exit }
	')
	[ "$path" = "$app_module" ] ||
		fail "cmd/rota/go.mod is not the module $app_module"
	case $json in
	*'"Replace"'*) fail "cmd/rota/go.mod replaces a module; it requires the root at a release" ;;
	esac
	v=$(pinned)
	printf '%s\n' "$v" | grep -Eq "$release" ||
		fail "cmd/rota/go.mod requires $root_module at '$v', which is not a release tag"

	# 3. The workspace only lists the modules; it replaces nothing.
	json=$(go work edit -json go.work) || fail "go.work cannot be read"
	case $json in
	*'"Replace"'*) fail "go.work replaces a module" ;;
	esac

	# 4. A program importing every public package of the root needs no
	# other module, so nothing is fetched and its go.sum stays empty.
	mkdir "$tmp/consumer"
	pkgs=$(GOWORK=off go list -e ./... 2>"$tmp/list.err" | grep -v '/internal/') || true
	if [ -z "$pkgs" ]; then
		echo "module: a program importing the root's packages needs other modules" >&2
		cat "$tmp/list.err" >&2
		exit 1
	fi
	{
		echo "package main"
		echo
		echo "import ("
		for p in $pkgs; do printf '\t_ "%s"\n' "$p"; done
		echo ")"
		echo
		echo "func main() {}"
	} >"$tmp/consumer/main.go"
	if ! (
		cd "$tmp/consumer" &&
			export GOWORK=off GOPROXY=off GOFLAGS= GOTOOLCHAIN=local &&
			go mod init example.com/consumer &&
			go mod edit -require="$root_module@v0.0.0" -replace="$root_module=$root" &&
			go mod tidy &&
			go build -o /dev/null . &&
			[ ! -s go.sum ]
	) >"$tmp/consumer.out" 2>&1; then
		echo "module: a program importing the root's packages needs other modules" >&2
		cat "$tmp/consumer.out" >&2
		if [ -s "$tmp/consumer/go.sum" ]; then cat "$tmp/consumer/go.sum" >&2; fi
		exit 1
	fi

	# 5. The committed workspace builds the program with nothing to fetch.
	mkdir "$tmp/modcache"
	if ! GOWORK="$root/go.work" GOPROXY=off GOMODCACHE="$tmp/modcache" \
		GOFLAGS="-modcacherw -buildvcs=false" \
		go build -o "$tmp/rota" ./cmd/rota >"$tmp/build.out" 2>&1; then
		echo "module: the workspace does not build the program offline" >&2
		cat "$tmp/build.out" >&2
		exit 1
	fi
	[ "$("$tmp/rota" --version)" = "rota $(version)" ] ||
		fail "the workspace does not build the program offline"

	echo "module: ok"
}

case ${1-} in
check) check ;;
*) usage ;;
esac
