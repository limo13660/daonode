#!/bin/sh

set -eu

unset GOROOT

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
ysblcore_dir=${YSBLCORE_DIR:-"$repo_dir/../ysblcore"}
go_command=${GO:-go}

if [ ! -f "$ysblcore_dir/go.mod" ]; then
	echo "YSBLCore module not found at $ysblcore_dir; set YSBLCORE_DIR to its checkout" >&2
	exit 1
fi

workspace_dir=$(mktemp -d "${TMPDIR:-/tmp}/daonode-sudoku-interop.XXXXXX")
cleanup() {
	rm -rf "$workspace_dir"
}
trap cleanup EXIT HUP INT TERM

(
	cd "$workspace_dir"
	GOTOOLCHAIN=local GOEXPERIMENT=jsonv2 "$go_command" work init "$repo_dir" "$ysblcore_dir"
)

GOTOOLCHAIN=local \
	GOEXPERIMENT=jsonv2 \
	GOWORK="$workspace_dir/go.work" \
	"$go_command" test -count=1 -tags "with_quic interop" github.com/limo13660/daonode/core/sudoku
