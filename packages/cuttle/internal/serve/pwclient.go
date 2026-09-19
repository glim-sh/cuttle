package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

// pwClientJS is a long-lived stand-in for the bundled driver's client half.
// Each `cuttle pw` verb otherwise costs an exec plus a node start that loads the
// driver's whole bundle just to send one request to the session daemon's
// socket; this loads only the client modules, once, and forwards each verb over
// that same socket to the same daemon - so input still reaches cuttle's CDP
// endpoint and is humanized there. It reads one {id, args} per line and answers
// one {id, ...} per line, verbs running concurrently as separate execs would.
// Anything the driver handles outside the daemon (attach, open, list, a session
// flag, a flag or arity it would reject), and every failure that happens before
// the verb reaches the daemon, answers "fallback" so the caller execs the driver
// and keeps its exact behavior and wording. run-code runs arbitrary code in the
// daemon's node process, which the port must not grant beyond what an exec does.
//
//nolint:gosec // G101 misreads the script as a credential
const pwClientJS = `
const fs = require('fs'), path = require('path'), readline = require('readline');
const bin = process.env.PATH.split(':').map(d => path.join(d, 'playwright-cli')).find(f => fs.existsSync(f));
const core = path.dirname(require.resolve('playwright-core/package.json', { paths: [path.dirname(fs.realpathSync(bin))] }));
const mod = f => require(path.join(core, 'lib/tools/cli-client', f));
const { Registry, createClientInfo, resolveSessionName } = mod('registry');
const { Session } = mod('session');
const { minimist } = mod('minimist');
const help = mod('help.json');
const clientSide = new Set(['list', 'close-all', 'delete-data', 'kill-all', 'open', 'attach', 'close', 'detach', 'install', 'install-browser', 'show', 'run-code']);
const boolean = [...help.booleanOptions, 'all', 'g', 'help', 'json', 'raw', 'version'];
const preSend = ['is not open. Run', 'Client is v'];
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
  try {
    const result = await new Session(entry).run(client, args, { raw, json: false });
    return { isError: !!result.isError, text: result.text + '\n' };
  } catch (e) {
    if (preSend.some(m => String(e && e.message).includes(m)))
      return { fallback: true };
    throw e;
  }
}
readline.createInterface({ input: process.stdin }).on('line', line => {
  const { id, args } = JSON.parse(line);
  handle(args).then(r => r, e => ({ failed: String(e && e.stack || e) }))
    .then(r => process.stdout.write(JSON.stringify({ id, ...r }) + '\n'));
}).on('close', () => process.exit(0));
process.stdout.write(JSON.stringify({ id: 0, ready: true }) + '\n');
`

// pwStartTimeout bounds the client's start: a node boot and a few small
// requires, far under this.
const pwStartTimeout = 15 * time.Second

// pwBodyLimit caps a /pw request: an argv, where a fill value is the largest part.
const pwBodyLimit = 1 << 20

var errPWClientDied = errors.New("the pw client exited mid-verb")

// pwReply is one verb's outcome. Fallback means the verb never reached the
// daemon and the caller must exec it; Failed means it may have, and must not be
// re-run.
type pwReply struct {
	ID       int    `json:"id"`
	Ready    bool   `json:"ready,omitempty"`
	Fallback bool   `json:"fallback,omitempty"`
	IsError  bool   `json:"isError,omitempty"`
	Text     string `json:"text,omitempty"`
	Failed   string `json:"failed,omitempty"`
}

// pwClient keeps one pwClientJS process for the daemon's lifetime, started on
// the first verb. One that dies is replaced on the next verb.
type pwClient struct {
	ctx     context.Context //nolint:containedctx // the client's lifetime is the daemon's
	workdir string

	mu      sync.Mutex
	stdin   io.WriteCloser
	pending map[int]chan pwReply
	nextID  int
}

func (c *pwClient) run(ctx context.Context, args []string) (pwReply, error) {
	c.mu.Lock()
	if c.stdin == nil {
		if err := c.start(); err != nil {
			c.mu.Unlock()
			logWarn("pw client did not start: %v", err)
			return pwReply{Fallback: true}, nil
		}
	}
	c.nextID++
	id, ch := c.nextID, make(chan pwReply, 1)
	c.pending[id] = ch
	req, _ := json.Marshal(map[string]any{"id": id, "args": args}) //nolint:errchkjson // an int and a []string
	_, err := c.stdin.Write(append(req, '\n'))
	c.mu.Unlock()
	if err != nil {
		// Never delivered, so the verb has not run and the exec path may run it.
		return pwReply{Fallback: true}, nil //nolint:nilerr // the fallback is the answer
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return pwReply{}, errPWClientDied
		}
		return r, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return pwReply{}, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
	}
}

// start launches the client and waits for its ready line. Called with mu held.
func (c *pwClient) start() error {
	cmd := exec.CommandContext(c.ctx, "node", "-e", pwClientJS)
	cmd.Dir = c.workdir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err //nolint:wrapcheck // only logged
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err //nolint:wrapcheck // only logged
	}
	if err = cmd.Start(); err != nil {
		return err //nolint:wrapcheck // only logged
	}
	ready := make(chan pwReply, 1)
	pending := map[int]chan pwReply{}
	c.stdin, c.pending = stdin, pending
	go c.read(cmd, stdin, stdout, pending, ready)
	select {
	case r, ok := <-ready:
		if ok && r.Ready {
			return nil
		}
	case <-time.After(pwStartTimeout):
		_ = cmd.Process.Kill()
	}
	c.stdin = nil
	return errPWClientDied
}

// read routes each reply to its waiter. When the client exits every verb still
// waiting learns it died, and the next verb starts a fresh client.
// The ready line goes to ready without the lock, which start holds.
func (c *pwClient) read(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader, pending map[int]chan pwReply, ready chan<- pwReply) {
	r := bufio.NewReader(stdout)
	if line, err := r.ReadBytes('\n'); err == nil {
		var reply pwReply
		_ = json.Unmarshal(line, &reply)
		ready <- reply
	}
	close(ready)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break
		}
		var reply pwReply
		if json.Unmarshal(line, &reply) != nil {
			continue
		}
		c.mu.Lock()
		if ch, ok := pending[reply.ID]; ok {
			ch <- reply
			delete(pending, reply.ID)
		}
		c.mu.Unlock()
	}
	_ = cmd.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range pending {
		close(ch)
		delete(pending, id)
	}
	if c.stdin == stdin {
		c.stdin = nil
	}
}

// pwRequest is one `cuttle pw` verb. Drive and Takeover carry the CLI's own lease
// decision, so the gate and the verb cost one round trip.
type pwRequest struct {
	Args     []string `json:"args"`
	Drive    bool     `json:"drive"`
	Takeover bool     `json:"takeover"`
	Owner    string   `json:"owner"`
}

// handlePW runs one driver verb through the persistent client. It is reachable
// exactly as the lease and downloads endpoints are - loopback Host only - and
// grants nothing an exec into the container does not already.
func (m *multiplexer) handlePW(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.requestSeed(w, r)
	if !ok {
		return
	}
	var req pwRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, pwBodyLimit)).Decode(&req); err != nil || len(req.Args) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "body must be {\"args\": [verb, ...]}"})
		return
	}
	if req.Drive {
		if req.Takeover {
			if !validLeaseOwner(req.Owner) {
				writeJSON(w, http.StatusBadRequest, map[string]any{keyError: errLeaseNoOwner.Error()})
				return
			}
			m.pool.leases.forceRelease(seed, req.Owner)
		} else if cur, held := m.pool.leases.status(seed); held {
			v := m.pool.leases.leaseView(cur, false)
			v["held"] = true
			writeJSON(w, http.StatusConflict, v)
			return
		}
	}
	reply, err := m.pw.run(r.Context(), req.Args)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{keyError: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, reply)
}
