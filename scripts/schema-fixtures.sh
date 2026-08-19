#!/usr/bin/env bash
#
# Generates the stored-schema upgrade fixtures used by db/schema_upgrade_test.go (TODO item 78).
#
# One fixture per released SQLite schema. Each is produced by BUILDING AND RUNNING THE RELEASED
# BINARY and seeding it over its own HTTP gateway — never by a hand-written CREATE TABLE, which
# would be a second copy of the schema and would drift exactly as every other copy in this repo has.
#
# Re-run only when a new release changes the schema; the fixtures are committed, and the test reads
# them rather than calling this. See db/testdata/schema/README.md.
#
# The three drivers have three separate schema-init functions (initSchema, initPostgresSchema,
# initMySQLSchema), each with its own migration list, so each needs its own fixtures. SQLite's is a
# database file; the server drivers' are SQL dumps taken from the server itself with pg_dump /
# mysqldump, normalised to one statement per line so the test can replay them over database/sql
# with no client binary of its own.
#
# The dump tools live in the test containers rather than on the host, so this script shells into
# them - which is why the CONTAINER RUNTIME DEPENDENCY IS HERE AND NOT IN THE TEST. The test needs
# nothing but HIPPOCAMPUS_TEST_POSTGRES_DSN / HIPPOCAMPUS_TEST_MYSQL_DSN, so it runs unchanged
# against CI's existing service containers.
#
# Usage: scripts/schema-fixtures.sh [--driver sqlite|postgres|mysql|all] [tag ...]
#
# Container wiring is overridable by environment; the defaults match the containers the repo's
# integration tests already use (hippo-test-pg, hippo-test-my).

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$(pwd)"

# The last tag of each distinct schema — i.e. the version somebody would actually be upgrading FROM.
# Every tag between two entries creates a byte-identical schema, so one fixture covers the band.
#   v0.4.0  — before the significance registry (migrateSignificanceToLevels, the one DATA migration)
#   v0.22.0 — before memories.is_compressed
#   v0.23.0 — before the FTS content index (initContentSearch backfills a non-empty store)
#   v0.25.0 — before links and metadata (initLinkTables, dropLegacyRelationshipColumns,
#             link_significance and metadata on both tables — the NULL-vs-'' trap)
#   v0.31.0 — before the forgotten log (initTombstones)
#   v0.34.0 — current release; the control, and the only fixture that should migrate to a no-op
DEFAULT_TAGS=(v0.4.0 v0.22.0 v0.23.0 v0.25.0 v0.31.0 v0.34.0)

DRIVERS=(sqlite)
TAGS=()

while [ $# -gt 0 ]; do
  case "$1" in

    --driver)
      case "$2" in

        all)
          DRIVERS=(sqlite postgres mysql)
          ;;

        sqlite | postgres | mysql)
          DRIVERS=("$2")
          ;;

        *)
          echo "unknown driver: $2" >&2

          exit 2
          ;;

      esac

      shift 2
      ;;

    *)
      TAGS+=("$1")

      shift
      ;;

  esac
done

[ ${#TAGS[@]} -eq 0 ] && TAGS=("${DEFAULT_TAGS[@]}")

# The container each dump tool lives in, and how to reach the same server from the host. Overridable
# so this is not welded to one machine's container names.
RUNTIME="${FIXTURE_RUNTIME:-podman}"
PG_CONTAINER="${FIXTURE_PG_CONTAINER:-hippo-test-pg}"
PG_HOST_PORT="${FIXTURE_PG_PORT:-55432}"
PG_USER="${FIXTURE_PG_USER:-test}"
PG_PASSWORD="${FIXTURE_PG_PASSWORD:-test}"
MY_CONTAINER="${FIXTURE_MY_CONTAINER:-hippo-test-my}"
MY_HOST_PORT="${FIXTURE_MY_PORT:-53306}"
MY_USER="${FIXTURE_MY_USER:-root}"
MY_PASSWORD="${FIXTURE_MY_PASSWORD:-test}"

# The scratch database each server fixture is built in, dropped and recreated per tag. Deliberately
# not the database the integration tests use: this one is created and destroyed.
SCRATCH_DB="${FIXTURE_SCRATCH_DB:-hippocampus_fixture}"

WORK="$(mktemp -d)"
WORKTREE="$WORK/src"
GRPC_PORT=50999
HTTP_PORT=8099
BASE="http://127.0.0.1:$HTTP_PORT"

cleanup() {
  [ -n "${SVC_PID:-}" ] && kill "$SVC_PID" 2>/dev/null || true
  git -C "$REPO" worktree remove --force "$WORKTREE" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# post PATH JSON — fails the script on a non-2xx, since a silently unseeded fixture is worse than
# no fixture at all: it would migrate cleanly and assert nothing.
post() {
  local path="$1" body="$2" code
  code=$(curl -sS -o "$WORK/resp" -w '%{http_code}' -X POST "$BASE$path" \
    -H 'content-type: application/json' -d "$body")

  if [ "$code" != "200" ]; then
    echo "  seed POST $path failed ($code): $(cat "$WORK/resp")" >&2

    return 1
  fi
}

# seed writes the fixture dataset. Every field used here exists unchanged from v0.4.0 to HEAD
# (field numbers are preserved across the contract's renames), so one seed serves every tag.
#
# Timestamps are fixed rather than relative so a fixture is reproducible and the test can assert
# exact values. 2026-01-01T00:00:00Z = 1767225600000000000ns.
seed() {
  local t=1767225600000000000
  local day=86400000000000

  # An event with memories written as one nested StoreEvent, the path that stamps the event id
  # onto each memory.
  post /v1/events "$(cat <<JSON
{"id":"evt-alpha","time_start":$t,"significance":60,"name":"alpha event",
 "description":"seeded fixture event","group":"alpha",
 "memories":[
   {"id":"mem-alpha-1","time_stamp":$t,"significance":50,"body":"alpha one: the quick brown fox"},
   {"id":"mem-alpha-2","time_stamp":$((t + day)),"significance":20,"body":"alpha two: jumps over the lazy dog"},
   {"id":"mem-alpha-3","time_stamp":$((t + 2*day)),"significance":80,"body":"alpha three: pack my box with five dozen liquor jugs"}
 ]}
JSON
  )"

  # A second event, ended, in another group — so group scoping and time_end both have something
  # to read on the migrated store.
  post /v1/events "$(cat <<JSON
{"id":"evt-beta","time_start":$((t + 3*day)),"time_end":$((t + 4*day)),"significance":30,
 "name":"beta event","description":"an ended event","group":"beta"}
JSON
  )"

  post /v1/memories "$(cat <<JSON
{"id":"mem-beta-1","time_stamp":$((t + 3*day)),"significance":40,"event_id":"evt-beta",
 "body":"beta one: sphinx of black quartz judge my vow","group":"beta"}
JSON
  )"

  # Loose memories, no event — the first consolidation pass's population.
  post /v1/memories "$(cat <<JSON
{"id":"mem-loose-1","time_stamp":$((t + 5*day)),"significance":70,
 "body":"loose one: how vexingly quick daft zebras jump","group":"alpha"}
JSON
  )"

  post /v1/memories "$(cat <<JSON
{"id":"mem-loose-2","time_stamp":$((t + 6*day)),"significance":10,
 "body":"loose two: a low-significance memory that decay should reach first"}
JSON
  )"

  # A binary body — never content-indexed, and the row is_compressed must not touch.
  post /v1/memories "$(cat <<JSON
{"id":"mem-binary","time_stamp":$((t + 7*day)),"significance":45,"is_binary":"TRUE",
 "body":"YmluYXJ5IHBheWxvYWQ=","group":"alpha"}
JSON
  )"

  # A body long enough to clear storage.compression.minBytes on the versions that compress, so a
  # fixture from >= v0.23.0 carries at least one is_compressed row and HEAD must read both kinds.
  post /v1/memories "$(cat <<JSON
{"id":"mem-long","time_stamp":$((t + 8*day)),"significance":55,"group":"alpha",
 "body":"$(printf 'the compressible body repeats itself so gzip has something to find. %.0s' {1..40})"}
JSON
  )"

  # Recall one, so the fixture carries a non-zero recall_count/time_recalled for the decay clock
  # to age from after migration.
  post /v1/memories/recall '{"ids":["mem-alpha-3"]}'
}

# storage_block emits the storage stanza for a driver, plus the sleep/consolidation settings every
# run shares. minimumAgeInDays is absurd and the capacity limits are off so nothing the seed writes
# can be consolidated or evicted before shutdown; the timed sleep cycle is disabled outright (a
# non-positive period has meant "no timed sleep" since 19.3).
storage_block() {
  case "$1" in

    sqlite)
      echo "\"storage\": { \"driver\": \"sqlite\", \"directory\": \"$DATA\" },"
      ;;

    postgres)
      echo "\"storage\": { \"driver\": \"postgres\", \"postgres\": { \"dsn\": \"postgres://$PG_USER:$PG_PASSWORD@127.0.0.1:$PG_HOST_PORT/$SCRATCH_DB?sslmode=disable\" } },"
      ;;

    mysql)
      echo "\"storage\": { \"driver\": \"mysql\", \"mysql\": { \"dsn\": \"$MY_USER:$MY_PASSWORD@tcp(127.0.0.1:$MY_HOST_PORT)/$SCRATCH_DB?parseTime=true\" } },"
      ;;

  esac
}

# reset_scratch drops and recreates the scratch database, so each tag starts from nothing. An
# existing database would let one tag's schema survive into the next fixture, which is the one
# failure this whole exercise would not notice.
reset_scratch() {
  case "$1" in

    postgres)
      "$RUNTIME" exec -e PGPASSWORD="$PG_PASSWORD" "$PG_CONTAINER" \
        psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -q \
        -c "DROP DATABASE IF EXISTS $SCRATCH_DB" -c "CREATE DATABASE $SCRATCH_DB" > /dev/null
      ;;

    mysql)
      "$RUNTIME" exec "$MY_CONTAINER" \
        mysql -u"$MY_USER" -p"$MY_PASSWORD" \
        -e "DROP DATABASE IF EXISTS $SCRATCH_DB; CREATE DATABASE $SCRATCH_DB" 2> /dev/null
      ;;

  esac
}

# dump_scratch writes the seeded scratch database to stdout as SQL. --inserts /
# --skip-extended-insert keep the data as ordinary INSERT statements rather than a COPY stream or
# one enormous row list, because the test replays these over database/sql and has no client binary
# to feed a COPY to.
dump_scratch() {
  case "$1" in

    postgres)
      "$RUNTIME" exec -e PGPASSWORD="$PG_PASSWORD" "$PG_CONTAINER" \
        pg_dump -U "$PG_USER" --inserts --no-owner --no-privileges --no-comments "$SCRATCH_DB"
      ;;

    mysql)
      "$RUNTIME" exec "$MY_CONTAINER" \
        mysqldump -u"$MY_USER" -p"$MY_PASSWORD" --skip-extended-insert --no-tablespaces \
        --skip-comments --compact --skip-set-charset "$SCRATCH_DB" 2> /dev/null
      ;;

  esac
}

# normalise_sql turns a dump into ONE STATEMENT PER LINE, which is the whole reason the test needs
# no SQL parser: it reads lines and executes them. Comments, blank lines, MySQL's /*! ... */
# executable comments and Postgres's session SET/SELECT preamble are dropped - none of them describe
# the schema, and several are not valid to send over database/sql.
#
# Statement termination tracks single-quote state rather than just looking for a trailing semicolon,
# so a body containing one cannot split a statement in half.
normalise_sql() {
  python3 -c '
import re, sys

# Lines that describe no schema. The backslash ones are psql META-COMMANDS (recent pg_dump wraps
# its output in \restrict/\unrestrict): they are not SQL at all and database/sql cannot send them.
skip = re.compile(r"^\s*(--|/\*|\\|SET\s|SELECT pg_catalog\.set_config|BEGIN;|COMMIT;)", re.I)

statement = []
quoted = False

for line in sys.stdin:
    line = line.rstrip("\n")

    if not statement and (not line.strip() or skip.match(line)):
        continue

    statement.append(line.strip())

    # Walk the line tracking quote state; a doubled quote inside a string is an escaped quote and
    # leaves the state unchanged.
    index = 0
    while index < len(line):
        if line[index] == "\x27":
            if quoted and index + 1 < len(line) and line[index + 1] == "\x27":
                index += 2

                continue

            quoted = not quoted

        index += 1

    if not quoted and line.rstrip().endswith(";"):
        print(" ".join(part for part in statement if part))
        statement = []

if statement:
    sys.exit("unterminated SQL statement at end of dump: " + " ".join(statement)[:200])
'
}

mkdir -p db/testdata/schema

for driver in "${DRIVERS[@]}"; do
  for tag in "${TAGS[@]}"; do
    echo "==> $tag ($driver)"

    rm -rf "$WORKTREE"
    git -C "$REPO" worktree remove --force "$WORKTREE" 2>/dev/null || true
    git -C "$REPO" worktree add --detach --quiet "$WORKTREE" "$tag"

    echo "  building"
    (cd "$WORKTREE" && unset GOROOT && go build -o "$WORK/hippocampus" ./cmd/hippocampus)

    DATA="$WORK/data"
    rm -rf "$DATA"
    mkdir -p "$DATA"

    reset_scratch "$driver"

    cat > "$WORK/config.json" <<JSON
{
  "logging": { "level": "warn", "json": false },
  "port": $GRPC_PORT,
  "gateway": { "port": $HTTP_PORT },
  "auth": { "method": "none" },
  $(storage_block "$driver")
  "sleep": { "periodSeconds": 0 },
  "stats": { "intervalSeconds": 0 },
  "consolidation": {
    "enabled": true,
    "method": 1,
    "aggressiveness": 1.0,
    "unitsOfAgeInDays": 1.0,
    "minimumAgeInDays": 36500,
    "minimumRetentionInDays": 36500,
    "capacityMemories": 0,
    "capacityBytes": 0,
    "walTriggerBytes": 0
  }
}
JSON

    echo "  starting"
    "$WORK/hippocampus" -c "$WORK/config.json" > "$WORK/service.log" 2>&1 &
    SVC_PID=$!

    for _ in $(seq 1 50); do
      curl -sf "$BASE/healthz" > /dev/null 2>&1 && break
      sleep 0.2
    done

    if ! curl -sf "$BASE/healthz" > /dev/null 2>&1; then
      echo "  service never became healthy; log follows" >&2
      cat "$WORK/service.log" >&2

      exit 1
    fi

    echo "  seeding"
    seed

    echo "  stopping"
    kill -TERM "$SVC_PID"
    wait "$SVC_PID" 2>/dev/null || true
    SVC_PID=""

    OUT="db/testdata/schema/$tag"
    mkdir -p "$OUT"

    case "$driver" in

      sqlite)
        # A leftover -wal means the last connection did not checkpoint, and the fixture would carry
        # its rows outside the file we commit.
        if [ -e "$DATA/hippocampus.db-wal" ]; then
          echo "  WAL still present after shutdown — refusing to commit a partial fixture" >&2

          exit 1
        fi

        cp "$DATA/hippocampus.db" "$OUT/hippocampus.db"
        ARTEFACT="$OUT/hippocampus.db"
        ;;

      *)
        # Provenance rides in the file rather than in SOURCE beside it, so regenerating one
        # driver's fixtures leaves the other drivers' files untouched. The test skips comment lines.
        {
          echo "-- generated by scripts/schema-fixtures.sh from $tag ($(git -C "$REPO" rev-parse --short "$tag^{commit}")) on $(date -u +%Y-%m-%dT%H:%M:%SZ)"
          echo "-- one statement per line; replayed by db/schema_upgrade_test.go. Do not hand-edit."
          dump_scratch "$driver" | normalise_sql
        } > "$OUT/$driver.sql"

        ARTEFACT="$OUT/$driver.sql"

        if [ "$(grep -cv '^--' "$ARTEFACT")" -lt 2 ]; then
          echo "  the $driver dump carries no statements — refusing to commit it" >&2

          exit 1
        fi

        reset_scratch "$driver"
        ;;

    esac

    if [ "$driver" = sqlite ]; then
      cat > "$OUT/SOURCE" <<META
tag:       $tag
commit:    $(git -C "$REPO" rev-parse "$tag^{commit}")
generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)
by:        scripts/schema-fixtures.sh
META
    fi

    echo "  wrote $ARTEFACT ($(wc -c < "$ARTEFACT" | tr -d ' ') bytes)"
  done
done

echo "done"
