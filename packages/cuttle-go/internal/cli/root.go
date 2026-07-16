// Package cli wires the cuttle command tree. Subcommands are registered on the
// root command by later phases via [AddCommand].
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is the CLI version, overridden at build time via -ldflags.
var version = "dev"

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

// Execute runs the root command, exiting non-zero on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cuttle:", err)
		os.Exit(1)
	}
}
