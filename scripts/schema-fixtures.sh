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
# Usage: scripts/schema-fixtures.sh [tag ...]        (default: every schema-boundary tag)

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
TAGS=("${@:-}")
[ -z "${TAGS[0]:-}" ] && TAGS=("${DEFAULT_TAGS[@]}")

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

mkdir -p db/testdata/schema

for tag in "${TAGS[@]}"; do
  echo "==> $tag"

  rm -rf "$WORKTREE"
  git -C "$REPO" worktree remove --force "$WORKTREE" 2>/dev/null || true
  git -C "$REPO" worktree add --detach --quiet "$WORKTREE" "$tag"

  echo "  building"
  (cd "$WORKTREE" && unset GOROOT && go build -o "$WORK/hippocampus" ./cmd/hippocampus)

  DATA="$WORK/data"
  rm -rf "$DATA"
  mkdir -p "$DATA"

  # A config every version in the range accepts. minimumAgeInDays is absurd and capacity limits are
  # off so that nothing the seed writes can be consolidated or evicted before shutdown; the timed
  # sleep cycle is disabled outright (a non-positive period has meant "no timed sleep" since 19.3).
  cat > "$WORK/config.json" <<JSON
{
  "logging": { "level": "warn", "json": false },
  "port": $GRPC_PORT,
  "gateway": { "port": $HTTP_PORT },
  "auth": { "method": "none" },
  "storage": { "driver": "sqlite", "directory": "$DATA" },
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

  # A leftover -wal means the last connection did not check point, and the fixture would carry its
  # rows outside the file we commit.
  if [ -e "$DATA/hippocampus.db-wal" ]; then
    echo "  WAL still present after shutdown — refusing to commit a partial fixture" >&2

    exit 1
  fi

  OUT="db/testdata/schema/$tag"
  mkdir -p "$OUT"
  cp "$DATA/hippocampus.db" "$OUT/hippocampus.db"

  cat > "$OUT/SOURCE" <<META
tag:       $tag
commit:    $(git -C "$REPO" rev-parse "$tag^{commit}")
generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)
by:        scripts/schema-fixtures.sh
META

  echo "  wrote $OUT/hippocampus.db ($(wc -c < "$OUT/hippocampus.db" | tr -d ' ') bytes)"
done

echo "done"
