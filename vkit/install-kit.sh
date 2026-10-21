#!/usr/bin/env bash
# Runs inside the virtual Pi, as root (vkit/deploy.sh starts it): puts the fake
# devices and the cloud's broker in place, then installs safeHome with the real
# deploy/install.sh, as on a real hub.
#
#   sudo bash /tmp/vkit-deploy/vkit/install-kit.sh /tmp/vkit-deploy
set -euo pipefail

src="$1"
bundle="$src/bundle"

install -m 755 "$src"/vkit/bin/vkit-* /usr/local/bin/
install -m 644 "$src"/vkit/systemd/*.service /etc/systemd/system/

# The cloud's broker: its own authority and certificate, made once.
apt-get install -y -q mosquitto socat ffmpeg >/dev/null
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
# The agent trusts that authority (agent.json: ca_file), and so does the system, as
# it would trust the real relay's certificate (go2rtc pushes the video in rtmps).
install -m 644 /etc/vkit/ca.pem "$bundle/site/vkit-cloud-ca.pem"
install -m 644 /etc/vkit/ca.pem /usr/local/share/ca-certificates/vkit-cloud-ca.crt
update-ca-certificates >/dev/null
install -m 644 "$src/vkit/camera.yaml" /etc/vkit/camera.yaml

# The maintenance bastion is the VM's own sshd (docs/maintenance.md): an account
# for the tunnels, without a shell, that may only listen on the loopback.
getent passwd tunnel >/dev/null || useradd --system --create-home --shell /usr/sbin/nologin tunnel
cat >/etc/ssh/sshd_config.d/vkit-bastion.conf <<'CONF'
Match User tunnel
	AllowTcpForwarding remote
	GatewayPorts no
	PermitTTY no
	X11Forwarding no
CONF
systemctl reload ssh
# The bastion's host key, pinned by the hub (read on the bastion itself).
echo "127.0.0.1 $(cut -d' ' -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)" >"$bundle/site/maint_known_hosts"

systemctl daemon-reload
systemctl enable --now vkit-modem vkit-cloud
"$bundle/deploy/install.sh"
systemctl enable --now vkit-z2m vkit-camera vkit-relay vkit-relay-tls vkit-upload

# The hub's tunnel key, allowed on the bastion for its own port only.
install -d -m 700 -o tunnel -g tunnel ~tunnel/.ssh
echo "restrict,port-forwarding,permitlisten=\"localhost:22001\" $(cat /etc/safehome/maint.key.pub)" >~tunnel/.ssh/authorized_keys
chown tunnel:tunnel ~tunnel/.ssh/authorized_keys
chmod 600 ~tunnel/.ssh/authorized_keys
