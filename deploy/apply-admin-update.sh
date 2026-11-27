#!/usr/bin/env bash
set -euo pipefail
umask 077

current=/opt/megalove/megalove
incoming=${1:-/root/megalove-linux-amd64}
backup="$current.before-admin.$(date +%s)"
next="$current.next"
base=http://127.0.0.1:8081
exec 9>/run/lock/megalove-update.lock
flock -n 9 || { echo 'Another MegaLove update is running.' >&2; exit 1; }

expected=${2:?Pass the SHA-256 reported by the local build as the second argument}
[[ $expected =~ ^[a-fA-F0-9]{64}$ ]]
printf '%s  %s\n' "${expected,,}" "$incoming" | sha256sum -c -
status=$(curl -fsS --max-time 5 "$base/api/voting/status")
case "$status" in
    *'"state":"preparing"'*|*'"state":"closed"'*) ;;
    *) echo 'Voting must be confirmed closed before applying the update.' >&2; exit 1 ;;
esac

cp -a "$current" "$backup"
install -o root -g root -m 0755 "$incoming" "$next"
mv -f "$next" "$current"
if systemctl restart megalove; then
    for attempt in {1..10}; do
        if page=$(curl -fsS --max-time 2 "$base/admin/login" 2>/dev/null) &&
           [[ $page == *'/assets/admin.css'* ]] &&
           css=$(curl -fsS --max-time 2 "$base/assets/admin.css" 2>/dev/null) &&
           [[ $css == *'.admin-event-management'* ]] &&
           curl -fsS --max-time 2 -o /dev/null "$base/healthz"; then
            echo 'MegaLove updated. Open https://vote.megabattle.site/admin'
            exit 0
        fi
        sleep 1
    done
fi

install -o root -g root -m 0755 "$backup" "$next"
mv -f "$next" "$current"
systemctl restart megalove
echo 'Update failed. Previous executable restored.' >&2
exit 1
