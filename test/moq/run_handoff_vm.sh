#!/usr/bin/env bash
# Run against a prepared Lima guest; copy traces back even when a trial fails.
set -euo pipefail
if [[ $# -eq 0 || $1 == --help ]]; then
    echo "Usage: $0 NEW_OUTPUT_DIRECTORY [handoff_compare.py options]"
    exit 0
fi
out=$1
shift
if [[ -e $out ]]; then
    echo "Output directory already exists: $out" >&2
    exit 1
fi
mkdir -p "$(dirname "$out")"
support=${XDG_DATA_HOME:-"$HOME/.local/share"}/livekit-handoff
ctl=${LIMACTL:-"$support/tools/bin/limactl"}
if [[ -z ${LIMACTL:-} && ! -x $ctl ]]; then ctl=limactl; fi
export LIMA_HOME=${LIMA_HOME:-"$support/vm"}
guest=${HANDOFF_VM:-net}
here=$(cd "$(dirname "$0")" && pwd -P)
state=$("$ctl" list "$guest" --format '{{.Status}}')
if [[ -z $state ]]; then
    echo "Prepare a Lima guest as described in test/moq/HANDOFF.md" >&2
    exit 1
fi
if [[ $state != Running ]]; then
    "$ctl" start --tty=false "$guest"
    trap '"$ctl" stop "$guest"' EXIT
fi
inside() { "$ctl" shell --workdir /tmp "$guest" "$@"; }
work=$(inside mktemp -d /tmp/lk-video.XXXXXXXX)
status=0
inside sudo env PYTHONDONTWRITEBYTECODE=1 python3 -u "$here/handoff_compare.py" \
    --server-bin /opt/livekit-handoff/livekit-server \
    --probe-bin /opt/livekit-handoff/probe \
    --output "$work/results" "$@" || status=$?
if inside test -d "$work/results"; then
    "$ctl" copy -r "$guest:$work/results" "$out"
fi
inside sudo rm -r -- "$work"
exit "$status"
