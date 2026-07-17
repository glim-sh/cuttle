// Package cli wires the cuttle command tree. Subcommands are registered on the
// root command by later phases via [AddCommand].
package cli

import (
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// version is the CLI version, overridden at build time via -ldflags.
var version = "dev"

// releaseTag matches a plain release tag. Pseudo-versions ("v0.0.0-2026...-abc")
// and "(devel)" deliberately do not match: they name no published image tag, so
// they must stay "dev" and fall back to :latest.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// GoReleaser stamps version via -ldflags, but `go install` does not - leaving
// "dev", which points defaultImage() at :latest instead of the matching tag. The
// go tool records the same version in the build info, so recover it from there.
func init() {
	if version != "dev" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || !releaseTag.MatchString(info.Main.Version) {
		return
	}
	version = strings.TrimPrefix(info.Main.Version, "v")
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cuttle",
		Short:         "Stealth-Chromium browser farm CLI",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	return root
}

var rootCmd = newRootCmd()

// AddCommand registers a subcommand on the root command. Later phases call this
// from their package init to plug in verbs (up/down/status/serve/...).
func AddCommand(cmds ...*cobra.Command) {
	rootCmd.AddCommand(cmds...)
}

// Execute runs the root command and returns its error for main to report.
func Execute() error {
	return rootCmd.Execute() //nolint:wrapcheck // cobra prints the error itself
}
