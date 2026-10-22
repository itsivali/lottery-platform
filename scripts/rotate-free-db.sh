#!/usr/bin/env bash
# Recreate the Render free Postgres before it expires (free instances expire 30 days after creation, keep no backups,
# and only one may exist per workspace). Phases, run in this order by .github/workflows/db-rotate.yml:
#
#   status   find the database, report plan and days left; writes due=true|false and plan=... to $GITHUB_OUTPUT
#   freeze   suspend the api so nothing is written after the backup
#   backup   pg_dump, prove the dump restores into a throwaway Postgres with identical row counts (exact when
#            FROZEN=true), encrypt it
#   replace  delete the old instance, create a new free one with the same name and settings, restore, compare row
#            counts, point the service's DATABASE_URL at it, resume and deploy, wait for /readyz
#   recover  like replace, from an encrypted dump of an earlier run (when replace failed after the delete)
#   thaw     resume the api (after a failure before the old database was deleted)
#
# Needs: RENDER_API_KEY, RENDER_OWNER_ID, SERVICE_ID, DB_NAME, WORK (a private directory), DB_BACKUP_PASSPHRASE
# (backup/recover), ROTATE_WITHIN_DAYS (status), HEALTH_URL (replace/recover). Docker, curl, jq and openssl.
# The restore uses --no-owner --no-privileges: the new instance runs in single-role mode (docs/runbooks/database-roles.md).
set -euo pipefail

API=https://api.render.com/v1
PG_IMAGE=postgres:18
: "${WORK:?}" "${RENDER_API_KEY:?}" "${DB_NAME:?}" "${SERVICE_ID:?}" "${RENDER_OWNER_ID:?}"
mkdir -p "$WORK" && chmod 700 "$WORK"

render() { # render METHOD PATH [JSON]
  local args=(-fsS -X "$1" "$API$2" -H "Authorization: Bearer $RENDER_API_KEY" -H 'Accept: application/json')
  [ $# -ge 3 ] && args+=(-H 'Content-Type: application/json' -d "$3")
  curl "${args[@]}"
}
out() { [ -n "${GITHUB_OUTPUT:-}" ] && echo "$1" >> "$GITHUB_OUTPUT"; echo "$1"; }
psql_url() { docker run --rm -i --network host "$PG_IMAGE" psql "$1" -v ON_ERROR_STOP=1 -At "${@:2}"; }

find_db() { # prints the instance JSON for DB_NAME, or nothing
  render GET "/postgres?name=$DB_NAME&ownerId=$RENDER_OWNER_ID&limit=20" | jq -c --arg n "$DB_NAME" '[.[].postgres | select(.name == $n)][0] // empty'
}
external_url() { render GET "/postgres/$1/connection-info" | jq -r .externalConnectionString; }
internal_url() { render GET "/postgres/$1/connection-info" | jq -r .internalConnectionString; }

# Exact row counts of every table, as "schema.table count" lines (the comparison after a restore).
counts() {
  psql_url "$1" <<'SQL'
SELECT format('SELECT %L, count(*) FROM %I.%I', table_schema || '.' || table_name, table_schema, table_name)
FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY table_schema, table_name \gexec
SQL
}

wait_db_available() {
  for _ in $(seq 1 60); do
    [ "$(render GET "/postgres/$1" | jq -r .status)" = available ] && return 0
    sleep 10
  done
  echo "database $1 did not become available"; return 1
}

wait_deploy_live() {
  local status
  for _ in $(seq 1 60); do
    status=$(render GET "/services/$SERVICE_ID/deploys/$1" | jq -r .status)
    case "$status" in
      live) return 0 ;;
      build_failed|update_failed|pre_deploy_failed|canceled|deactivated) echo "deploy $1: $status"; return 1 ;;
    esac
    sleep 15
  done
  echo "deploy $1 not live after 15 minutes"; return 1
}

restore_into() { # restore_into URL DUMP
  docker run --rm -i --network host -v "$(dirname "$2"):/w:ro" "$PG_IMAGE" \
    pg_restore --dbname "$1" --no-owner --no-privileges --exit-on-error "/w/$(basename "$2")"
}

phase_status() {
  local db; db=$(find_db)
  [ -n "$db" ] || { echo "no database named $DB_NAME"; out "due=false"; out "plan=missing"; return 0; }
  echo "$db" > "$WORK/old.json"
  local plan expires left
  plan=$(jq -r .plan <<<"$db"); expires=$(jq -r '.expiresAt // empty' <<<"$db")
  out "plan=$plan"
  if [ "$plan" != free ] || [ -z "$expires" ]; then
    echo "$DB_NAME is on plan $plan with no expiry: nothing to rotate"; out "due=false"; return 0
  fi
  left=$(( ($(date -d "$expires" +%s) - $(date +%s)) / 86400 ))
  echo "$DB_NAME ($(jq -r .id <<<"$db")) expires $expires: $left day(s) left"
  if [ "$left" -le "${ROTATE_WITHIN_DAYS:-3}" ]; then out "due=true"; else out "due=false"; fi
}

phase_freeze() {
  render POST "/services/$SERVICE_ID/suspend" >/dev/null
  for _ in $(seq 1 30); do
    [ "$(render GET "/services/$SERVICE_ID" | jq -r .suspended)" = suspended ] && break
    sleep 5
  done
  sleep 20 # let in-flight requests and in-process jobs finish before the dump
  echo "api suspended"
}
phase_thaw()   { render POST "/services/$SERVICE_ID/resume" >/dev/null; echo "api resumed"; }

phase_backup() {
  : "${DB_BACKUP_PASSPHRASE:?}"
  local db id src
  db=$(find_db); id=$(jq -r .id <<<"$db"); echo "$db" > "$WORK/old.json"
  src=$(external_url "$id")
  docker run --rm --network host -v "$WORK:/w" "$PG_IMAGE" pg_dump "$src" -Fc --no-owner --no-privileges -f /w/db.dump
  counts "$src" > "$WORK/counts.src"
  echo "dumped $(wc -c < "$WORK/db.dump") bytes, $(wc -l < "$WORK/counts.src") tables"

  # prove the dump restores before anything is deleted
  docker rm -f rotate-verify >/dev/null 2>&1 || true
  docker run -d --name rotate-verify --network host -e POSTGRES_PASSWORD=verify -e PGPORT=55432 "$PG_IMAGE" >/dev/null
  trap 'docker rm -f rotate-verify >/dev/null 2>&1 || true' RETURN
  for _ in $(seq 1 30); do docker exec rotate-verify pg_isready -h 127.0.0.1 -p 55432 >/dev/null 2>&1 && break; sleep 1; done
  local verify=postgres://postgres:verify@127.0.0.1:55432/postgres
  restore_into "$verify" "$WORK/db.dump"
  counts "$verify" > "$WORK/counts.verify"
  # With the api frozen (a real rotation) nothing can change between the dump and the count, so they must match
  # exactly. In a dry run the api keeps writing (job queue rows, say), so a difference is only reported.
  if ! diff -u "$WORK/counts.src" "$WORK/counts.verify"; then
    if [ "${FROZEN:-false}" = true ]; then echo "verification restore differs from the frozen source"; return 1; fi
    echo "note: the live database changed during the dry-run backup (the api is not frozen in a dry run)"
  else
    echo "verification restore matches the source"
  fi
  # the dump's own content is what the new database must match
  mv "$WORK/counts.verify" "$WORK/counts.src"

  openssl enc -aes-256-cbc -pbkdf2 -iter 600000 -salt -pass env:DB_BACKUP_PASSPHRASE -in "$WORK/db.dump" -out "$WORK/db.dump.enc"
  mkdir -p "$WORK/backup"
  cp "$WORK/old.json" "$WORK/counts.src" "$WORK/backup/"
  mv "$WORK/db.dump.enc" "$WORK/backup/"
  echo "encrypted backup ready in $WORK/backup"
}

create_like() { # create a new instance with the old one's settings; free-tier limit: retry until the old one is gone
  local old=$1 body new
  body=$(jq -c --arg owner "$RENDER_OWNER_ID" '{name, ownerId: $owner, plan, version, region, databaseName, databaseUser,
    ipAllowList: [.ipAllowList[] | {cidrBlock, description}]}' "$old")
  for _ in $(seq 1 40); do
    if new=$(render POST /postgres "$body" 2>/dev/null); then jq -r .id <<<"$new"; return 0; fi
    sleep 15
  done
  echo "could not create the new database"; return 1
}

point_service_at() { # point_service_at DB_ID
  local url; url=$(internal_url "$1")
  render PUT "/services/$SERVICE_ID/env-vars/DATABASE_URL" "$(jq -nc --arg v "$url" '{value: $v}')" >/dev/null
  phase_thaw
  local deploy; deploy=$(render POST "/services/$SERVICE_ID/deploys" '{}' | jq -r .id)
  wait_deploy_live "$deploy"
  for _ in $(seq 1 20); do curl -fsS "${HEALTH_URL:?}" >/dev/null && { echo "api ready on $1"; return 0; }; sleep 6; done
  echo "api not ready after the switch"; return 1
}

restore_and_switch() { # restore_and_switch DUMP OLD_JSON COUNTS
  local id new
  id=$(find_db | jq -r '.id // empty')
  if [ -z "$id" ]; then id=$(create_like "$2"); echo "created $id"; fi
  wait_db_available "$id"
  new=$(external_url "$id")
  restore_into "$new" "$1"
  counts "$new" > "$WORK/counts.new"
  diff -u "$3" "$WORK/counts.new" || { echo "restored database differs from the backup"; return 1; }
  echo "restored $(wc -l < "$3") tables with identical row counts"
  point_service_at "$id"
}

phase_replace() {
  local old_id; old_id=$(jq -r .id "$WORK/old.json")
  [ "$(find_db | jq -r .id)" = "$old_id" ] || { echo "the database changed since the backup; stopping"; return 1; }
  render DELETE "/postgres/$old_id" >/dev/null
  echo "deleted $old_id"
  restore_and_switch "$WORK/db.dump" "$WORK/old.json" "$WORK/counts.src"
}

phase_recover() { # expects the artifact of the failed run in $WORK/backup
  : "${DB_BACKUP_PASSPHRASE:?}"
  openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -pass env:DB_BACKUP_PASSPHRASE -in "$WORK/backup/db.dump.enc" -out "$WORK/db.dump"
  local old_id; old_id=$(jq -r .id "$WORK/backup/old.json")
  if [ "$(find_db | jq -r '.id // empty')" = "$old_id" ]; then
    echo "the original database still exists: nothing to recover (resume with the thaw phase)"; return 1
  fi
  restore_and_switch "$WORK/db.dump" "$WORK/backup/old.json" "$WORK/backup/counts.src"
}

case "${1:-}" in
  status|freeze|thaw|backup|replace|recover) "phase_$1" ;;
  *) echo "usage: $0 status|freeze|backup|replace|recover|thaw"; exit 2 ;;
esac
