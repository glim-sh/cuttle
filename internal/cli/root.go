// Package cli wires the cuttle command tree. Subcommands are registered on the
// root command by later phases via [AddCommand].
package cli

import (
	"regexp"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

// devVersion is the sentinel for a build with no -ldflags stamp (local build or
// `go install`); cliVersion tries to recover a real tag from the build info.
const devVersion = "dev"

// version is set at build time via -ldflags (GoReleaser + the Docker build). A
// const initializer keeps it a valid `-X ...cli.version=X` target; do not read
// it directly - go through [cliVersion], which also recovers the `go install`
// case.
var version = devVersion

// releaseTag matches a plain release tag. Pseudo-versions ("v0.0.0-2026...-abc")
// and "(devel)" deliberately do not match: they name no published image tag, so
// they must stay "dev" and fall back to :latest.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// cliVersion resolves the CLI version. GoReleaser stamps `version` via -ldflags,
// but `go install` does not - leaving "dev", which would point defaultImage() at
// :latest instead of the matching tag. The go tool records the tag in the build
// info, so recover it from there. It must be resolved at each use-site (not once
// in init) because rootCmd and the subcommand help strings are built during
// package-var init, before any init() runs.
var cliVersion = sync.OnceValue(func() string {
	if version != devVersion {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || !releaseTag.MatchString(info.Main.Version) {
		return devVersion
	}
	return strings.TrimPrefix(info.Main.Version, "v")
})

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cuttle",
		Short:         "Stealth-Chromium browser farm CLI",
		Version:       cliVersion(),
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
