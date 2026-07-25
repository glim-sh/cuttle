set shell := ["bash", "-euo", "pipefail", "-c"]

binary := "cuttle"

[private]
default:
    @just --list --unsorted

# ── Code Quality ──────────────────────────────────────────

# Format all Go code
[group('quality')]
fmt:
    golangci-lint fmt ./...

# Check formatting without modifying (CI-safe)
[group('quality')]
fmt-check:
    gofumpt -d . 2>&1 | (! grep -q '^') || (gofumpt -l . && exit 1)

# Run linter
[group('quality')]
lint:
    golangci-lint run ./...

# Run linter with auto-fix
[group('quality')]
lint-fix:
    golangci-lint run --fix ./...

# Run vulnerability check
[group('quality')]
vuln:
    govulncheck ./...

# ── Testing ───────────────────────────────────────────────

# Run all tests with race detection
[group('test')]
test *args="./...":
    gotestsum --format testname -- -race {{ args }}

# Run tests with coverage
[group('test')]
test-cov:
    gotestsum --format testname -- -race -coverprofile=coverage.out -covermode=atomic ./...
    go tool cover -func=coverage.out

# ── Build ─────────────────────────────────────────────────

# Build the binary
[group('build')]
build:
    go build -o {{ binary }} ./cmd/{{ binary }}

# Build optimized release binary
[group('build')]
build-release:
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o {{ binary }} ./cmd/{{ binary }}

# Build the container image for the host arch (amd64 -> Windows persona; on an
# Apple Silicon Mac -> arm64/macOS persona, native). CI builds+pushes both arches
# as one multi-arch manifest. BuildKit is required so the per-Dockerfile
# ops/docker/Dockerfile.dockerignore is honored (there is no root .dockerignore);
# classic builder would send the whole repo.
[group('build')]
build-image tag="cuttle:local":
    DOCKER_BUILDKIT=1 docker build -f ops/docker/Dockerfile -t {{ tag }} .

# ── Dependencies ──────────────────────────────────────────

# Tidy and verify modules
[group('deps')]
tidy:
    go mod tidy
    go mod verify

# ── CI ────────────────────────────────────────────────────

# Full CI gate (format check + lint + test)
[group('ci')]
check: fmt-check lint test
    @echo "All checks passed"

# Regenerate the fingerprint parity golden snapshot from the Go primitives
[group('ci')]
parity-golden:
    GOTOOLCHAIN=auto go test ./internal/fingerprint -run TestGolden -update

# Validate the GoReleaser config (lives under ops/config, not repo root)
[group('ci')]
release-check:
    goreleaser check --config ops/config/goreleaser.yaml

# Clean build artifacts
[group('ci')]
clean:
    go clean
    rm -f {{ binary }} coverage.out
