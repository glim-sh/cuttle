#!/usr/bin/env bash
# Guards the two halves of a version-bearing file, either of which silently does
# nothing without the other:
#
#   1. an `x-release-please-version` comment on the line holding the version, and
#   2. the file listed under `extra-files` in .github/release-please-config.json.
#
# A file annotated but unlisted is never bumped. `ops/helm/cuttle/values.yaml`
# drifted two releases stale that way, pinning a dead image and
# CrashLoopBackOff-ing the k8s backend, with nothing failing anywhere.
#
# It also rejects a YAML/JSON/TOML extra-file listed as a bare string. For those,
# release-please runs TWO updaters: the annotation-based `Generic` one you want,
# and a jsonpath updater hardcoded to `$.version`. The second reserializes the
# document through a dumper that by design removes all comments - including the
# annotation the first updater needs - and it runs first. On a file with a
# top-level `version:` key the result is the exact inverse of the intent: the
# wrong line is bumped, the annotated line is left stale, and every comment is
# stripped. Chart.yaml shipped that way in the 0.5.0 release PR. The
# {"type": "generic"} object form dispatches to `Generic` alone.
set -euo pipefail

cd "$(dirname "$0")/../.."
config=.github/release-please-config.json
fail=0

# The annotation only does anything on a line that also holds a version, which
# is also what separates a real marker from prose mentioning one (this script
# included).
# Tracked files only, symlinks skipped. A recursive scan also reads build output,
# and the `cuttle` binary embeds SKILL.md - annotation included - so an ordinary
# `go build` in the working tree failed this check; the repo root's SKILL.md is a
# symlink to internal/cli/SKILL.md, which grep follows into a duplicate under a
# path release-please is right not to list. The annotation also only does
# anything on a line that holds a version, which is what separates a real marker
# from prose mentioning one (this script included).
annotated=$(git ls-files | while IFS= read -r f; do
  if [ ! -L "$f" ] && grep -qE '[0-9]+\.[0-9]+\.[0-9]+.*x-release-please-version' "$f" 2>/dev/null; then
    echo "$f"
  fi
done | sort)
listed=$(jq -r '.packages["."]["extra-files"][] | if type == "string" then . else .path end' "$config" | sort)

while IFS= read -r f; do
  [ -n "$f" ] || continue
  if ! printf '%s\n' "$listed" | grep -qxF "$f"; then
    echo "$f carries an x-release-please-version annotation but is not in $config extra-files - it will never be bumped" >&2
    fail=1
  fi
done <<EOF
$annotated
EOF

while IFS= read -r f; do
  [ -n "$f" ] || continue
  if [ ! -e "$f" ]; then
    echo "$config lists $f under extra-files, but that file does not exist" >&2
    fail=1
    continue
  fi
  if ! grep -q 'x-release-please-version' "$f"; then
    echo "$config lists $f under extra-files, but it carries no x-release-please-version annotation - the updater has nothing to find" >&2
    fail=1
  fi
done <<EOF
$listed
EOF

bare=$(jq -r '.packages["."]["extra-files"][] | select(type == "string") | select(test("\\.(ya?ml|json|toml)$"))' "$config")
if [ -n "$bare" ]; then
  echo "these YAML/JSON/TOML extra-files are listed as bare strings and must use the {\"type\": \"generic\"} object form, or their comments (annotation included) are stripped:" >&2
  printf '%s\n' "$bare" | sed 's/^/  /' >&2
  fail=1
fi

exit "$fail"
