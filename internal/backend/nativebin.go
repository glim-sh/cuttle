package backend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/glim-sh/cuttle/internal/fingerprint"
	"github.com/glim-sh/cuttle/internal/xdg"
)

// clark stealth-Chromium darwin-arm64 prebuilt, pinned to the same release the
// docker image bakes (see ops/docker/Dockerfile). The macOS build ships a
// signed Chromium.app whose --fingerprint-* patches are runtime-confirmed on
// Apple Silicon.
const (
	clarkTag       = "chromium-v148.0.7778.96-stealth5"
	clarkAsset     = "clark-browser-darwin-arm64.tar.gz"
	clarkSHA256    = "c3f16e23262d16d8f899414143dadf06f326fa29ab9d24006d28a68cf5fe3040"
	clarkAppBinary = "Chromium.app/Contents/MacOS/Chromium"
)

var (
	errNativeArch     = errors.New("the native backend needs an Apple Silicon (arm64) Mac; clark ships no darwin-amd64 build - use `--context local` for the docker path")
	errClarkChecksum  = errors.New("downloaded clark archive failed its sha256 check")
	errClarkNoBinary  = errors.New("extracted clark archive is missing " + clarkAppBinary)
	errClarkTarEscape = errors.New("clark archive contains a path escaping the extraction dir")
)

// ensureNativeBinary resolves the stealth Chromium the native daemon runs. It
// honors an explicit CUTTLE_BROWSER_BINARY override, else uses a cached clark
// build, else downloads, verifies, and extracts the pinned darwin-arm64 release.
func ensureNativeBinary(ctx context.Context) (string, error) {
	if override := os.Getenv(fingerprint.BinaryPathEnv); override != "" {
		return fingerprint.EnsureBinary() // validates existence, returns the path
	}
	if runtime.GOARCH != "arm64" {
		return "", errNativeArch
	}

	dir := clarkCacheDir()
	binary := filepath.Join(dir, clarkAppBinary)
	if _, err := os.Stat(binary); err == nil {
		return binary, nil
	}

	if err := downloadClark(ctx, dir); err != nil {
		return "", err
	}
	if _, err := os.Stat(binary); err != nil {
		return "", errClarkNoBinary
	}
	return binary, nil
}

func clarkCacheDir() string {
	base := xdg.CacheDir()
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "cuttle", "clark", clarkTag)
}

func clarkURL() string {
	return "https://github.com/clark-labs-inc/clark-browser/releases/download/" +
		clarkTag + "/" + clarkAsset
}

// downloadClark streams the release asset, verifies its sha256, and extracts it
// into dir via a temp dir renamed atomically into place, so a partial or
// corrupt download never leaves a half-usable cache entry.
func downloadClark(ctx context.Context, dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("creating clark cache dir: %w", err)
	}

	fmt.Fprintf(os.Stderr, "cuttle: downloading stealth Chromium (clark %s, ~135MB, first run only)...\n", clarkTag)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clarkURL(), nil)
	if err != nil {
		return fmt.Errorf("clark request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading clark: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading clark: unexpected status %s", resp.Status) //nolint:err113
	}

	// Download to a temp file while hashing the full byte stream, so the sha256
	// is verified before any extraction (tar/gzip can stop reading early, so
	// hashing must happen at the raw-body level, not via the tar reader).
	archive, err := os.CreateTemp(parent, ".clark-dl-*.tar.gz")
	if err != nil {
		return fmt.Errorf("clark download temp: %w", err)
	}
	defer func() { _ = os.Remove(archive.Name()) }()
	hasher := sha256.New()
	if _, cpErr := io.Copy(io.MultiWriter(archive, hasher), resp.Body); cpErr != nil {
		_ = archive.Close()
		return fmt.Errorf("downloading clark: %w", cpErr)
	}
	_ = archive.Close()
	if sum := hex.EncodeToString(hasher.Sum(nil)); sum != clarkSHA256 {
		return fmt.Errorf("%w: got %s", errClarkChecksum, sum)
	}

	tmp, err := os.MkdirTemp(parent, ".clark-extract-*")
	if err != nil {
		return fmt.Errorf("clark temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	f, err := os.Open(archive.Name())
	if err != nil {
		return fmt.Errorf("reopening clark archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := extractTarGz(f, tmp); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(tmp, clarkAppBinary)); err != nil {
		return errClarkNoBinary
	}

	if err := os.Rename(tmp, dir); err != nil {
		// A concurrent run may have populated dir first; that is fine.
		if _, statErr := os.Stat(filepath.Join(dir, clarkAppBinary)); statErr == nil {
			return nil
		}
		return fmt.Errorf("installing clark into cache: %w", err)
	}
	return nil
}

// extractTarGz unpacks a gzipped tar into destDir, handling regular files,
// directories, and symlinks (the .app bundle relies on framework symlinks), with
// a path-traversal guard.
func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("clark gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("clark tar: %w", err)
		}
		target := filepath.Join(destDir, hdr.Name) //nolint:gosec // guarded below
		if !withinDir(destDir, target) {
			return errClarkTarEscape
		}
		if err := extractTarEntry(tr, hdr, destDir, target); err != nil {
			return err
		}
	}
	return nil
}

func extractTarEntry(tr *tar.Reader, hdr *tar.Header, destDir, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o750); err != nil {
			return err //nolint:wrapcheck
		}
		return nil
	case tar.TypeSymlink:
		// Reject a symlink whose target escapes destDir so a later entry cannot
		// write through it to outside the extraction root (defense-in-depth beyond
		// the sha256 pin). The clark bundle uses only relative in-tree links.
		resolved := hdr.Linkname
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(target), resolved) //nolint:gosec // guarded by withinDir on the next line
		}
		if !withinDir(destDir, resolved) {
			return errClarkTarEscape
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err //nolint:wrapcheck
		}
		_ = os.Remove(target)
		return os.Symlink(hdr.Linkname, target) //nolint:wrapcheck
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err //nolint:wrapcheck
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777) //nolint:gosec
		if err != nil {
			return err //nolint:wrapcheck
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err //nolint:wrapcheck
		}
		return f.Close() //nolint:wrapcheck
	default:
		return nil // skip anything exotic (hardlinks/devices are not in this bundle)
	}
}

// withinDir reports whether path resolves inside base (or equals it).
func withinDir(base, path string) bool {
	base = filepath.Clean(base)
	path = filepath.Clean(path)
	return path == base || strings.HasPrefix(path, base+string(os.PathSeparator))
}
