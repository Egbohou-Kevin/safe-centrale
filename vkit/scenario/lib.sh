#!/usr/bin/env bash
# Helpers of the virtual kit's scenarios (docs/kit-virtuel.md). Sourced inside the
# virtual Pi, as root, by vkit/scenario/run.sh.

SITE=tg-lom-0001
HUB="mosquitto_sub -h 127.0.0.1 -p 1883"
CLOUD_TLS=(-h 127.0.0.1 -p 8883 --cafile /etc/vkit/ca.pem)
BUS=/tmp/vkit-bus.log     # every message on the hub's broker
CLOUDLOG=/tmp/vkit-cloud.log # every message the cloud receives
passed=0
failed=0

# record starts listening to both brokers, once per run.
record() {
	pkill -x mosquitto_sub || true
	: >"$BUS"
	: >"$CLOUDLOG"
	$HUB -t 'safehome/#' -t 'zigbee2mqtt/#' -v >>"$BUS" 2>&1 &
	mosquitto_sub "${CLOUD_TLS[@]}" -t 'safehome/#' -v >>"$CLOUDLOG" 2>&1 &
	sleep 1
}

say() { printf '\n=== %s\n' "$*"; }

# id makes a message id in the contract's format (26 characters, Crockford base32).
id() {
	local first rest
	first="$(LC_ALL=C tr -dc '0-7' </dev/urandom | head -c 1)"
	rest="$(LC_ALL=C tr -dc '0-9A-HJKMNP-TV-Z' </dev/urandom | head -c 25)"
	printf '%s%s' "$first" "$rest"
}

now() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
later() { date -u -d "+$1" +%Y-%m-%dT%H:%M:%S.%3NZ; }

# cloud_send TOPIC TYPE DATA_JSON: the cloud publishes a message for the site.
cloud_send() {
	mosquitto_pub "${CLOUD_TLS[@]}" -q 1 -t "safehome/$SITE/$1" -m \
		"{\"v\":1,\"id\":\"$(id)\",\"site\":\"$SITE\",\"src\":\"cloud\",\"ts\":\"$(now)\",\"type\":\"$2\",\"data\":$3}"
}

# app_order COMMAND [ARGS_JSON]: the app sends an order through the cloud.
app_order() {
	cloud_send alarm/cmd alarm.command \
		"{\"command\":\"$1\",\"args\":${2:-{\}},\"origin\":\"app\",\"user\":\"u_owner\",\"exp\":\"$(later '1 minute')\"}"
}

zigbee() { echo "$*" >/run/vkit/zigbee; }
modem() { echo "$*" >/run/vkit/modem; }

# mark remembers where the logs are, so that expect only looks at what follows.
mark() {
	bus_from=$(wc -l <"$BUS")
	cloud_from=$(wc -l <"$CLOUDLOG")
	journal_from="$(date '+%Y-%m-%d %H:%M:%S')"
}

# found WHERE PATTERN: the pattern appeared since the mark. WHERE is bus, cloud, or
# a systemd unit.
found() {
	case "$1" in
	bus) tail -n +"$((bus_from + 1))" "$BUS" | grep -E -- "$2" >/dev/null ;;
	cloud) tail -n +"$((cloud_from + 1))" "$CLOUDLOG" | grep -E -- "$2" >/dev/null ;;
	*) journalctl -u "$1" --since "$journal_from" --no-pager -o cat | grep -E -- "$2" >/dev/null ;;
	esac
}

check_passed() {
	passed=$((passed + 1))
	printf '  ok    %s\n' "$1"
}

check_failed() {
	failed=$((failed + 1))
	printf '  FAIL  %s\n' "$1"
}

# expect WHERE PATTERN SECONDS WHAT: waits for the pattern, and counts the check.
expect() {
	local deadline=$((SECONDS + $3))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if found "$1" "$2"; then
			check_passed "$4"
			return 0
		fi
		sleep 0.5
	done
	check_failed "$4 (no /$2/ in $1 within $3 s)"
}

# expect_retained TOPIC PATTERN SECONDS WHAT: the retained message of a topic of the
# site (published at start and at each change, so maybe before the mark) matches.
expect_retained() {
	local deadline=$((SECONDS + $3))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if timeout 3 mosquitto_sub -h 127.0.0.1 -t "safehome/$SITE/$1" -C 1 2>/dev/null | grep -E -- "$2" >/dev/null; then
			check_passed "$4"
			return 0
		fi
		sleep 2
	done
	check_failed "$4 (the retained $1 never matched /$2/ within $3 s)"
}

# expect_file FILE PATTERN WHAT: the file holds the pattern.
expect_file() {
	if grep -qE -- "$2" "$1"; then
		check_passed "$3"
	else
		check_failed "$3 (no /$2/ in $1: $(tr '\n' ' ' <"$1" | cut -c1-300))"
	fi
}

summary() {
	printf '\n%d check(s) passed, %d failed\n' "$passed" "$failed"
	[ "$failed" = 0 ]
}
