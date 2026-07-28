#!/usr/bin/env bash
# test-isolation.sh - prove the Go test suite writes nothing outside its
# temporary directories.
#
# The suite is supposed to keep every file it creates inside t.TempDir() or an
# os.MkdirTemp() directory it removes again. This script tests that claim twice,
# because one check alone cannot see both failure modes:
#
#   Phase A  Snapshot the real locations a leak would land in, run the suite,
#            snapshot again, and compare. This catches a test that MODIFIES
#            state that is already on the machine.
#
#   Phase B  Run the suite again with HOME pointed at an empty scratch
#            directory, then check that no bladerunner path appeared under it.
#            This catches a test that CREATES state on a clean machine - which
#            Phase A cannot see, because on a machine where the file already
#            exists the test finds it and leaves it alone.
#
# The script is read-only apart from its own mktemp directory and the Go build
# cache. It never deletes anything in $HOME, /Volumes or the working tree.
#
# Exit status: 0 when both phases are clean, 1 when residue is found.

set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

if [ ! -f go.mod ]; then
	echo "error: $repo_root is not the repository root (no go.mod)" >&2
	exit 2
fi

# sha256 of stdin, portable between macOS (shasum) and Linux (sha256sum).
if command -v sha256sum >/dev/null 2>&1; then
	hash_file() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	hash_file() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	echo "error: neither sha256sum nor shasum is installed" >&2
	exit 2
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/br-isolation.XXXXXX")
trap 'rm -rf "$work"' EXIT

export GOCACHE="${GOCACHE:-$repo_root/.cache/go-build}"

# Directories bladerunner itself writes to. These are small, so every file is
# hashed: a rewritten file with identical content is still reported as touched
# only if its bytes changed, which is what a hash is for.
hashed_roots=(
	"$HOME/.local/state/bladerunner"
	"$HOME/.config/bladerunner"
)

# Directories a stray test could drop a cartridge or a mount into. These can
# hold many gigabytes, so only the top level is listed - a leaked .dmg or a
# leaked mount appears as a new top-level entry, which is enough to catch it.
listed_roots=(
	"$HOME/Downloads"
	"/Volumes"
)

# snapshot writes a manifest of every watched location to $1.
snapshot() {
	local out=$1
	: >"$out"
	local root file
	for root in "${hashed_roots[@]}"; do
		if [ ! -d "$root" ]; then
			printf 'ABSENT\t%s\n' "$root" >>"$out"
			continue
		fi
		while IFS= read -r -d '' file; do
			printf '%s\t%s\n' "$(hash_file "$file")" "$file" >>"$out"
		done < <(find "$root" -type f -print0 2>/dev/null | sort -z)
	done
	for root in "${listed_roots[@]}"; do
		if [ ! -d "$root" ]; then
			printf 'ABSENT\t%s\n' "$root" >>"$out"
			continue
		fi
		while IFS= read -r -d '' file; do
			printf 'ENTRY\t%s\n' "$file" >>"$out"
		done < <(find "$root" -mindepth 1 -maxdepth 1 -print0 2>/dev/null | sort -z)
	done
	git status --porcelain >>"$out"
}

fail=0

echo "== Phase A: snapshot, run the suite, snapshot again =="
snapshot "$work/before.txt"
echo "manifest: $(wc -l <"$work/before.txt" | tr -d ' ') lines"

set +e
go test -count=1 ./... >"$work/suite-a.log" 2>&1
suite_a=$?
set -e
echo "suite exit status: $suite_a"
if [ "$suite_a" -ne 0 ]; then
	echo "the suite itself failed; see the tail below"
	tail -n 20 "$work/suite-a.log"
	fail=1
fi

snapshot "$work/after.txt"
if diff -u "$work/before.txt" "$work/after.txt" >"$work/residue.diff"; then
	echo "PASS: no watched path was created, modified or removed"
else
	echo "FAIL: the suite changed state outside its temporary directories"
	sed -e "s|$HOME|\$HOME|g" "$work/residue.diff"
	fail=1
fi

echo
echo "== Phase B: run the suite with HOME redirected to an empty directory =="
sandbox="$work/home"
mkdir -p "$sandbox"

# Keep the Go toolchain's own caches on their real paths. Otherwise the module
# cache and build cache would be rebuilt inside the sandbox, and the toolchain's
# own bookkeeping would look like suite residue.
real_gomodcache=$(go env GOMODCACHE)
real_gopath=$(go env GOPATH)

set +e
env HOME="$sandbox" \
	GOCACHE="$GOCACHE" \
	GOMODCACHE="$real_gomodcache" \
	GOPATH="$real_gopath" \
	go test -count=1 ./... >"$work/suite-b.log" 2>&1
suite_b=$?
set -e
echo "suite exit status: $suite_b"
if [ "$suite_b" -ne 0 ]; then
	echo "the suite itself failed; see the tail below"
	tail -n 20 "$work/suite-b.log"
	fail=1
fi

# The Go toolchain writes its own bookkeeping under HOME whatever we do: the
# module cache in $HOME/go and the telemetry counters in the user config dir,
# which is $HOME/.config/go on Linux and "$HOME/Library/Application Support/go"
# on macOS. Prune every directory named "go" or ".cache", then list FILES only,
# so a directory that holds nothing after the prune does not read as residue.
residue=$(find "$sandbox" \
	-type d \( -name go -o -name .cache \) -prune -o \
	-type f -print 2>/dev/null | sort)

if [ -z "$residue" ]; then
	echo "PASS: the suite created no file under a clean HOME"
else
	echo "FAIL: the suite created these files under a clean HOME"
	printf '%s\n' "$residue" | sed -e "s|$sandbox|\$HOME|g"
	fail=1
fi

echo
if [ "$fail" -eq 0 ]; then
	echo "test isolation: OK"
else
	echo "test isolation: RESIDUE FOUND"
fi
exit "$fail"
