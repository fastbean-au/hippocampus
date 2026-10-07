#!/usr/bin/env bash
# The HTTP/JSON gateway from a shell: store a memory, find it, recall it, ask where it stands, and
# delete it again (TODO-3 item 172). Every RPC is reachable this way under /v1.
#
#   examples/curl/quickstart.sh                       # against http://localhost:8080
#   HIPPOCAMPUS_URL=https://memory.example.com HIPPOCAMPUS_TOKEN=... examples/curl/quickstart.sh
#
# Needs curl and jq. It exits non-zero on the first call that fails, which is what lets CI run it
# against the compose stack as a contract smoke test.
set -euo pipefail

base="${HIPPOCAMPUS_URL:-http://localhost:8080}"

headers=(-H 'Content-Type: application/json')

if [ -n "${HIPPOCAMPUS_TOKEN:-}" ]; then
	headers+=(-H "Authorization: Bearer ${HIPPOCAMPUS_TOKEN}")
fi

call() {
	curl -sf "${headers[@]}" "$@"
}

# Significance is what the decay runs on: higher survives longer.
id=$(call -X POST "$base/v1/memories" \
	-d '{"body":"the 14:03 deploy of the billing service rolled back cleanly","significance":50,"group":"examples"}' |
	jq -er .id)
echo "stored $id"

# Content search, with the same filters the gRPC request carries.
found=$(call -X POST "$base/v1/memories/search" -d '{"query":"billing rollback","group":"examples"}' |
	jq '.memories | length')
echo "search found $found memories"

# A read by id does not reinforce the memory; a recall does, resetting its decay clock.
call "$base/v1/memories?ids=$id" | jq -e --arg id "$id" '.memories[0].id == $id' >/dev/null
count=$(call -X POST "$base/v1/memories/recall" -d "{\"ids\":[\"$id\"]}" | jq -r '.memories[0].recallCount')
echo "recalled; recall count is now $count"

# Where the memory stands against the decay rules, and how long it has left.
call -X POST "$base/v1/consolidation/explain" -d "{\"memoryIds\":[\"$id\"]}" |
	jq -r '"value \(.valuations[0].value) against a threshold of \(.deletionThreshold)"'

call -X POST "$base/v1/memories/delete" -d "{\"ids\":[\"$id\"]}" >/dev/null
echo "deleted $id"
