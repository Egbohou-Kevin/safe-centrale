#!/usr/bin/env bash
# The steps of the virtual kit's scenario (docs/kit-virtuel.md), sourced after
# lib.sh. Each step leaves the site disarmed and quiet for the next one.

DOOR=0x00158d0000000001
MOTION=0x00158d0000000002
SMOKE=0x00158d0000000003
SIREN=0x00158d0000000004
KEYPAD=0x00158d0000000006
OWNER_CODE=1357
INSTALLER_CODE=2468
GUARD=+22870000003
INSTALL="sudo -u safehome safehome-install"
HUBCONF="-config /etc/safehome/hub.json"

# A fresh site: no device, no code, no alarm state, no retained message.
step_reset() {
	say "fresh site"
	systemctl stop 'safehome-*' vkit-z2m mosquitto
	rm -rf /var/lib/safehome/* /var/lib/mosquitto/*
	systemctl start mosquitto vkit-z2m
	systemctl start safehome-hub safehome-gsm safehome-zigbee safehome-agent safehome-console
	sleep 3
	record
}

# docs/systeme.md steps 9 and 10, with the installer's tool.
step_install() {
	say "installation: codes, pairing, zones, siren"
	mark
	echo "$OWNER_CODE" | sudo -u safehome safehome-hub codes $HUBCONF set u_owner >/dev/null
	echo "$INSTALLER_CODE" | sudo -u safehome safehome-hub codes $HUBCONF set installer >/dev/null
	$INSTALL pair $HUBCONF -for 20s >/tmp/vkit-pair.log 2>&1 &
	local pairing=$!
	sleep 3
	zigbee pair MCCGQ11LM $DOOR
	zigbee pair RTCGQ11LM $MOTION
	zigbee pair HS1SA $SMOKE
	zigbee pair HS2WD-E $SIREN
	zigbee pair KEPZB-110 $KEYPAD
	zigbee pair TS0203 0x00158d00000000ff
	wait "$pairing"
	expect_file /tmp/vkit-pair.log "network closed: 5 device\(s\) added" "five devices paired"
	expect_file /tmp/vkit-pair.log "x refused TS0203: no profile" "an unknown model is refused, with the reason"
	expect_file /tmp/vkit-pair.log "next: safehome-install siren .* add" "the siren's next step is to make it a siren"
	local door motion smoke siren
	door="$(device_id aqara.contact)"
	motion="$(device_id aqara.motion)"
	smoke="$(device_id heiman.smoke)"
	siren="$(device_id heiman.siren)"
	$INSTALL zone $HUBCONF -name Entree -kind entry "$door" >/dev/null
	$INSTALL zone $HUBCONF -name Boutique -kind interior "$motion" >/dev/null
	$INSTALL zone $HUBCONF -name Fumee -kind 24h_fire "$smoke" >/dev/null
	$INSTALL siren $HUBCONF add "$siren" >/dev/null
	$INSTALL siren $HUBCONF remove dev_siren1 >/dev/null
	$INSTALL check $HUBCONF >/tmp/vkit-check.log 2>&1
	expect_file /tmp/vkit-check.log "OK: 3 zone\(s\), 1 siren\(s\), 5 paired device\(s\)" "the installation checks"
	systemctl restart safehome-hub
	sleep 3
	# Every device says how it is, as real ones do soon after pairing.
	zigbee send $DOOR '{"contact":true,"battery":95,"linkquality":200}'
	zigbee send $MOTION '{"occupancy":false,"battery":95,"linkquality":200}'
	zigbee send $SMOKE '{"smoke":false,"tamper":false,"battery":95,"linkquality":200}'
	zigbee send $KEYPAD '{"tamper":false,"battery":95,"linkquality":200}'
	sleep 2
}

device_id() { $INSTALL devices $HUBCONF | awk -v m="$1" 'index($2, m) == 1 { print $1 }'; }

step_arm() {
	say "arming from the app"
	mark
	app_order alarm.arm
	expect bus '"type":"alarm.ack"' 10 "the hub acknowledges the order"
	expect bus '"type":"alarm.arming"' 10 "the exit delay starts"
	expect cloud '"type":"alarm.arming"' 15 "the cloud sees it"
	expect bus '"type":"alarm.armed"' 25 "armed after the exit delay"
}

step_intrusion() {
	say "intrusion: the door opens"
	mark
	zigbee send $DOOR '{"contact":false}'
	expect bus '"type":"alarm.entry_delay"' 5 "the entry delay starts"
	expect bus '"type":"alarm.triggered"' 20 "the alarm goes off after the entry delay"
	expect vkit-z2m "SET $SIREN .*burglar" 10 "the siren is told to sound"
	expect cloud '"type":"alert.request"' 15 "the cloud gets the alert"
	expect vkit-modem "SMS to=\\$GUARD text=\"ALARME" 60 "an SMS goes to the guard through the modem"
	expect vkit-modem "CALL to=\\$GUARD" 60 "the guard is called"
	zigbee send $DOOR '{"contact":true}'

	say "the guard answers 1 by SMS"
	mark
	modem sms "$GUARD" 1
	expect bus '"type":"alert.ack".*"via":"sms"' 15 "the answer acknowledges the alert"

	say "the owner disarms from the app"
	mark
	app_order alarm.disarm "{\"code\":\"$OWNER_CODE\"}"
	expect bus '"type":"alarm.disarmed"' 10 "disarmed"
	expect cloud '"type":"alarm.disarmed"' 15 "the cloud sees it"
	never_code
}

# The code is never written in a log nor on the disk (the order that carries it
# goes over the hub's own broker only).
never_code() {
	if journalctl -u 'safehome-*' --since "-10min" --no-pager -o cat | grep -q "$OWNER_CODE" ||
		grep -rqs "$OWNER_CODE" /var/lib/safehome /var/lib/mosquitto; then
		check_failed "the code is written in a log or on the disk"
	else
		check_passed "the code is in no log and on no disk"
	fi
}

step_wrong_codes() {
	say "three wrong codes from the app"
	mark
	for _ in 1 2 3; do
		app_order alarm.disarm '{"code":"9999"}'
		sleep 1
	done
	expect bus '"type":"alarm.auth_failed"' 10 "the failures are announced"
	expect bus '"type":"alert.request".*auth_failed' 10 "the owner is alerted"
	app_order alarm.disarm "{\"code\":\"$OWNER_CODE\"}"
	expect bus '"status":"(refused|locked|rejected)' 10 "the right code is refused while locked"
}

step_fire() {
	say "smoke while disarmed"
	mark
	zigbee send $SMOKE '{"smoke":true}'
	expect bus '"type":"alarm.triggered".*"capability":"smoke"' 10 "a fire alarm, armed or not"
	expect vkit-modem 'SMS to=.*text=".*(FEU|feu|incendie|INCENDIE|fumee|Fumee)' 30 "an SMS goes out at once"
	zigbee send $SMOKE '{"smoke":false}'
	sleep 1
	app_order alarm.disarm "{\"code\":\"$OWNER_CODE\"}"
	expect bus '"type":"alarm.(disarmed|cleared)"' 70 "stopped by the owner (after the lockout)"
}

step_internet_cut() {
	say "internet cut while armed"
	systemctl stop vkit-cloud
	mark
	app_order_local alarm.arm
	expect bus '"type":"alarm.armed"' 30 "armed from the hub's side"
	expect bus '"type":"agent.status".*"uplink":"down"' 90 "the agent sees the link down"
	mark
	systemctl start vkit-cloud
	expect cloud '"type":"alarm.armed"' 60 "the cloud gets what happened during the cut"
	app_order alarm.disarm "{\"code\":\"$OWNER_CODE\"}"
	expect bus '"type":"alarm.disarmed"' 10 "disarmed again"
}

# app_order_local publishes the order on the hub's broker, as the agent would: for
# when the cloud is cut.
app_order_local() {
	local data
	data="{\"command\":\"$1\",\"args\":${2:-{\}},\"origin\":\"app\",\"user\":\"u_owner\",\"exp\":\"$(later '1 minute')\"}"
	mosquitto_pub -h 127.0.0.1 -q 1 -t "safehome/$SITE/alarm/cmd" -m \
		"{\"v\":1,\"id\":\"$(id)\",\"site\":\"$SITE\",\"src\":\"cloud\",\"ts\":\"$(now)\",\"type\":\"alarm.command\",\"data\":$data}"
}

step_modem() {
	say "the modem is pulled out and comes back"
	mark
	modem unplug
	expect safehome-gsm "went away|does not answer" 60 "the gateway notices"
	mark
	expect bus '"type":"gsm.status".*"responding":true' 90 "the line answers again"
	say "the modem loses the network"
	mark
	modem network off
	expect bus '"type":"gsm.status".*"registered":false' 90 "the status says it"
	modem network on
	expect bus '"type":"gsm.status".*"registered":true' 90 "and that it is back"
}

step_crashes() {
	say "the hub crashes while armed"
	mark
	app_order alarm.arm
	sleep 1
	app_order alarm.arm_now
	expect bus '"type":"alarm.armed"' 10 "armed at once"
	mark
	systemctl kill -s KILL safehome-hub
	expect safehome-hub "Started safehome-hub" 15 "systemd starts it again"
	expect bus '"type":"alarm.state".*"state":"armed"' 15 "still armed after the crash"

	say "the hub freezes: the watchdog"
	mark
	systemctl kill -s STOP safehome-hub
	expect safehome-hub "Watchdog timeout|watchdog" 60 "systemd's watchdog sees it"
	expect safehome-hub "Started safehome-hub" 30 "and restarts it"
	app_order alarm.disarm "{\"code\":\"$OWNER_CODE\"}"
	expect bus '"type":"alarm.disarmed"' 15 "disarmed"
}

step_console() {
	say "installation mode from the keypad, then the console's Wi-Fi"
	mark
	zigbee send $KEYPAD "{\"action\":\"disarm\",\"action_code\":\"$INSTALLER_CODE\"}"
	expect bus '"type":"install.mode".*"status":"open"' 10 "the installation mode opens"
	expect safehome-console "the console's page is served" 30 "the console starts its network and its page"
	local pass
	pass="$(cat /etc/safehome/console.secret)"
	if join_wifi "safehome-$SITE" "$pass"; then
		check_passed "a phone joins the hub's Wi-Fi"
	else
		check_failed "a phone joins the hub's Wi-Fi ($(cat /tmp/vkit-wifi.log))"
	fi
	if $PHONE curl -fsS --max-time 5 http://10.42.0.1/ -o /tmp/vkit-console.html; then
		check_passed "the console's page answers"
	else
		check_failed "the console's page answers"
	fi
	leave_wifi
	mark
	mosquitto_pub -h 127.0.0.1 -q 1 -t "safehome/$SITE/install/cmd" -m \
		"{\"v\":1,\"id\":\"$(id)\",\"site\":\"$SITE\",\"src\":\"console\",\"ts\":\"$(now)\",\"type\":\"install.close\",\"data\":{}}"
	expect bus '"type":"install.mode".*"status":"closed"' 15 "the mode closes"
}

step_maint() {
	say "remote maintenance: a tunnel to the bastion, on the cloud's request"
	mark
	cloud_send maint/cmd maint.open \
		"{\"session\":\"m1\",\"user\":\"kevin\",\"reason\":\"vkit\",\"duration_s\":120,\"exp\":\"$(later '1 minute')\"}"
	expect bus '"type":"maint.session".*"status":"open"' 30 "the tunnel opens"
	expect cloud '"type":"maint.session".*"status":"open"' 15 "the owner can see it"
	if ssh-keyscan -T 5 -p 22001 localhost 2>/dev/null | grep -q "$(cut -d' ' -f2 /etc/ssh/ssh_host_ed25519_key.pub)"; then
		check_passed "the bastion's port 22001 leads to the hub's ssh"
	else
		check_failed "the bastion's port 22001 leads to the hub's ssh"
	fi
	# docs/maintenance.md: a stolen hub gives neither a shell on the bastion nor
	# another site's port.
	# As the service's user: the key is root:safehome 0640, fine for it, too open for root.
	local key=(-F /dev/null -i /etc/safehome/maint.key -o BatchMode=yes -o UserKnownHostsFile=/etc/safehome/maint_known_hosts)
	local said rc=0
	said="$(timeout 10 sudo -u safehome ssh "${key[@]}" tunnel@127.0.0.1 id 2>&1)" || rc=$?
	if [ "$rc" != 0 ] && echo "$said" | grep -q "not available"; then
		check_passed "the hub's key opens no shell on the bastion"
	else
		check_failed "the hub's key opens no shell on the bastion (exit $rc: $said)"
	fi
	# Refused, ssh stops at once (255); allowed, it would stay up until the timeout.
	rc=0
	said="$(timeout 10 sudo -u safehome ssh "${key[@]}" -o ExitOnForwardFailure=yes -N -R localhost:22002:localhost:22 tunnel@127.0.0.1 2>&1)" || rc=$?
	if [ "$rc" = 255 ] && echo "$said" | grep -qi "forwarding failed"; then
		check_passed "the hub's key cannot listen on another site's port"
	else
		check_failed "the hub's key cannot listen on another site's port (exit $rc: $said)"
	fi
	mark
	cloud_send maint/cmd maint.close '{"session":"m1"}'
	expect bus '"type":"maint.session".*"status":"closed"' 15 "the tunnel closes"
	sleep 1
	if ss -ltn | grep -q ':22001 '; then
		check_failed "nothing listens on the bastion once closed"
	else
		check_passed "nothing listens on the bastion once closed"
	fi
}

step_video() {
	say "live video: the owner watches the camera from the app"
	mark
	expect bus '"type":"video.status".*"reachable":true' 330 "the camera is reachable"
	mark
	cloud_send video/cmd video.request \
		"{\"session\":\"v1\",\"camera\":\"cam_caisse\",\"reason\":\"demand\",\"user\":\"u_owner\",\"max_s\":60,\"push_url\":\"rtmps://127.0.0.1:1936/live/v1\",\"token\":\"vkit-video-token\",\"exp\":\"$(later '1 minute')\"}"
	expect bus '"type":"video.session".*"status":"live"' 30 "the stream is live"
	expect vkit-relay "Input #0, flv" 30 "the relay receives it"
	expect vkit-relay "Video: h264" 10 "in H.264, untouched"
	mark
	cloud_send video/cmd video.stop '{"session":"v1"}'
	expect bus '"type":"video.session".*"status":"ended"' 20 "the stream ends"
	if journalctl -u 'safehome-*' --since "-5min" --no-pager -o cat | grep -q vkit-video-token; then
		check_failed "the stream's token is in no log"
	else
		check_passed "the stream's token is in no log"
	fi
}

# The installer's phone: the second radio, in a network namespace of its own so
# that its traffic really goes over the simulated air to the hub (in one namespace
# the kernel would shortcut it). A new network takes a few seconds to show up.
PHONE="ip netns exec phone"

phone_radio() {
	ip netns add phone 2>/dev/null || true
	if ! $PHONE iw dev 2>/dev/null | grep -q wlan1; then
		iw phy phy1 set netns name phone
	fi
	$PHONE ip link set lo up
}

# join_wifi SSID PASSPHRASE: the phone joins the network and takes an address.
join_wifi() {
	phone_radio
	pkill -f "wpa_supplicant -B -i wlan1" || true
	wpa_passphrase "$1" "$2" >/tmp/vkit-phone.conf
	$PHONE wpa_supplicant -B -i wlan1 -c /tmp/vkit-phone.conf >/tmp/vkit-wifi.log 2>&1 || return 1
	$PHONE dhcpcd -1 -4 -t 60 wlan1 >>/tmp/vkit-wifi.log 2>&1
}

leave_wifi() {
	$PHONE dhcpcd -k wlan1 >/dev/null 2>&1 || true
	pkill -f "wpa_supplicant -B -i wlan1" || true
}
