#!/usr/bin/env bash
# The project's verification, in one place: the pre-commit hook (.githooks/pre-commit)
# and the CI (.github/workflows/ci.yml) both run this script, so their steps cannot
# drift apart. Run it from anywhere in the repository:
#
#   scripts/check.sh           everything: gofmt, vet, quality and architecture rules,
#                              go test -race ./..., shell scripts, build for the target
#   scripts/check.sh --quick   before a commit: gofmt, vet, go test -short ./...
#                              (the quality and architecture rules included; the
#                              end-to-end scenarios and the install script skipped)
#
# The guardrails themselves live in internal/checks/qualitytest and internal/checks/archtest
# (docs/architecture.md, "Les garde-fous").
# Quiet unless CHECK_VERBOSE=1 or under CI: green says one line, red the end of the log.
# Every line printed here lands in a session's context (creator, profiles/quiet.sh).
if [ -z "${CHECK_VERBOSE:-}" ] && [ -z "${CI:-}" ] && [ -z "${CHECK_QUIET_INNER:-}" ]; then
	log="$(git rev-parse --absolute-git-dir 2>/dev/null || echo "${TMPDIR:-/tmp}")/creator-check.log"
	SECONDS=0
	if CHECK_QUIET_INNER=1 "$BASH" "$0" "$@" >"$log" 2>&1; then
		echo "check vert en ${SECONDS}s ($(wc -l <"$log" | tr -d ' ') lignes dans $log)" >&2
		exit 0
	else
		status=$?
	fi
	echo "check ROUGE (code $status, ${SECONDS}s) ; les 60 dernières lignes, le tout dans $log :" >&2
	tail -n 60 "$log" >&2
	exit "$status"
fi
# The inner run forgets it, so a check it starts (a test of creator) is quiet in its turn.
unset CHECK_QUIET_INNER
set -euo pipefail

quick=false
case "${1:-}" in
"") ;;
--quick) quick=true ;;
*)
	echo "usage: scripts/check.sh [--quick]" >&2
	exit 2
	;;
esac

cd "$(dirname "$0")/.."

# The project's own settings (the "env" of .creator.json) hold for every caller, not only the
# pre-commit hook; the caller's own value wins (19/11/2026: a lot's start check called this script
# directly, and station-backend's quick ran with the base and one fork, 436 s median by day and
# 2 239 s by night, against 87 s through the hook; creator, profiles/project-env.sh).
if [ -f .creator.json ]; then
	eval "$(python3 -c 'import json, shlex, sys
for k, v in json.load(open(sys.argv[1])).get("env", {}).items():
    if k.isidentifier():
        print("[ -n \"${%s+x}\" ] || export %s=%s" % (k, k, shlex.quote(str(v))))' .creator.json)"
fi

# What is the project's own, each with its default: the packages that hold the quality and
# architecture rules, the shell scripts to lint, the os/arch the programs are built for.
rule_packages=${CHECK_RULE_PACKAGES:-./internal/checks/qualitytest ./internal/checks/archtest}
shell_scripts=${CHECK_SHELL_SCRIPTS:-deploy/*.sh scripts/*.sh .githooks/pre-commit}
target=${CHECK_TARGET:-linux/arm64}

# One line per run in a journal that stays, the same eight columns as the other profiles (17/11/2026:
# safeHome made 27 commits in a week and nothing said what its checks cost; creator metrics reads this
# journal for the monitoring): date, mode, vert or rouge:code, seconds, step, branch, first failing
# line of the log (empty when green or with no log kept), seconds per step. In the git common dir,
# shared by the worktrees, capped, best effort.
mode=full current="" steps="" mark=$SECONDS
[ "$quick" = true ] && mode=quick
journal() {
	local status=$? file log first="" last=$current
	[ -n "$current" ] && steps="$steps${steps:+ }$current=$((SECONDS - mark))"
	file="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)/creator-runs.log" || return 0
	log="$(git rev-parse --absolute-git-dir 2>/dev/null)/creator-check.log"
	if [ "$status" != 0 ]; then
		first="$(grep -m1 -E '^--- FAIL|^FAIL|\.go:[0-9]+:[0-9]+: ' "$log" 2>/dev/null | tr '\t\r' '  ' | cut -c1-160)" || first=""
	else
		last=""
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$(date +%Y-%m-%dT%H:%M:%S%z)" "$mode" \
		"$([ "$status" = 0 ] && echo vert || echo "rouge:$status")" "$SECONDS" "$last" \
		"$(git branch --show-current 2>/dev/null)" "$first" "$steps" >>"$file" 2>/dev/null || return 0
	if [ "$(wc -l <"$file" | tr -d ' ')" -gt 5000 ]; then
		tail -n 2000 "$file" >"$file.tmp" && mv "$file.tmp" "$file"
	fi
}
trap journal EXIT

# step KEY TEXT: KEY names the step in the journal (empty: the end, nothing more to time).
step() {
	[ -n "$current" ] && steps="$steps${steps:+ }$current=$((SECONDS - mark))"
	current=$1 mark=$SECONDS
	printf '\n==> %s\n' "$2"
}

step gofmt "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
	echo "not gofmt-formatted (run gofmt -w on them):" >&2
	echo "$unformatted" >&2
	exit 1
fi

step vet "go vet"
go vet ./...

if [ "$quick" = true ]; then
	step test "go test -short"
	go test -short ./...
	step "" "all checks passed"
	exit 0
fi

step rules "quality and architecture rules (sizes, ASCII, duplication, dead code, dependencies...)"
go test -count=1 $rule_packages

step race "go test -race"
go test -race ./...

step shell "shell scripts"
if command -v shellcheck >/dev/null 2>&1; then
	shellcheck --severity=error $shell_scripts
else
	echo "shellcheck is not installed: skipped"
fi

step build "build every program for the target ($target)"
CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}" go build ./cmd/...

step "" "all checks passed"
