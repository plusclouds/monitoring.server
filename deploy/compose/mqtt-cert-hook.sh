#!/bin/sh
# Copies a Let's Encrypt certificate for the MQTT broker into ./tls, readable
# by the container's user (uid 65532). The broker picks up a changed
# certificate within a minute, without a restart.
#
# Once, as root, with the reverse proxy serving /.well-known/acme-challenge/
# for the name from /var/www/letsencrypt (not --standalone: it needs port 80
# and fails, or stops the proxy, on renewal):
#   certbot certonly --webroot -w /var/www/letsencrypt -d mqtt.plusclouds.com \
#     --deploy-hook "sh /path/to/deploy/compose/mqtt-cert-hook.sh mqtt.plusclouds.com"
#   sh /path/to/deploy/compose/mqtt-cert-hook.sh mqtt.plusclouds.com
# certbot's timer then renews and runs the hook by itself. Pass the hook with
# --deploy-hook as above, not in /etc/letsencrypt/renewal-hooks/deploy/: the
# script finds ./tls next to itself and needs the certificate name.
set -eu
name=${1:?usage: mqtt-cert-hook.sh <certificate name>}
dir=$(cd "$(dirname "$0")" && pwd)/tls
live=/etc/letsencrypt/live/$name
mkdir -p "$dir"
install -m 0644 -o 65532 -g 65532 "$live/fullchain.pem" "$dir/mqtt.crt.new"
install -m 0600 -o 65532 -g 65532 "$live/privkey.pem" "$dir/mqtt.key.new"
# Key first: the broker reloads when the certificate file changes.
mv "$dir/mqtt.key.new" "$dir/mqtt.key"
mv "$dir/mqtt.crt.new" "$dir/mqtt.crt"
echo "MQTT certificate for $name installed in $dir"
