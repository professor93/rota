#!/bin/sh
# The line between the two modules: the root, github.com/professor93/rota,
# which holds every package and requires nothing, and the program,
# github.com/professor93/rota/cmd/rota, which requires the root at a release.
#
#   sh scripts/module.sh check       the line holds; prints "module: ok"
#   sh scripts/module.sh tidy        tidies cmd/rota/go.mod against this
#                                    checkout, keeping the pinned version
#   sh scripts/module.sh pin vX.Y.Z  at a release: pins the program to the
#                                    pushed root tag, commits, and tags
#                                    cmd/rota/vX.Y.Z locally; pushes nothing
#
# Works from any directory. A broken rule is one line on stderr and exit 1.
set -eu

# -P: go resolves go.work's paths physically, so the working directory has
# to be the physical one too, or a symlinked checkout is outside the workspace.
cd -P "$(dirname "$0")/.."
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

# tidy runs go mod tidy for the program against this checkout's root, in a
# temporary copy, then puts back the pinned version of the root and its
# go.sum lines. A failure writes nothing.
tidy() {
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	v=$(pinned)
	if [ -z "$v" ]; then
		echo "module: tidy failed" >&2
		echo "cmd/rota/go.mod does not require $root_module" >&2
		exit 1
	fi
	cp cmd/rota/go.mod "$tmp/go.mod"
	if [ -e cmd/rota/go.sum ]; then cp cmd/rota/go.sum "$tmp/go.sum"; fi
	if ! (
		go mod edit -replace="$root_module=$root" "$tmp/go.mod" &&
			cd cmd/rota &&
			GOWORK=off GOFLAGS= go mod tidy -modfile="$tmp/go.mod" &&
			go mod edit -dropreplace="$root_module" -require="$root_module@$v" "$tmp/go.mod"
	) >"$tmp/tidy.out" 2>&1; then
		echo "module: tidy failed" >&2
		cat "$tmp/tidy.out" >&2
		exit 1
	fi

	# The root's own lines stay: with the replace, tidy neither needs nor
	# keeps them.
	{
		if [ -e "$tmp/go.sum" ]; then cat "$tmp/go.sum"; fi
		if [ -e cmd/rota/go.sum ]; then grep "^$root_module " cmd/rota/go.sum || true; fi
	} | LC_ALL=C sort -u >"$tmp/go.sum.new"

	cmp -s "$tmp/go.mod" cmd/rota/go.mod || cp "$tmp/go.mod" cmd/rota/go.mod
	if [ -s "$tmp/go.sum.new" ]; then
		cmp -s "$tmp/go.sum.new" cmd/rota/go.sum 2>/dev/null ||
			cp "$tmp/go.sum.new" cmd/rota/go.sum
	fi
}

# on_origin succeeds when origin has the ref, fails when it does not, and
# stops the script when origin cannot be asked.
on_origin() {
	if git ls-remote --exit-code --tags origin "$1" >/dev/null 2>"$tmp/ls-remote.err"; then
		return 0
	else
		rc=$?
	fi
	[ "$rc" -eq 2 ] && return 1
	echo "module: origin cannot be asked for $1" >&2
	cat "$tmp/ls-remote.err" >&2
	exit 1
}

# restore puts back the program's go.mod and go.sum saved by pin.
restore() {
	cp "$tmp/go.mod.bak" cmd/rota/go.mod
	if [ -e "$tmp/go.sum.bak" ]; then
		cp "$tmp/go.sum.bak" cmd/rota/go.sum
	else
		rm -f cmd/rota/go.sum
	fi
}

# pin requires the root at the pushed tag $1, checks the program builds
# against it, commits cmd/rota/go.mod and go.sum, and tags the commit
# cmd/rota/$1. It pushes nothing.
pin() {
	tag=${1-}
	printf '%s\n' "$tag" | grep -Eq "$release" || usage
	app_tag=cmd/rota/$tag

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	if git rev-parse -q --verify "refs/tags/$app_tag" >/dev/null ||
		on_origin "refs/tags/$app_tag"; then
		fail "$app_tag exists already; a version is never reused"
	fi
	on_origin "refs/tags/$tag" ||
		fail "$tag is not a tag on origin; push the root's tag first"
	[ "$(git rev-parse HEAD)" = "$(git rev-parse -q --verify "$tag^{commit}" 2>/dev/null)" ] ||
		fail "HEAD is not the commit tagged $tag"
	[ -z "$(git status --porcelain)" ] || fail "the tree has changes"
	says=$(GOWORK="$root/go.work" go run ./cmd/rota --version 2>&1) || true
	[ "$says" = "rota ${tag#v}" ] || fail "the program says $says, the tag says $tag"

	cp cmd/rota/go.mod "$tmp/go.mod.bak"
	if [ -e cmd/rota/go.sum ]; then cp cmd/rota/go.sum "$tmp/go.sum.bak"; fi
	if ! (
		go mod edit -require="$root_module@$tag" cmd/rota/go.mod &&
			cd cmd/rota &&
			GOWORK=off go mod tidy &&
			GOWORK=off GOFLAGS=-mod=readonly go build -o /dev/null . &&
			GOWORK=off go vet ./...
	) >"$tmp/pin.out" 2>&1; then
		restore
		echo "module: the program does not build against $tag" >&2
		cat "$tmp/pin.out" >&2
		exit 1
	fi

	if ! sh "$root/scripts/module.sh" check; then
		restore
		exit 1
	fi
	git add cmd/rota/go.mod
	if [ -e cmd/rota/go.sum ]; then git add cmd/rota/go.sum; fi
	git commit -q -m "cmd/rota requires rota $tag"
	git tag "$app_tag"
	branch=$(git symbolic-ref --short -q HEAD) || branch=HEAD
	echo "module: tagged $app_tag; push it with: git push origin $branch $app_tag"
}

case ${1-} in
check) check ;;
tidy) tidy ;;
pin) pin "${2-}" ;;
*) usage ;;
esac
