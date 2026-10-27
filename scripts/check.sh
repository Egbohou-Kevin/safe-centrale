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
# The guardrails themselves live in internal/qualitytest and internal/archtest
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

# What is the project's own, each with its default: the packages that hold the quality and
# architecture rules, the shell scripts to lint, the os/arch the programs are built for.
rule_packages=${CHECK_RULE_PACKAGES:-./internal/qualitytest ./internal/archtest}
shell_scripts=${CHECK_SHELL_SCRIPTS:-deploy/*.sh scripts/*.sh .githooks/pre-commit}
target=${CHECK_TARGET:-linux/arm64}

step() { printf '\n==> %s\n' "$*"; }

step "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
	echo "not gofmt-formatted (run gofmt -w on them):" >&2
	echo "$unformatted" >&2
	exit 1
fi

step "go vet"
go vet ./...

if [ "$quick" = true ]; then
	step "go test -short"
	go test -short ./...
	exit 0
fi

step "quality and architecture rules (sizes, ASCII, duplication, dead code, dependencies...)"
go test -count=1 $rule_packages

step "go test -race"
go test -race ./...

step "shell scripts"
if command -v shellcheck >/dev/null 2>&1; then
	shellcheck --severity=error $shell_scripts
else
	echo "shellcheck is not installed: skipped"
fi

step "build every program for the target ($target)"
CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}" go build ./cmd/...

step "all checks passed"
