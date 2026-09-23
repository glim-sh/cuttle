package cli

import (
	_ "embed"

	"github.com/spf13/cobra"
)

// skillGuide is the full agent-facing cuttle guide, compiled into the binary so
// `cuttle skill` always prints the doc that matches this CLI. go:embed cannot
// reach parent dirs or follow symlinks, so skills/cuttle/SKILL.md - what skill
// installers fetch - is a plain copy, held identical by TestSkillCopyMatches.
//
//go:embed SKILL.md
var skillGuide string

func init() {
	AddCommand(newSkillCmd())
}

func newSkillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "skill",
		Short: "print the full agent-facing cuttle guide",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write([]byte(skillGuide))
			return err //nolint:wrapcheck // cobra prints the error itself
		},
	}
}
