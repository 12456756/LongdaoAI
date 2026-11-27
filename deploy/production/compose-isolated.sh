#!/bin/sh
set -eu
mountpoint -q /www
exec docker -H unix:///run/longdao-docker.sock compose -p longdao-upgrade -f /www/longdao/environments/2026.11.27-1/compose.json "$@"
