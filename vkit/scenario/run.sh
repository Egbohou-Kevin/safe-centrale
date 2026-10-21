#!/usr/bin/env bash
# Runs the virtual kit's scenario inside the virtual Pi, as root (vkit/test.sh copies
# it there): every step by default, or the ones named.
#
#   sudo bash run.sh [STEP...]   steps: reset install arm intrusion wrong_codes fire
#                                       internet_cut modem crashes console maint video photo power
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=steps.sh
. "$here/steps.sh"

trap 'internet on' EXIT # an interrupted run never leaves the site cut

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=(reset install arm intrusion wrong_codes fire internet_cut modem crashes console maint video photo power)
[ "${steps[0]}" = reset ] || record
for s in "${steps[@]}"; do
	"step_$s"
done
summary
