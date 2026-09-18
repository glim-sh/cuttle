package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/jev"
)

func init() { AddCommand(newJevBrowseCmd()) }

var (
	errJevTextPair   = errors.New("--text takes name=value")
	errJevTaskTwice  = errors.New("the task was given twice: once as the argument and once as --task - pass one")
	errJevTaskAbsent = errors.New("a task is required: pass it as the argument or with --task")
)

type jevBrowseFlags struct {
	task     string
	url      string
	maxSteps int
	extract  string
	text     []string
	json     bool
	mock     bool
	takeover bool
}

func newJevBrowseCmd() *cobra.Command {
	var f jevBrowseFlags
	cmd := &cobra.Command{
		Use:   "jev-browse [task]",
		Short: "(experimental) browse toward a task on cuttle's browser, one model-picked action at a time",
		Long: `EXPERIMENTAL. Drive cuttle's browser toward a task without an LLM in the loop.

Each step reads the page with the same bundled playwright-cli ` + "`cuttle pw`" + ` runs,
offers its interactive elements to TypeSafe's System-One model, and performs the
one it picks. The model only ever CHOOSES - it generates no text - so a step
costs a fraction of asking an LLM which button to press next.

  cuttle jev-browse --url https://example.com "open the support page"
  cuttle jev-browse --task "sign in" --text user=qa@example.com --text pass='{{cuttle:QA_PASS}}'
  cuttle jev-browse --task "go to the open tickets list" --extract "one ticket, with its id and title"

--url is the page to start from, and required on a fresh session with no page
yet. Phrase the task as reaching a page: a read-only task ("find X", "list Y")
may never decide it is done and spend the whole step budget. Read the page it
reaches with --extract or ` + "`cuttle pw snapshot`" + `.

--text supplies the values that may be typed. Only their NAMES are sent: the
model picks WHICH field a value belongs in, and the value itself is looked up
here, afterwards, and handed to the driver verbatim - which is what lets a
` + "`{{cuttle:NAME}}`" + ` sentinel from ` + "`cuttle secret set`" + ` pass through untouched and be
substituted inside cuttle, on the fill path. A --text value is argv, so it shows
in the host's ` + "`ps`" + `; a sentinel keeps a secret out of it.

--extract picks the page lines that are one item of the kind it describes and
prints them verbatim. It does not write an answer, and headings or prose are
never picked, so it suits list-shaped answers. It runs on every ending but an
error, and needs the model: --mock refuses it.

The run happens in the same driver session as ` + "`cuttle pw`" + `, so whatever the
outcome the browser is left on exactly the page it stopped at, and
` + "`cuttle pw snapshot`" + ` picks it up mid-state.

--context/--name pick which instance the run drives, as they do for every other
verb (CUTTLE_CONTEXT/CUTTLE_NAME do it without a flag):

  cuttle jev-browse --name scraper "open the support page"

While it runs it holds the session lease: a second run refuses to start and
` + "`cuttle pw`" + ` refuses verbs that drive the page, both naming this run. --takeover
takes the browser from whoever holds it, and a run taken over stops with 1.

Exit codes: 0 the task is done, 1 an error, 3 blocked (a person is needed), 4 the
step budget ran out.

The API key comes from ` + jev.APIKeyEnv + `. --mock needs no key: it decides
locally, without judgement, but it still clicks and fills the live page.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runJevBrowse(cmd, f, args) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.task, "task", "", "what the run is trying to achieve, in one sentence; also takeable as the argument")
	fl.StringVar(&f.url, "url", "", "page to start from (default: wherever the browser already is)")
	fl.IntVar(&f.maxSteps, "max-steps", 25, "the most decisions to take before giving up; one can cost more than one action")
	fl.StringVar(&f.extract, "extract", "", "the kind of item to pick off the final page; matching lines print verbatim (for list-shaped answers)")
	fl.StringArrayVar(&f.text, "text", nil, "name=value a field may be filled with; repeatable. Only the name is sent")
	fl.BoolVar(&f.json, "json", false, "write the step log and the outcome as JSON lines")
	fl.BoolVar(&f.mock, "mock", false, "decide locally instead of calling the API: no key and no judgement")
	fl.BoolVar(&f.takeover, "takeover", false, "take the browser from whoever holds its session lease, instead of refusing to start")
	return cmd
}

// jevTask resolves the task from either spelling. The positional form is sugar
// for --task, which stays the canonical one; giving both is a typo worth naming
// rather than a precedence rule worth remembering.
func jevTask(flag string, args []string) (string, error) {
	if len(args) == 0 {
		if strings.TrimSpace(flag) == "" {
			return "", errJevTaskAbsent
		}
		return flag, nil
	}
	if strings.TrimSpace(flag) != "" {
		return "", errJevTaskTwice
	}
	if strings.TrimSpace(args[0]) == "" {
		return "", errJevTaskAbsent
	}
	return args[0], nil
}

func runJevBrowse(cmd *cobra.Command, f jevBrowseFlags, args []string) error {
	task, err := jevTask(f.task, args)
	if err != nil {
		return err
	}
	values, err := parseTextValues(f.text)
	if err != nil {
		return err
	}
	ex, self, err := playwrightExecer(cmd.Context())
	if err != nil {
		return err
	}
	lease, err := acquireLease(cmd.Context(), ex, leaseOwner("jev-browse"), f.takeover)
	if err != nil {
		return err
	}
	defer lease.release()
	// Ctrl-C would otherwise kill the process before the deferred release, leaving
	// the browser locked for a full lease TTL.
	sigCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(sigCtx)
	defer cancel(nil)
	go lease.heartbeat(ctx, cancel)

	code, err := jev.Run(ctx, jev.Options{
		Task:     task,
		URL:      f.url,
		MaxSteps: f.maxSteps,
		Extract:  f.extract,
		JSON:     f.json,
		Mock:     f.mock,
		Values:   values,
		Cuttle:   self,
		Driver:   lease.guard(newPlaywrightRunner(ex), cancel),
		Out:      cmd.OutOrStdout(),
		Err:      cmd.ErrOrStderr(),
	})
	// A takeover is why the run stopped, whatever the loop made of its canceled
	// verb, so it is what gets reported.
	if cause := context.Cause(ctx); errors.Is(cause, errSessionTakenOver) {
		return cause //nolint:wrapcheck // our own cancel cause, already worded for the user
	}
	if sigCtx.Err() != nil {
		return &ExitCodeError{Code: 130}
	}
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
