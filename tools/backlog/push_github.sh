#!/usr/bin/env bash
# Create the labels, milestones and issues on GitHub from the generated payloads.
# Idempotent: existing labels are updated, existing milestones reused, and an issue
# whose MJF id already appears in a title is skipped.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)"
echo "repo: $REPO"

echo "--- labels ---"
created=0; updated=0
while read -r name color desc; do
  if gh label create "$name" --color "$color" --description "$desc" --repo "$REPO" >/dev/null 2>&1; then
    created=$((created+1))
  elif gh label edit "$name" --color "$color" --description "$desc" --repo "$REPO" >/dev/null 2>&1; then
    updated=$((updated+1))
  else
    echo "  FAILED: $name"
  fi
done < <(python3 -I -c "
import json
for l in json.load(open('$HERE/labels.json')):
    print(l['name'], l['color'], l['description'])
")
echo "  created $created, updated $updated"

echo "--- milestones ---"
# gh has no milestone command, so the REST API it is.
python3 -I -c "
import json
for m in json.load(open('$HERE/milestones.json')):
    print(m['title'] + '\t' + m['description'])
" | while IFS=$'\t' read -r title desc; do
  existing=$(gh api "repos/$REPO/milestones?state=all&per_page=100" --jq ".[] | select(.title==\"$title\") | .number" 2>/dev/null | head -1)
  if [ -n "$existing" ]; then
    echo "  exists: $title (#$existing)"
  else
    num=$(gh api "repos/$REPO/milestones" -f title="$title" -f description="$desc" --jq .number 2>/dev/null)
    echo "  created: $title (#$num)"
  fi
done
