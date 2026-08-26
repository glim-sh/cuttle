package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `cuttle secret capture` moves a value OUT of a page without it rendering. The
// leak it exists for is not typing a credential: it is reading one back, and the
// worst case in the corpus was a snapshot taken later, to debug a failed login,
// with the value still in the field.
//
// The default sink is memory, so the common site-A-to-site-B flow (generate a
// token on A, type it into B) is two commands and the value never leaves the
// daemon at all.

var (
	errCaptureSink   = errors.New("unknown --to sink")
	errCaptureInRepo = errors.New("refusing to write a secret inside a git working tree")
)

const (
	sinkMemory = "memory"
	sinkFile   = "file:"
	sinkExec   = "exec:"
)

func newSecretCaptureCmd() *cobra.Command {
	var cf commonFlags
	var selector, to string
	var clipboard, force bool
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "capture NAME (--selector <css> | --from-clipboard)",
		Short: "read a value out of the page into the session, without it rendering",
		Long: `Reads a value out of the browser - one element, or its clipboard - and keeps
it, without the value passing through a snapshot, a screenshot or your context.

  cuttle secret capture API_KEY --selector '#new-token'           # keep it in the session
  cuttle secret capture API_KEY --selector '#new-token' --to file:key.txt
  cuttle secret capture API_KEY --selector '#new-token' --to exec:'gh secret set API_KEY'
  cuttle secret capture API_KEY --from-clipboard                  # after a "copy" button

--from-clipboard reads the browser's clipboard instead of an element, which is
the shape of a page whose only affordance is a copy button. It needs an https
page: the browser itself refuses a clipboard read anywhere else.

--to memory (the default) keeps it in the daemon under a TTL, ready to be typed
as {{cuttle:NAME}}; the value never leaves the browser's container. file: writes
0600 and refuses a path inside a git working tree. exec: runs a command with the
value on ITS STDIN - never in its arguments.

On a one-time-display credential, capture BEFORE you look: a snapshot or a
screenshot of that page is the leak, not the diagnostic.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case selector != "" && clipboard:
				return errCaptureTwoSources
			case selector == "" && !clipboard:
				return errCaptureNoSelector
			}
			return runSecretCapture(cmd, cf, args[0], captureSource{selector: selector, clipboard: clipboard}, to, force, ttl)
		},
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().StringVar(&selector, "selector", "", "CSS selector for the element to read")
	cmd.Flags().BoolVar(&clipboard, "from-clipboard", false, "read the browser's clipboard instead of an element")
	cmd.Flags().StringVar(&to, "to", sinkMemory, "where the value goes: memory, file:<path>, or exec:<command>")
	cmd.Flags().BoolVar(&force, "force", false, "allow --to file: inside a git working tree")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the daemon keeps a --to memory value (default 15m)")
	return cmd
}

var (
	errCaptureNoSelector = errors.New("name a source: --selector '#api-key', or --from-clipboard")
	errCaptureTwoSources = errors.New("pass either --selector or --from-clipboard, not both")
)

// captureSource is where a value is read from. Exactly one of the two is set.
type captureSource struct {
	selector  string
	clipboard bool
}

func (s captureSource) String() string {
	if s.clipboard {
		return "the clipboard"
	}
	return s.selector
}

func runSecretCapture(cmd *cobra.Command, cf commonFlags, name string, src captureSource, to string, force bool, ttl time.Duration) error {
	// Validate the sink BEFORE reading anything: a capture that succeeds and then
	// discovers it has nowhere to put the value has already taken the risk.
	sink, arg, err := parseSink(to, force)
	if err != nil {
		return err
	}
	base, release, err := secretEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()

	body := map[string]any{"selector": src.selector, "clipboard": src.clipboard, "return": sink != sinkMemory}
	if ttl > 0 {
		body["ttl_seconds"] = int(ttl.Seconds())
	}
	var reply struct {
		Name       string `json:"name"`
		Length     int    `json:"length"`
		TTLSeconds int    `json:"ttl_seconds"`
		Value      string `json:"value"`
	}
	endpoint := base + "/secret/" + url.PathEscape(name) + "/capture"
	if err := requestJSON(cmd.Context(), http.MethodPost, endpoint, body, &reply); err != nil {
		return fmt.Errorf("capturing %s: %w", name, err)
	}
	out := cmd.OutOrStdout()
	if sink == sinkMemory {
		// Name the TTL: a sink-less capture that quietly expires is the one way
		// this verb wastes a one-time-display credential.
		fmt.Fprintf(out, "%s  %d bytes  from %s  expires in %s\n  "+fillHint+"\n",
			reply.Name, reply.Length, src, time.Duration(reply.TTLSeconds)*time.Second, reply.Name)
		return nil
	}

	value := []byte(reply.Value)
	defer clear(value)
	if err := writeToSink(cmd.Context(), sink, arg, value); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s  %d bytes  from %s  -> %s\n", name, len(value), src, to)
	return nil
}

// parseSink splits and validates a --to argument. The git-worktree check happens
// here, before the value exists: driver scratch state has been swept into a
// commit before, and a credential landing in a repo is that accident with a much
// worse blast radius.
func parseSink(to string, force bool) (string, string, error) {
	switch {
	case to == "" || to == sinkMemory:
		return sinkMemory, "", nil
	case strings.HasPrefix(to, sinkFile):
		path := strings.TrimPrefix(to, sinkFile)
		if path == "" {
			return "", "", fmt.Errorf("%w: file: needs a path", errCaptureSink)
		}
		if root, inRepo := gitWorkingTree(path); inRepo && !force {
			return "", "", fmt.Errorf("%w (%s) - write it outside the repo, or pass --force if you are sure",
				errCaptureInRepo, root)
		}
		return sinkFile, path, nil
	case strings.HasPrefix(to, sinkExec):
		command := strings.TrimPrefix(to, sinkExec)
		if command == "" {
			return "", "", fmt.Errorf("%w: exec: needs a command", errCaptureSink)
		}
		return sinkExec, command, nil
	default:
		return "", "", fmt.Errorf("%w %q: use memory, file:<path> or exec:<command>", errCaptureSink, to)
	}
}

func writeToSink(ctx context.Context, sink, arg string, value []byte) error {
	if sink == sinkFile {
		if err := os.WriteFile(arg, value, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", arg, err)
		}
		return nil
	}
	// STDIN, always. A value in a command's arguments is world-readable in
	// /proc and lands in shell history.
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", arg)
	c.Stdin = bytes.NewReader(value)
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	c.WaitDelay = 5 * time.Second
	if err := c.Run(); err != nil {
		return fmt.Errorf("%w: %w - run it yourself to see why (cuttle does not capture its stderr, which can quote the value)",
			errSecretExecFailed, err)
	}
	return nil
}

// gitWorkingTree walks up from path looking for a .git, returning the repo root
// it found. It deliberately does not care whether .git is a directory or a FILE:
// the file form is a `git worktree` checkout, which is where this feature was
// itself developed, and a check that only recognized the directory form would
// have missed exactly that case. go-git would answer this in one call and cost
// 6 MB of binary for a boolean.
func gitWorkingTree(path string) (string, bool) {
	dir, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
