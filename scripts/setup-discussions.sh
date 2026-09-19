#!/usr/bin/env bash
#
# Turn Discussions on for every repository in the Hippocampus family, and seed each one with a
# single pinned-worthy welcome post in Announcements.
#
# WHY A SEED POST IS PART OF "SETTING IT UP". Enabling Discussions creates the default categories
# and nothing else, and an empty Discussions tab is worse than an absent one: it reads as a channel
# nobody is listening on, so the first person with a question opens an issue instead - which is the
# outcome the tab exists to avoid. The post below says where the family's discussion actually
# happens, which for every repository but `hippocampus` is: not here.
#
# THAT IS THE SHAPE OF THIS FAMILY. Six of the seven repositories are thin satellites of one
# service - a Homebrew tap, a generator, three adapters and a demo site - and splitting conversation
# seven ways across them would mean six tabs with one thread each. So each satellite's welcome post
# points at the service's Discussions for anything about Hippocampus itself, and keeps its own tab
# for what is genuinely local to that repository.
#
# Idempotent: a repository that already has a discussion titled "Welcome" is left alone, so this is
# safe to re-run after adding a repository to the list.
#
#   GITHUB_TOKEN=<token with `administration: write` and `discussions: write`> scripts/setup-discussions.sh
#   scripts/setup-discussions.sh --dry-run
#
set -euo pipefail

OWNER="fastbean-au"
HUB="hippocampus"
TITLE="Welcome"

REPOS=(
	hippocampus
	hippocampus-demo-site
	hippocampus-gen
	hippocampus-llamaindex
	hippocampus-obsidian
	hippocampus-otel-collector
	homebrew-tap
)

DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then
	DRY_RUN=1
	shift
fi

if [[ $# -gt 0 ]]; then
	REPOS=("$@")
fi

hub_body() {
	cat <<'MD'
This is the discussion space for **Hippocampus** and for every repository in its family — the
service itself, the demo site, the generators, the Homebrew tap, and the LlamaIndex, Obsidian and
OpenTelemetry adapters. The satellites have their own tabs for anything local to them, but anything
about the service, its contract, its configuration or its behaviour is best asked here, where it is
in front of everyone.

Some things that are a good fit:

- **Q&A** — how to configure decay for a workload, what a number in the console is telling you, why
  something was forgotten that you expected to survive.
- **Ideas** — a store shape, an integration, or an RPC that would make the service fit a case it
  currently does not.
- **Show and tell** — what you are pointing it at, and what it kept.

Bugs and concrete feature requests are better as [issues](../../issues); anything with a security
dimension belongs in a private report rather than here — see `SECURITY.md`.
MD
}

satellite_body() {
	cat <<MD
This repository is part of the **Hippocampus** family. Its Discussions tab is here for what is
genuinely local to it — how *this* piece is built, configured or used.

For anything about the service itself — how decay is configured, what a figure means, whether a
behaviour is intended — the conversation is in one place, in front of everyone:

**https://github.com/${OWNER}/${HUB}/discussions**

Bugs and concrete feature requests for this repository are better as [issues](../../issues);
anything with a security dimension belongs in a private report rather than here.
MD
}

if [[ "$DRY_RUN" == "1" ]]; then
	printf 'would enable discussions and seed "%s" on: %s\n\n--- hub post ---\n' "$TITLE" "${REPOS[*]}"
	hub_body
	printf '\n--- satellite post ---\n'
	satellite_body

	exit 0
fi

: "${GITHUB_TOKEN:?set GITHUB_TOKEN to a token with administration:write and discussions:write}"

gh_api() {
	curl -sS -H "Authorization: Bearer ${GITHUB_TOKEN}" \
		-H "Accept: application/vnd.github+json" \
		-H "X-GitHub-Api-Version: 2022-11-28" "$@"
}

graphql() {
	# $1 is a JSON object with `query` and `variables`, already encoded.
	curl -sS -H "Authorization: Bearer ${GITHUB_TOKEN}" \
		-H "Content-Type: application/json" \
		-X POST --data "$1" https://api.github.com/graphql
}

status=0

for repo in "${REPOS[@]}"; do
	# 1. The toggle. REST, and idempotent by nature.
	if ! gh_api -X PATCH "https://api.github.com/repos/${OWNER}/${repo}" \
		-d '{"has_discussions": true}' >/dev/null; then
		printf '%-30s FAILED to enable discussions\n' "$repo"
		status=1

		continue
	fi

	# 2. The seed post. Discussions are GraphQL-only: the repository id, the Announcements category
	#    id, and whether a "Welcome" thread already exists all come from one query.
	body=$([[ "$repo" == "$HUB" ]] && hub_body || satellite_body)

	payload=$(BODY="$body" REPO="$repo" OWNER="$OWNER" TITLE="$TITLE" python3 <<'PY'
import json, os
q = """
query($owner:String!, $repo:String!) {
  repository(owner:$owner, name:$repo) {
    id
    discussionCategories(first: 25) { nodes { id name } }
    discussions(first: 50) { nodes { title } }
  }
}
"""
print(json.dumps({"query": q, "variables": {"owner": os.environ["OWNER"], "repo": os.environ["REPO"]}}))
PY
	)

	info=$(graphql "$payload")

	read -r repo_id cat_id exists <<<"$(REPO_INFO="$info" TITLE="$TITLE" python3 <<'PY'
import json, os
d = json.loads(os.environ["REPO_INFO"])
r = (d.get("data") or {}).get("repository")
if not r:
    print("- - error")
    raise SystemExit

cats = {c["name"]: c["id"] for c in r["discussionCategories"]["nodes"]}
cat = cats.get("Announcements") or cats.get("General") or next(iter(cats.values()), "-")
titles = {n["title"] for n in r["discussions"]["nodes"]}
print(r["id"], cat, "yes" if os.environ["TITLE"] in titles else "no")
PY
	)"

	if [[ "$exists" == "error" ]]; then
		printf '%-30s FAILED to read repository: %s\n' "$repo" "$info"
		status=1

		continue
	fi

	if [[ "$exists" == "yes" ]]; then
		printf '%-30s enabled; welcome post already present\n' "$repo"

		continue
	fi

	create=$(REPO_ID="$repo_id" CAT_ID="$cat_id" TITLE="$TITLE" BODY="$body" python3 <<'PY'
import json, os
m = """
mutation($repoId:ID!, $catId:ID!, $title:String!, $body:String!) {
  createDiscussion(input:{repositoryId:$repoId, categoryId:$catId, title:$title, body:$body}) {
    discussion { url }
  }
}
"""
print(json.dumps({"query": m, "variables": {
    "repoId": os.environ["REPO_ID"], "catId": os.environ["CAT_ID"],
    "title": os.environ["TITLE"], "body": os.environ["BODY"]}}))
PY
	)

	out=$(graphql "$create")
	url=$(RESP="$out" python3 -c '
import json, os
d = json.loads(os.environ["RESP"])
try:
    print(d["data"]["createDiscussion"]["discussion"]["url"])
except Exception:
    print("")
')

	if [[ -n "$url" ]]; then
		printf '%-30s enabled; welcome post %s\n' "$repo" "$url"
	else
		printf '%-30s enabled; FAILED to post welcome: %s\n' "$repo" "$out"
		status=1
	fi
done

exit "$status"
