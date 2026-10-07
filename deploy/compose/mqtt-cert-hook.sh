#!/bin/sh
# Copies a Let's Encrypt certificate for the MQTT broker into ./tls, readable
# by the container's user (uid 65532). The broker picks up a changed
# certificate within a minute, without a restart.
#
# Once, as root, from deploy/compose:
#   certbot certonly --standalone -d mqtt.plusclouds.com     # or your DNS plugin
#   ./mqtt-cert-hook.sh mqtt.plusclouds.com
# On every renewal:
#   certbot renew --deploy-hook "/path/to/deploy/compose/mqtt-cert-hook.sh mqtt.plusclouds.com"
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
