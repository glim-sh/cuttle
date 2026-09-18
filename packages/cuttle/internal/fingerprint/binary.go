package fingerprint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// BinaryPathEnv selects the stealth Chromium fork binary.
const BinaryPathEnv = "CUTTLE_BROWSER_BINARY"

var (
	errBinaryPathUnset   = errors.New(BinaryPathEnv + " is not set; cuttle ships no binary download, point it at a local stealth Chromium build")
	errBinaryPathMissing = errors.New(BinaryPathEnv + " points at a file that does not exist")
)

// EnsureBinary resolves the stealth Chromium binary from CUTTLE_BROWSER_BINARY,
// erroring clearly when the variable is unset or points at a missing file.
func EnsureBinary() (string, error) {
	path := os.Getenv(BinaryPathEnv)
	if path == "" {
		return "", errBinaryPathUnset
	}
	if _, err := os.Stat(path); err != nil { //nolint:gosec // operator-supplied binary path by design
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %q", errBinaryPathMissing, path)
		}
		return "", fmt.Errorf("stat %s: %w", BinaryPathEnv, err)
	}
	return path, nil
}
