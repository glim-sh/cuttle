#!/usr/bin/env bash
# Puts git-cliff on PATH for a CI step. Deliberately NOT the container action:
# ops/scripts/release-note-from-pr.sh runs inside generation and needs `gh` and
# its token, which the runner has and a container does not.
set -euo pipefail
version=2.13.1
url="https://github.com/orhun/git-cliff/releases/download/v${version}/git-cliff-${version}-x86_64-unknown-linux-gnu.tar.gz"
curl -fsSL "$url" | tar xz -C "$RUNNER_TEMP"
install -m 0755 "$RUNNER_TEMP/git-cliff-${version}/git-cliff" /usr/local/bin/git-cliff
git-cliff --version
