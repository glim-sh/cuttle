// Package cli wires the cuttle command tree. Subcommands are registered on the
// root command by later phases via [AddCommand].
package cli

import (
	"github.com/spf13/cobra"
)

// devVersion is the sentinel for a build with no -ldflags stamp (a source
// build); it points defaultImage() at the local-build tag rather than a
// version-matched published one. There is no build-info fallback: the module
// sits under packages/cuttle, which `go install ...@latest` cannot resolve, so
// every real release goes through the stamped GoReleaser/Docker builds.
const devVersion = "dev"

// version is set at build time via -ldflags (GoReleaser + the Docker build). A
// const initializer keeps it a valid `-X ...cli.version=X` target.
var version = devVersion

func cliVersion() string { return version }

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cuttle",
		Short:         "A browser for agents: websites do not block it, logins persist, a person can take over",
		Version:       cliVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// --context/--name select the instance, not the verb, so they live here and
	// every subcommand inherits them.
	addInstanceFlags(root)
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
