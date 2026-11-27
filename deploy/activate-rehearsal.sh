#!/usr/bin/env bash
set -euo pipefail
umask 077
cd /opt

base=http://127.0.0.1:8081
settings=/etc/megalove.env
app=/opt/megalove/megalove
db=megalove_rehearsal
dsn='postgres:///megalove_rehearsal?host=/var/run/postgresql&user=megalove'
status=$(curl -fsS --max-time 5 "$base/api/voting/status")
if [[ $status == *'"event":"rehearsal"'* ]]; then
    echo 'Rehearsal already selected.'
    exit 0
fi
if [[ $status != *'"event":"demo"'* || $status == *'"state":"open"'* ]]; then
    echo 'Refusing to switch an active or non-demo event.' >&2
    exit 1
fi

if ! runuser -u postgres -- psql -Atq -c "SELECT 1 FROM pg_database WHERE datname = '$db'" | grep -qx 1; then
    runuser -u postgres -- createdb --owner=megalove "$db"
fi
runuser -u megalove -- env "DATABASE_URL=$dsn" "$app" migrate
runuser -u megalove -- psql -X -v ON_ERROR_STOP=1 -d "$db" -c \
    "UPDATE events SET slug = 'rehearsal', name = 'Репетиция Мегабаттла' WHERE slug = 'megabattle-2026' AND state = 'preparing' AND NOT EXISTS (SELECT 1 FROM participants)"
state=$(runuser -u megalove -- psql -Atq -d "$db" -c "SELECT state FROM events WHERE slug = 'rehearsal'")
[[ $state = preparing ]]

backup=$(mktemp /etc/megalove.env.before-rehearsal.XXXXXX)
next=$(mktemp /etc/.megalove-env.XXXXXX)
trap 'rm -f "$next"' EXIT
cp -p "$settings" "$backup"
awk '!/^(DATABASE_URL|MEGALOVE_EVENT_SLUG)=/' "$settings" > "$next"
printf 'DATABASE_URL=%s\nMEGALOVE_EVENT_SLUG=rehearsal\n' "$dsn" >> "$next"
mv "$next" "$settings"
if systemctl restart megalove; then
    for attempt in {1..10}; do
        status=$(curl -fsS --max-time 2 "$base/api/voting/status" 2>/dev/null || true)
        if [[ $status == *'"event":"rehearsal"'* && $status == *'"state":"preparing"'* ]]; then
            echo 'Rehearsal ready.'
            exit 0
        fi
        sleep 1
    done
fi
install -o root -g root -m 0600 "$backup" "$settings"
systemctl restart megalove
echo 'Could not select rehearsal. Previous settings restored.' >&2
exit 1
