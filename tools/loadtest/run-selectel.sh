#!/usr/bin/env bash
set -euo pipefail
umask 077

dir=${1:?Path to dedicated load test directory required}
db=megalove_loadtest
unit=megalove-loadtest
proxy_unit=megalove-load-proxy
app=/opt/megalove/megalove
base=http://127.0.0.1:8083
dsn='postgres:///megalove_loadtest?host=/var/run/postgresql&user=megalove'
created=0
app_started=0
proxy_started=0
stats_pid=
watch_pid=

cleanup() {
    local code=$?
    trap - EXIT
    if [[ -n $watch_pid ]]; then kill "$watch_pid" 2>/dev/null || true; fi
    if [[ -n $stats_pid ]]; then kill "$stats_pid" 2>/dev/null || true; fi
    if (( proxy_started )); then systemctl stop "$proxy_unit" || true; fi
    if (( app_started )); then systemctl stop "$unit" || true; fi
    if (( created )); then runuser -u postgres -- dropdb "$db" || true; fi
    printf 'EXIT=%d\n' "$code"
    exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
cd /opt

for port in 8082 8083; do
    if ss -lntH "sport = :$port" | grep -q .; then
        printf 'Load test port %s already in use\n' "$port" >&2
        exit 1
    fi
done
if systemctl is-active --quiet "$unit" || systemctl is-active --quiet "$proxy_unit"; then
    echo 'Another load test is running' >&2
    exit 1
fi

echo 'Before test'
nproc
free -m
curl -fsS --max-time 3 -o /dev/null -w 'Afterparty: %{http_code} in %{time_total}s\n' http://127.0.0.1:4318/
curl -fsS --max-time 3 -o /dev/null -w 'Current voting service: %{http_code}\n' http://127.0.0.1:8081/healthz
runuser -u postgres -- createdb --owner=megalove "$db"
created=1
runuser -u megalove -- env "DATABASE_URL=$dsn" "$app" migrate
gzip -dc "$dir/load-fixtures.sql.gz" | runuser -u megalove -- psql -X -v ON_ERROR_STOP=1 -d "$db"
sql() { runuser -u megalove -- psql -X -v ON_ERROR_STOP=1 -Atq -d "$db" -c "$1"; }
rows=$(sql 'SELECT count(*) FROM participants WHERE octet_length(photo_jpeg) = 98304')
[[ $rows = 235 ]]
sql 'SELECT faculty_code, nomination, count(*) FROM participants JOIN candidacies ON candidacies.participant_id=participants.id AND candidacies.event_id=participants.event_id GROUP BY faculty_code, nomination ORDER BY faculty_code, nomination'

load_password=$(openssl rand -hex 24)
hash=$(printf '%s\n' "$load_password" | "$app" admin-hash)
systemd-run --quiet --unit="$unit" --uid=megalove --gid=megalove \
    --setenv="DATABASE_URL=$dsn" --setenv=MEGALOVE_EVENT_SLUG=loadtest \
    --setenv=MEGALOVE_ADDR=127.0.0.1:8082 --setenv=MEGALOVE_VOTE_LOCAL_HTTP=1 \
    --setenv=MEGALOVE_ADMIN_LOCAL_HTTP=1 --setenv="MEGALOVE_ADMIN_HASH=$hash" "$app" serve
app_started=1
unset hash
cat > "$dir/Caddyfile" <<'CADDY'
{
    admin off
}
http://127.0.0.1:8083 {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8082
}
CADDY
systemd-run --quiet --unit="$proxy_unit" /usr/bin/caddy run --config "$dir/Caddyfile" --adapter caddyfile
proxy_started=1
wait_ready() {
    local attempt
    for attempt in {1..15}; do
        if curl -fsS --max-time 2 -o /dev/null "$base/healthz" 2>/dev/null; then return 0; fi
        sleep 1
    done
    return 1
}
wait_ready
jar="$dir/admin.cookies"
login() {
    printf 'password=%s' "$load_password" |
        curl -sS --max-time 8 -o /dev/null -w '%{http_code}' -c "$jar" -d @- "$base/admin/login"
}
[[ $(login) = 303 ]]
opened=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' -b "$jar" -d 'from=preparing&to=open' "$base/admin/voting")
[[ $opened = 303 ]]
rm -f "$jar"

vmstat -w -n 1 > "$dir/vmstat.log" &
stats_pid=$!
(
    bad=0
    while true; do
        result=$(curl -sS --max-time 3 -o /dev/null -w '%{http_code} %{time_total}' http://127.0.0.1:4318/ 2>/dev/null || true)
        available=$(awk '/^MemAvailable:/ { print $2 }' /proc/meminfo)
        printf '%s %s %s\n' "$(date +%s)" "$result" "$available"
        if [[ $result != '200 '* ]] || (( available < 204800 )); then
            bad=$((bad+1))
            if (( bad >= 3 )); then
                echo 'Stopped because Afterparty or memory health check failed' >&2
                kill -TERM "$$"
                break
            fi
        else
            bad=0
        fi
        sleep 1
    done
) > "$dir/health.log" &
watch_pid=$!

ballot_counts() {
    sql 'SELECT count(*), count(DISTINCT browser_id), count(dancer_id), count(vocalist_id), count(actor_id) FROM ballots'
}

echo 'Load profile: 500 viewers'
"$dir/loadtest" -base "$base" -users 500 -arrival 12s -burst 5s | tee "$dir/profile500.log"
[[ $(ballot_counts) = '500|500|500|500|500' ]]
echo 'Load profile: 1000 viewers'
"$dir/loadtest" -base "$base" -users 1000 -arrival 20s -burst 10s | tee "$dir/profile1000.log"
[[ $(ballot_counts) = '1500|1500|1500|1500|1500' ]]

echo 'Closure race'
MEGALOVE_LOAD_PASSWORD=$load_password "$dir/loadtest" -base "$base" -users 120 -close-race | tee "$dir/race.log"
accepted=$(sed -n -E 's/^Race accepted=([0-9]+) blocked=[0-9]+$/\1/p' "$dir/race.log")
[[ $accepted =~ ^[0-9]+$ ]]
total=$((1500 + accepted))
expected="$total|$total|$total|$total|$total"
[[ $(ballot_counts) = "$expected" ]]
[[ $(sql "SELECT state FROM events WHERE slug='loadtest'") = closed ]]

echo 'Restart persistence'
systemctl restart "$unit"
wait_ready
[[ $(ballot_counts) = "$expected" ]]
[[ $(sql "SELECT state FROM events WHERE slug='loadtest'") = closed ]]
[[ $(login) = 303 ]]
curl -fsS --max-time 8 -b "$jar" "$base/admin/results/export" -o "$dir/results.csv"
rm -f "$jar" "$dir/results.csv"
echo "Confirmed totals (ballots/unique/dancer/vocalist/actor): $expected"
curl -fsS --max-time 3 -o /dev/null -w 'Afterparty: %{http_code} in %{time_total}s\n' http://127.0.0.1:4318/
curl -fsS --max-time 3 -o /dev/null -w 'Current voting service: %{http_code}\n' http://127.0.0.1:8081/healthz
awk 'NR > 3 && NF > 16 { if (100-$15 > cpu) cpu=100-$15; if ($1 > queue) queue=$1; if ($16 > wait) wait=$16; n++ } END {printf "vmstat: %d samples, peak non-idle CPU %d%%, runnable peak %d, I/O wait peak %d%%\n",n,cpu,queue,wait}' "$dir/vmstat.log"
awk '$2 == 200 { if (!min || $4 < min) min=$4; if ($3 > max) max=$3; count++ } $2 != 200 {bad++} END {printf "Afterparty: %d checks, %d errors, max %.3fs; min available memory %.1f MiB\n",count,bad,max,min/1024}' "$dir/health.log"
