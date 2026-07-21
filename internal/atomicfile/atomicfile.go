// Package atomicfile writes a file atomically: a temp file in the destination
// directory, then a rename, so a crash mid-write never leaves a truncated file.
// It is the single home for the temp-write-rename dance the profile store and the
// serve daemon's snapshot store both need.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write creates data at path atomically with the given file permission. The
// destination directory must already exist. On any failure the temp file is
// removed and the original path is left untouched.
func Write(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("committing file: %w", err)
	}
	return nil
}
