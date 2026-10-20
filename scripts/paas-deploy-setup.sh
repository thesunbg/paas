#!/bin/sh
# One-time server setup for CI deploys, run as root on the target server:
#   sh paas-deploy-setup.sh 'ssh-ed25519 AAAA... github-deploy' ./paas-deploy
# Does NOT touch sshd. Disable PasswordAuthentication / PermitRootLogin only after a deploy
# through the new user has worked (see plans/261020-isolation-policy-ci/phase-05-deploy-ci.md).
set -eu
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
[ $# -eq 2 ] || { echo "usage: $0 '<public key>' <path to paas-deploy>" >&2; exit 2; }
KEY="$1"; WRAPPER="$2"

id deploy >/dev/null 2>&1 || useradd --create-home --shell /bin/bash deploy
usermod -aG systemd-journal deploy 2>/dev/null || true
install -d -m 0700 -o deploy -g deploy /home/deploy/.ssh /home/deploy/stage
printf '%s\n' "$KEY" > /home/deploy/.ssh/authorized_keys
chown deploy:deploy /home/deploy/.ssh/authorized_keys && chmod 0600 /home/deploy/.ssh/authorized_keys

install -m 0755 -o root -g root "$WRAPPER" /usr/local/sbin/paas-deploy
printf 'deploy ALL=(root) NOPASSWD: /usr/local/sbin/paas-deploy\n' > /etc/sudoers.d/paas-deploy
chmod 0440 /etc/sudoers.d/paas-deploy
visudo -cf /etc/sudoers.d/paas-deploy || { rm -f /etc/sudoers.d/paas-deploy; exit 1; }
echo "ok: user deploy ready; ssh host key to pin:"
ssh-keyscan -t ed25519 localhost 2>/dev/null | sed 's/^localhost/<HOST>/' || true
