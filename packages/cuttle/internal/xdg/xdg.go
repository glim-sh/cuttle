// Package xdg resolves XDG base directories with home-directory fallbacks, so
// cuttle's config and data paths are derived in one place instead of each
// package repeating the fallback ladder.
package xdg

import (
	"os"
	"path/filepath"
)

// ConfigDir is $XDG_CONFIG_HOME, or ~/.config when unset. It returns "" only if
// the env var is unset and the home directory cannot be determined.
func ConfigDir() string { return baseDir("XDG_CONFIG_HOME", ".config") }

// DataDir is $XDG_DATA_HOME, or ~/.local/share when unset.
func DataDir() string { return baseDir("XDG_DATA_HOME", filepath.Join(".local", "share")) }

// CacheDir is $XDG_CACHE_HOME, or ~/.cache when unset.
func CacheDir() string { return baseDir("XDG_CACHE_HOME", ".cache") }

// StateDir is $XDG_STATE_HOME, or ~/.local/state when unset.
func StateDir() string { return baseDir("XDG_STATE_HOME", filepath.Join(".local", "state")) }

func baseDir(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, fallback)
	}
	return ""
}
