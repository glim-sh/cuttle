package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/jev"
)

func init() { AddCommand(newJevBrowseCmd()) }

var errJevTextPair = errors.New("--text takes name=value")

type jevBrowseFlags struct {
	task     string
	url      string
	maxSteps int
	extract  string
	text     []string
	json     bool
	mock     bool
}

func newJevBrowseCmd() *cobra.Command {
	var f jevBrowseFlags
	cmd := &cobra.Command{
		Use:   "jev-browse",
		Short: "browse toward a task on cuttle's browser, one model-picked action at a time",
		Long: `Drive cuttle's browser toward a task without an LLM in the loop.

Each step reads the page with the same bundled playwright-cli ` + "`cuttle pw`" + ` runs,
offers its interactive elements to TypeSafe's System-One model, and performs the
one it picks. The model only ever CHOOSES - it generates no text - so a step
costs a fraction of asking an LLM which button to press next.

  cuttle jev-browse --task "find the support phone number" --url https://example.com
  cuttle jev-browse --task "sign in" --text user=qa@example.com --text pass='{{cuttle:QA_PASS}}'
  cuttle jev-browse --task "list the open tickets" --extract "one ticket, with its id and title"

--text supplies the values that may be typed. Only their NAMES are sent: the
model picks WHICH field a value belongs in, and the value itself is looked up
here, afterwards, and handed to the driver verbatim - which is what lets a
` + "`{{cuttle:NAME}}`" + ` sentinel from ` + "`cuttle secret set`" + ` pass through untouched and be
substituted inside cuttle, on the fill path. A --text value is argv, so it shows
in the host's ` + "`ps`" + `; a sentinel keeps a secret out of it.

--extract picks the page lines that are one item of the kind it describes and
prints them verbatim. It does not write an answer, and headings or prose are
never picked, so it suits list-shaped answers.

The run happens in the same driver session as ` + "`cuttle pw`" + `, so whatever the
outcome the browser is left on exactly the page it stopped at, and
` + "`cuttle pw snapshot`" + ` picks it up mid-state.

Exit codes: 0 the task is done, 1 an error, 3 blocked (a person is needed), 4 the
step budget ran out.

The API key comes from ` + jev.APIKeyEnv + `. --mock needs no key: it decides
locally, without judgement, but it still clicks and fills the live page.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runJevBrowse(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.task, "task", "", "what the run is trying to achieve, in one sentence (required)")
	fl.StringVar(&f.url, "url", "", "page to start from (default: wherever the browser already is)")
	fl.IntVar(&f.maxSteps, "max-steps", 25, "the most decisions to take before giving up; one can cost more than one action")
	fl.StringVar(&f.extract, "extract", "", "the kind of item to pick off the final page; matching lines print verbatim (for list-shaped answers)")
	fl.StringArrayVar(&f.text, "text", nil, "name=value a field may be filled with; repeatable. Only the name is sent")
	fl.BoolVar(&f.json, "json", false, "write the step log and the outcome as JSON lines")
	fl.BoolVar(&f.mock, "mock", false, "decide locally instead of calling the API: no key and no judgement")
	_ = cmd.MarkFlagRequired("task")
	return cmd
}

func runJevBrowse(cmd *cobra.Command, f jevBrowseFlags) error {
	values, err := parseTextValues(f.text)
	if err != nil {
		return err
	}
	driver, err := newPlaywrightRunner(cmd.Context())
	if err != nil {
		return err
	}
	code, err := jev.Run(cmd.Context(), jev.Options{
		Task:     f.task,
		URL:      f.url,
		MaxSteps: f.maxSteps,
		Extract:  f.extract,
		JSON:     f.json,
		Mock:     f.mock,
		Values:   values,
		Driver:   driver,
		Out:      cmd.OutOrStdout(),
		Err:      cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}
	if code != jev.ExitDone {
		// The loop has already written the handoff brief, so this only carries the
		// code out to main - which exits with it verbatim, the way `cuttle pw` does.
		return &ExitCodeError{Code: code}
	}
	return nil
}

// parseTextValues splits the repeatable --text pairs. The value half is never
// echoed back, not even in the error for a malformed pair: a mistyped
// `--text pass=hunter2` would otherwise print the password.
func parseTextValues(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w, and one has no name", errJevTextPair)
		}
		values[name] = value
	}
	return values, nil
}
