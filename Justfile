set shell := ["bash", "-euo", "pipefail", "-c"]

binary := "cuttle"

# Never in CI: a gate that repairs its own input can only ever pass.
fix := if env('CI', '') == '' { '--fix' } else { '' }

[private]
default:
    @just --list --unsorted

# ── Code Quality ──────────────────────────────────────────

# Lint and format both: gofumpt and goimports are golangci-lint `formatters` here.
[group('quality')]
lint:
    golangci-lint run {{ fix }} ./...

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

# The gate, and the only one: the pre-commit hook and CI both run exactly this.
[group('ci')]
check: lint version-files test
    @echo "All checks passed"

# Both halves of a version-bearing file - annotation AND extra-files entry
[group('ci')]
version-files:
    ./ops/scripts/check-version-files.sh

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
