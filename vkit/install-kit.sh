#!/usr/bin/env bash
# Runs inside the virtual Pi, as root (vkit/deploy.sh starts it): puts the fake
# devices and the cloud's broker in place, then installs safeHome with the real
# deploy/install.sh, as on a real hub.
#
#   sudo bash /tmp/vkit-deploy/vkit/install-kit.sh /tmp/vkit-deploy
set -euo pipefail

src="$1"
bundle="$src/bundle"

install -m 755 "$src/vkit/bin/vkit-fakemodem" "$src/vkit/bin/vkit-fakez2m" /usr/local/bin/
install -m 644 "$src"/vkit/systemd/*.service /etc/systemd/system/

# The cloud's broker: its own authority and certificate, made once.
apt-get install -y -q mosquitto >/dev/null
mkdir -p /etc/vkit
install -m 644 "$src/vkit/cloud.conf" /etc/vkit/cloud.conf
if [ ! -f /etc/vkit/ca.pem ]; then
	openssl req -x509 -newkey ed25519 -nodes -days 3650 -subj /CN=vkit-ca \
		-keyout /etc/vkit/ca.key -out /etc/vkit/ca.pem 2>/dev/null
	openssl req -newkey ed25519 -nodes -subj /CN=127.0.0.1 \
		-keyout /etc/vkit/cloud.key -out /etc/vkit/cloud.csr 2>/dev/null
	openssl x509 -req -in /etc/vkit/cloud.csr -CA /etc/vkit/ca.pem -CAkey /etc/vkit/ca.key \
		-days 3650 -extfile <(printf 'subjectAltName=IP:127.0.0.1') -out /etc/vkit/cloud.pem 2>/dev/null
	chown root:mosquitto /etc/vkit/cloud.key
	chmod 640 /etc/vkit/cloud.key
fi
# The agent trusts that authority (agent.json: ca_file).
install -m 644 /etc/vkit/ca.pem "$bundle/site/vkit-cloud-ca.pem"

systemctl daemon-reload
systemctl enable --now vkit-modem vkit-cloud
"$bundle/deploy/install.sh"
systemctl enable --now vkit-z2m
