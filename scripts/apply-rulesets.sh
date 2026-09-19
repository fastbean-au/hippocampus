#!/usr/bin/env bash
#
# Apply the `protect-main` branch ruleset to every repository in the Hippocampus family.
#
# WHAT IT PROTECTS AGAINST, and - more importantly - what it deliberately does not.
#
# These repositories are pushed to directly. `main` is the working branch, work is rebased onto it
# rather than merged from a fork, and release tags are re-pointed when a rebase moves the commit
# they name. A ruleset written the way a multi-team repository writes one - required pull requests,
# required status checks, required linear history - would not harden that workflow, it would end
# it, and the predictable outcome is a bypass actor granted to the only person who pushes, which
# protects nothing at all.
#
# So this ruleset carries exactly the two rules that cost a direct-push workflow nothing and cover
# the two ways `main` is actually lost:
#
#   deletion        - the branch cannot be deleted.
#   non_fast_forward - it cannot be force-pushed, so no history that CI has already reported on can
#                     be silently rewritten out of existence.
#
# And it grants NO bypass actors, which is the whole point: a rule an administrator can step over
# is a note to self, not a control. Neither rule ever refuses an ordinary `git push`.
#
# TAGS ARE DELIBERATELY NOT TARGETED. A tag ruleset with `non_fast_forward` would refuse exactly
# the re-point that a rebase of already-tagged work requires, and that re-point is part of how this
# project releases. Protecting them would trade a real workflow for a theoretical one.
#
# REQUIRED STATUS CHECKS ARE DELIBERATELY ABSENT for the same reason: on a direct push GitHub has
# no completed check run for the commit being pushed, so the rule rejects the push rather than
# gating a merge. It is the right rule for a pull-request workflow and the wrong one for this.
#
# Idempotent: an existing ruleset of the same name is updated in place (PUT) rather than
# duplicated, so re-running after editing RULES below rolls the change out to every repository.
#
#   GITHUB_TOKEN=<token with `administration: write` on these repos> scripts/apply-rulesets.sh
#   scripts/apply-rulesets.sh --dry-run          # print what would be sent, call nothing
#   scripts/apply-rulesets.sh hippocampus        # one repository
#
set -euo pipefail

OWNER="fastbean-au"
NAME="protect-main"

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

read -r -d '' RULES <<'JSON' || true
{
  "name": "protect-main",
  "target": "branch",
  "enforcement": "active",
  "bypass_actors": [],
  "conditions": {
    "ref_name": {
      "include": ["~DEFAULT_BRANCH"],
      "exclude": []
    }
  },
  "rules": [
    { "type": "deletion" },
    { "type": "non_fast_forward" }
  ]
}
JSON

if [[ "$DRY_RUN" == "1" ]]; then
	printf 'would apply to: %s\n\n%s\n' "${REPOS[*]}" "$RULES"

	exit 0
fi

: "${GITHUB_TOKEN:?set GITHUB_TOKEN to a token with administration:write on these repositories}"

api() {
	curl -sS -o /tmp/ruleset-body.$$ -w '%{http_code}' \
		-H "Authorization: Bearer ${GITHUB_TOKEN}" \
		-H "Accept: application/vnd.github+json" \
		-H "X-GitHub-Api-Version: 2022-11-28" \
		"$@"
}

status=0

for repo in "${REPOS[@]}"; do
	base="https://api.github.com/repos/${OWNER}/${repo}/rulesets"

	# Does a ruleset of this name already exist? If so this is an update, not a create.
	code=$(api "$base")
	if [[ "$code" != "200" ]]; then
		printf '%-30s FAILED to list rulesets (HTTP %s): %s\n' "$repo" "$code" "$(cat /tmp/ruleset-body.$$)"
		status=1

		continue
	fi

	id=$(python3 -c '
import json, sys
name = sys.argv[1]
for r in json.load(open(sys.argv[2])):
    if r.get("name") == name:
        print(r["id"])
        break
' "$NAME" "/tmp/ruleset-body.$$")

	if [[ -n "$id" ]]; then
		code=$(api -X PUT "${base}/${id}" -d "$RULES")
		action="updated"
	else
		code=$(api -X POST "$base" -d "$RULES")
		action="created"
	fi

	if [[ "$code" == "200" || "$code" == "201" ]]; then
		printf '%-30s %s\n' "$repo" "$action"
	else
		printf '%-30s FAILED (HTTP %s): %s\n' "$repo" "$code" "$(cat /tmp/ruleset-body.$$)"
		status=1
	fi
done

rm -f "/tmp/ruleset-body.$$"

exit "$status"
