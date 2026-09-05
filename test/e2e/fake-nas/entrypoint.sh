#!/bin/sh
set -eu

# A persistent host key: regenerating it on every restart would look like a
# man-in-the-middle to the relay's pinning, which is correct but unhelpful here.
install -m 600 /keys/nas_hostkey /etc/ssh/ssh_host_ed25519_key
ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key > /etc/ssh/ssh_host_ed25519_key.pub

mkdir -p /home/nas/.ssh
install -m 600 /keys/nas_key.pub /home/nas/.ssh/authorized_keys
chown -R nas:nas /home/nas
chmod 700 /home/nas/.ssh

mkdir -p /volume1/media
chown nas:nas /volume1/media

exec /usr/sbin/sshd -D -e
