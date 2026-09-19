package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
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
yet. Phrase the task as reaching a page: the model never sees page text, so a
read-only task ("find X", "list Y") can end blocked or out of steps on the very
page that holds the answer. Read the page it reaches with --extract or
` + "`cuttle pw snapshot`" + `.

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

Controls that change the site rather than move around it (send, post, apply,
save, connect, follow, message, delete, pay, ...) are never offered, so the run
cannot take them; the brief lists them as withheld. Each option names its
section - "[banner: Global Navigation] searchbox: Search" - and its state.

The run happens in the same driver session as ` + "`cuttle pw`" + `, so whatever the
outcome the browser is left on exactly the page it stopped at, and
` + "`cuttle pw snapshot`" + ` picks it up mid-state. Its verbs go through one persistent
driver client for the whole run rather than an exec each, falling back to an
exec for anything that client does not handle.

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
	fl.IntVar(&f.maxSteps, "max-steps", 25, "the most decisions to take before giving up; one can cost more than one action, and the page the last one lands on is still judged for done")
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
	counted := &countingExecer{Execer: ex}
	ex = counted
	// Checked up front: the loop would otherwise take the lease and then report the
	// exec failure of its first verb as a stuck page.
	if bundledDriverAbsent(cmd.Context(), ex) {
		return errDriverMissing(self)
	}
	lease, err := acquireLease(cmd.Context(), ex, leaseOwner("jev-browse"), f.takeover)
	if err != nil {
		return err
	}
	defer lease.release()
	driver := &persistentDriver{ex: ex}
	defer driver.close()
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
		Driver:   lease.guard(attachingRunner(ex, driver.run), cancel),
		Spawns:   counted.spawns.Load,
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

// countingExecer counts every process run through it - the driver's verbs, a
// re-attach, and the lease curls alike - so a step's spawns are the real number.
type countingExecer struct {
	backend.Execer
	spawns atomic.Int64
}

func (c *countingExecer) ExecCommand(workdir string, argv []string) (string, []string) {
	c.spawns.Add(1)
	return c.Execer.ExecCommand(workdir, argv)
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
		if !ok {
			return nil, fmt.Errorf("%w, and one has no =", errJevTextPair)
		}
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w, and one has no name", errJevTextPair)
		}
		values[name] = value
	}
	return values, nil
}

// persistentClientJS is a long-lived stand-in for the bundled driver's client
// half. Each playwright-cli invocation is a fresh exec plus a node start that
// loads the driver's whole bundle (~0.3s) just to send one line to the session
// daemon's socket; this loads only the client modules, once, and forwards each
// verb over that same socket to the same daemon - which is what keeps a verb's
// input on cuttle's CDP endpoint, humanized there as `cuttle pw`'s is. It reads
// one JSON array of driver args per line and answers one JSON object per line.
// Anything the driver handles outside the daemon (attach, open, list, a session
// flag, a flag or arity it would reject) answers "fallback", and the verb is
// execed as usual, so those keep the driver's own behavior and wording.
const persistentClientJS = `
const fs = require('fs'), path = require('path'), readline = require('readline');
const bin = process.env.PATH.split(':').map(d => path.join(d, 'playwright-cli')).find(f => fs.existsSync(f));
const core = path.dirname(require.resolve('playwright-core/package.json', { paths: [path.dirname(fs.realpathSync(bin))] }));
const mod = f => require(path.join(core, 'lib/tools/cli-client', f));
const { Registry, createClientInfo, resolveSessionName } = mod('registry');
const { Session } = mod('session');
const { minimist } = mod('minimist');
const help = mod('help.json');
const clientSide = new Set(['list', 'close-all', 'delete-data', 'kill-all', 'open', 'attach', 'close', 'detach', 'install', 'install-browser', 'show']);
const boolean = [...help.booleanOptions, 'all', 'g', 'help', 'json', 'raw', 'version'];
const send = o => process.stdout.write(JSON.stringify(o) + '\n');
async function handle(argv) {
  const args = minimist(argv, { boolean, string: ['_'] });
  const name = args._[0], command = help.commands[name];
  if (!command || clientSide.has(name))
    return { fallback: true };
  const own = Object.keys(args).filter(k => k !== '_' && k !== 'raw');
  if (own.some(k => !(k in command.flags)) || (args._.length - 1 > command.args.length && !command.variadicArg))
    return { fallback: true };
  const raw = !!args.raw || !!command.raw;
  delete args.raw;
  const session = resolveSessionName();
  const client = createClientInfo();
  const entry = (await Registry.load()).entry(client, session);
  if (!entry)
    return { isError: true, text: "The browser '" + session + "' is not open, please run open first\n\n  playwright-cli" + (session !== 'default' ? ' -s=' + session : '') + ' open [params]\n' };
  const result = await new Session(entry).run(client, args, { raw, json: false });
  return { isError: !!result.isError, text: result.text + '\n' };
}
let queue = Promise.resolve();
readline.createInterface({ input: process.stdin })
  .on('line', line => { queue = queue.then(() => handle(JSON.parse(line))).then(send, e => send({ isError: true, text: String(e && e.stack || e) + '\n' })); })
  .on('close', () => queue.then(() => process.exit(0)));
send({ ready: true });
`

// persistentStartTimeout bounds the client's start: a node boot and a few
// small requires, far under this.
const persistentStartTimeout = 15 * time.Second

var errPersistentDied = errors.New("the persistent driver client exited mid-verb")

// persistentDriver runs jev-browse's driver verbs through one persistentClientJS
// process that lives for the whole run, so a verb costs the daemon round-trip
// and not an exec plus a node start. It never talks CDP and drives the same
// session `cuttle pw` does. A client that cannot start is given up on for the
// run and every verb execs as before; one that dies mid-verb fails only that
// verb - it may already have acted, so it is never re-run - and the next verb
// starts a fresh client.
type persistentDriver struct {
	ex     backend.Execer
	mu     sync.Mutex
	stop   context.CancelFunc
	stdin  io.WriteCloser
	lines  chan []byte
	exited chan struct{}
	broken bool
}

type persistentReply struct {
	Ready    bool   `json:"ready"`
	Fallback bool   `json:"fallback"`
	IsError  bool   `json:"isError"`
	Text     string `json:"text"`
}

func (d *persistentDriver) run(ctx context.Context, argv []string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.broken || argv[1] == verbAttach || argv[1] == verbOpen {
		return execVerb(ctx, d.ex, argv)
	}
	if d.stdin == nil {
		if err := d.start(ctx); err != nil {
			d.broken = true
			return execVerb(ctx, d.ex, argv)
		}
	}
	req, err := json.Marshal(argv[1:])
	if err != nil {
		return "", err //nolint:wrapcheck // a []string always marshals
	}
	if _, err = d.stdin.Write(append(req, '\n')); err != nil {
		// Never delivered, so the verb has not run and the exec path may run it.
		d.shutdown()
		return execVerb(ctx, d.ex, argv)
	}
	reply, err := d.read(ctx)
	if err != nil {
		d.shutdown()
		return "", err
	}
	if reply.Fallback {
		return execVerb(ctx, d.ex, argv)
	}
	if reply.IsError {
		// The driver's own exit status for a failed verb, as the exec path reports it.
		return reply.Text, &ExitCodeError{Code: 1}
	}
	return reply.Text, nil
}

func (d *persistentDriver) start(ctx context.Context) error {
	pctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	exe, args := d.ex.ExecCommand(playwrightWorkdir, []string{"node", "-e", persistentClientJS})
	c := exec.CommandContext(pctx, exe, args...)
	stdin, err := c.StdinPipe()
	if err != nil {
		stop()
		return err //nolint:wrapcheck // only decides the fallback
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		stop()
		return err //nolint:wrapcheck // only decides the fallback
	}
	if err = c.Start(); err != nil {
		stop()
		return err //nolint:wrapcheck // only decides the fallback
	}
	lines, exited := make(chan []byte), make(chan struct{})
	go func() {
		r := bufio.NewReader(stdout)
		for {
			line, readErr := r.ReadBytes('\n')
			if readErr != nil {
				_ = c.Wait()
				close(exited)
				return
			}
			select {
			case lines <- line:
			case <-pctx.Done():
			}
		}
	}()
	d.stop, d.stdin, d.lines, d.exited = stop, stdin, lines, exited
	sctx, cancel := context.WithTimeout(ctx, persistentStartTimeout)
	defer cancel()
	reply, err := d.read(sctx)
	if err == nil && !reply.Ready {
		err = errPersistentDied
	}
	if err != nil {
		d.shutdown()
	}
	return err
}

func (d *persistentDriver) read(ctx context.Context) (persistentReply, error) {
	var reply persistentReply
	select {
	case line := <-d.lines:
		return reply, json.Unmarshal(line, &reply) //nolint:wrapcheck // our own client's reply
	case <-d.exited:
		return reply, errPersistentDied
	case <-ctx.Done():
		return reply, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
	}
}

// shutdown kills the client; the next verb starts a fresh one.
func (d *persistentDriver) shutdown() {
	if d.stdin == nil {
		return
	}
	d.stop()
	<-d.exited
	d.stdin = nil
}

// close ends the client at the end of the run: EOF on its stdin lets it finish
// and exit on its own, and a kill follows if it does not.
func (d *persistentDriver) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stdin == nil {
		return
	}
	_ = d.stdin.Close()
	select {
	case <-d.exited:
	case <-time.After(2 * time.Second):
	}
	d.shutdown()
}
