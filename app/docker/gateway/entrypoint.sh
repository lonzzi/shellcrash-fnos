#!/bin/sh
set -eu
umask 077

secret_file=/run/secrets/shellcrash-api.secret
template=/etc/shellcrash-gateway/nginx.conf.template
config=/etc/nginx/conf.d/default.conf
socket=/app/target/app.sock

if [ ! -r "$secret_file" ]; then
    echo "ShellCrash API secret is missing or unreadable" >&2
    exit 1
fi

secret=$(cat "$secret_file")
case "$secret" in
    *[!0-9a-f]*|'')
        echo "ShellCrash API secret has an invalid format" >&2
        exit 1
        ;;
esac
if [ "${#secret}" -ne 64 ]; then
    echo "ShellCrash API secret has an invalid length" >&2
    exit 1
fi

if [ ! -d /app/target ]; then
    echo "fnOS application target directory is not mounted" >&2
    exit 1
fi

rm -f "$socket"
sed "s/@@API_SECRET@@/$secret/g" "$template" > "$config"
chmod 600 "$config"
nginx -t
exec nginx -g 'daemon off;'
