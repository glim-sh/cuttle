# cuttle - stealth-Chromium CDP farm

image := "cuttle:local"

# List recipes
default:
    @just --list

# Type-check + lint + format the authored Python (test/ + scripts/)
check:
    uv run ty check
    uv run ruff check --fix
    uv run ruff format

# Build the linux/amd64 image (clark/clearcote are linux-x64 only)
build:
    docker buildx build --platform linux/amd64 --load -t {{image}} .

# Build, then run the neutral smoke harness against a throwaway container
smoke: build
    #!/usr/bin/env bash
    set -euo pipefail
    docker rm -f cuttle-smoke >/dev/null 2>&1 || true
    docker run -d --name cuttle-smoke -p 127.0.0.1:19222:9222 --shm-size=2g {{image}} >/dev/null
    trap 'docker rm -f cuttle-smoke >/dev/null 2>&1 || true' EXIT
    for _ in $(seq 1 60); do curl -sf http://127.0.0.1:19222/json/version >/dev/null 2>&1 && break; sleep 1; done
    CUTTLE_URL=http://127.0.0.1:19222 uv run python test/harness.py

# Show drift of the vendored upstream subset (reviewable diff; does not overwrite)
vendor-sync:
    ./scripts/sync.sh

# Remove build artifacts
clean:
    rm -rf build dist *.egg-info .ruff_cache
    find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
