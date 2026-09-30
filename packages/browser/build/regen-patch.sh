#!/usr/bin/env bash
# Regenerate one stealth patch from a base tree and an edited tree, then prove it.
#
# Usage: regen-patch.sh <patch> <base-dir> <new-dir> <path>...
#
#   <patch>     packages/browser/patches/00NN-*.patch, replaced once proven. Its header
#               comment (everything above the first `diff --git` or `--- `) is kept.
#   <base-dir>  the files as they are BEFORE this patch, at their tree paths
#               (<base-dir>/v8/src/...). A path missing here is a new file.
#   <new-dir>   the same paths with the intended edits.
#   <path>...   the tree paths the patch covers.
#
# The proof is the round trip build-linux.sh depends on: the patch applies to a
# copy of the base with `git apply` and yields exactly <new-dir>, and reverses
# back to the base. Then it diffs the identifiers the patch adds against the
# committed version: rebuilding 0016 from a reverted base once dropped a whole
# hunk with nothing failing, and a dropped identifier is how that shows.
set -euo pipefail

if (( $# < 4 )); then
  sed -n '3,11p' "$0" >&2
  exit 2
fi
patch=$1 base=$2 new=$3
shift 3

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

header=""
[[ -f "$patch" ]] && header=$(sed -E '/^(diff --git |--- )/,$d' "$patch")
{
  [[ -z "$header" ]] || printf '%s\n' "$header"
  for f in "$@"; do
    [[ -f "$new/$f" ]] || { echo "regen-patch: $new/$f does not exist" >&2; exit 2; }
    echo "diff --git a/$f b/$f"
    from=$base/$f label=a/$f
    if [[ ! -f "$from" ]]; then
      from=/dev/null label=/dev/null
      echo "new file mode 100644"
    fi
    rc=0
    diff -u --label "$label" --label "b/$f" "$from" "$new/$f" || rc=$?
    (( rc == 1 )) || { echo "regen-patch: $f is unchanged, or diff failed" >&2; exit 2; }
  done
} > "$tmp/patch"
if grep -nE '^@@ -[1-9][0-9]*,0 ' "$tmp/patch" >&2; then
  echo "regen-patch: zero-context insertion, which git apply would land at end of file" >&2
  exit 1
fi

mkdir "$tmp/tree"
for f in "$@"; do
  if [[ -f "$base/$f" ]]; then mkdir -p "$tmp/tree/$(dirname "$f")" && cp "$base/$f" "$tmp/tree/$f"; fi
done
# The ceiling keeps git from finding an enclosing repo, which would make it skip
# every path outside the current directory.
export GIT_CEILING_DIRECTORIES="$tmp"
(cd "$tmp/tree" && git apply "$tmp/patch")
for f in "$@"; do
  cmp "$tmp/tree/$f" "$new/$f" || { echo "regen-patch: $f does not round-trip" >&2; exit 1; }
done
(cd "$tmp/tree" && git apply -R "$tmp/patch")
for f in "$@"; do
  if [[ -f "$base/$f" ]]; then
    cmp "$tmp/tree/$f" "$base/$f" || { echo "regen-patch: $f does not reverse to its base" >&2; exit 1; }
  fi
done
# Only a proven patch replaces the committed one.
cp "$tmp/patch" "$patch"
echo "regen-patch: $(basename "$patch") applies and reverses cleanly"

idents() { grep -E '^\+' | grep -vE '^\+\+\+ ' | grep -oE '[A-Za-z_][A-Za-z0-9_]*' | LC_ALL=C sort -u || true; }
if prev=$(git -C "$(dirname "$patch")" show "HEAD:./$(basename "$patch")" 2>/dev/null); then
  idents <<< "$prev" > "$tmp/prev"
  idents < "$patch" > "$tmp/cur"
  dropped=$(LC_ALL=C comm -23 "$tmp/prev" "$tmp/cur" | tr '\n' ' ')
  added=$(LC_ALL=C comm -13 "$tmp/prev" "$tmp/cur" | tr '\n' ' ')
  echo "regen-patch: identifiers added vs HEAD: ${added:-none}"
  if [[ -n "$dropped" ]]; then
    echo "regen-patch: DROPPED vs HEAD - confirm each is intended: $dropped" >&2
  fi
else
  echo "regen-patch: no committed version to compare identifiers against"
fi
