#!/usr/bin/env bash
# Plays the scenario on the virtual Pi (docs/kit-virtuel.md), after vkit/up.sh and
# vkit/deploy.sh: every step by default, or the ones named (see scenario/run.sh).
#
#   vkit/test.sh [STEP...]
set -euo pipefail
VM=safehome-pi
here="$(cd "$(dirname "$0")" && pwd)"
limactl shell "$VM" -- rm -rf /tmp/vkit-scenario
limactl copy -r "$here/scenario" "$VM:/tmp/vkit-scenario"
limactl shell "$VM" sudo bash /tmp/vkit-scenario/run.sh "$@"
