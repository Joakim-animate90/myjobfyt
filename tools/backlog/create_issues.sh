#!/usr/bin/env bash
# Create the backlog issues. Idempotent: an issue whose MJF id already appears in an
# existing title is skipped, so a partial run can be resumed safely.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)"
WORK="$HERE/.work"; mkdir -p "$WORK"

# One API call for the existing titles rather than one per issue.
gh issue list --repo "$REPO" --state all --limit 500 --json title --jq '.[].title' > "$WORK/existing.txt" 2>/dev/null || : > "$WORK/existing.txt"

python3 -I -c "
import json, os
issues = json.load(open('$HERE/issues.json'))
os.makedirs('$WORK/bodies', exist_ok=True)
for i in issues:
    with open('$WORK/bodies/' + i['id'] + '.md', 'w') as f:
        f.write(i['body'])
    print('\t'.join([i['id'], i['title'], ','.join(i['labels']), i['milestone']]))
" > "$WORK/index.tsv"

created=0; skipped=0; failed=0
while IFS=$'\t' read -r id title labels milestone; do
  if grep -qF "$id" "$WORK/existing.txt"; then
    skipped=$((skipped+1)); continue
  fi
  label_args=()
  IFS=',' read -ra ls <<< "$labels"
  for l in "${ls[@]}"; do label_args+=(--label "$l"); done

  if url=$(gh issue create --repo "$REPO" --title "$title" \
       --body-file "$WORK/bodies/$id.md" \
       "${label_args[@]}" --milestone "$milestone" 2>&1); then
    created=$((created+1))
    printf "  %s -> %s\n" "$id" "$(basename "$url")"
  else
    failed=$((failed+1))
    printf "  FAILED %s: %s\n" "$id" "$(echo "$url" | head -1)"
  fi
  # GitHub rate-limits issue creation; a short pause keeps a 61-issue run clean.
  sleep 1.2
done < "$WORK/index.tsv"

echo "created=$created skipped=$skipped failed=$failed"
