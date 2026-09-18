package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestPlaywrightArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
		err  error
	}{
		{
			name: "attach gets the in-container endpoint",
			args: []string{"attach"},
			want: []string{"playwright-cli", "attach", "--cdp=http://127.0.0.1:9222"},
		},
		{
			name: "an explicit --cdp is left alone",
			args: []string{"attach", "--cdp=http://127.0.0.1:9333"},
			want: []string{"playwright-cli", "attach", "--cdp=http://127.0.0.1:9333"},
		},
		{
			name: "--cdp as a separate token counts too",
			args: []string{"attach", "--cdp", "http://127.0.0.1:9333"},
			want: []string{"playwright-cli", "attach", "--cdp", "http://127.0.0.1:9333"},
		},
		{
			name: "other verbs pass through untouched",
			args: []string{"screenshot", "--filename=shot.png"},
			want: []string{"playwright-cli", "screenshot", "--filename=shot.png"},
		},
		{
			name: "--cdp is only injected for attach",
			args: []string{"navigate", "https://example.com"},
			want: []string{"playwright-cli", "navigate", "https://example.com"},
		},
		// The image's CDP endpoint makes `open` attach too, so it passes through.
		{
			name: "open passes through",
			args: []string{"open", "https://example.com"},
			want: []string{"playwright-cli", "open", "https://example.com"},
		},
		{name: "--endpoint redirects the driver", args: []string{"attach", "--endpoint=ws://elsewhere"}, err: errPlaywrightRedirect},
		{name: "--endpoint as a bare flag", args: []string{"attach", "--endpoint", "ws://elsewhere"}, err: errPlaywrightRedirect},
		{name: "--extension redirects the driver", args: []string{"attach", "--extension"}, err: errPlaywrightRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := playwrightArgv(tt.args)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err=%v want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("argv=%v want %v", got, tt.want)
			}
		})
	}
}

func TestPlaywrightNeedsAttach(t *testing.T) {
	t.Parallel()
	const notOpen = "Error: The browser 'cuttle' is not open, please run open first"
	const browserDied = "The browser 'cuttle' is not open, please run open first\n\n  playwright-cli -s=cuttle open [params]\n"
	tests := []struct {
		name     string
		args     []string
		combined string
		want     bool
	}{
		{name: "a verb with no session", args: []string{"snapshot"}, combined: notOpen, want: true},
		// Observed, not assumed: kill /opt/browser/chrome under a live session and
		// the daemon exits with it, so the next verb reports exactly this - stdout,
		// exit 1, no distinct "browser closed" wording to match. Re-attach is the
		// right answer because cuttle has a replacement browser up by then.
		{name: "a browser killed mid-session reports the same marker", args: []string{"snapshot"}, combined: browserDied, want: true},
		{name: "the marker on stdout counts", args: []string{"goto", "https://example.com"}, combined: notOpen, want: true},
		{name: "any other failure is real", args: []string{"click", "e17"}, combined: "Error: no element e17", want: false},
		{name: "attach failing is real", args: []string{"attach"}, combined: notOpen, want: false},
		{name: "open failing is real", args: []string{"open", "https://example.com"}, combined: notOpen, want: false},
		{name: "no args", args: nil, combined: notOpen, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := playwrightNeedsAttach(tt.args, tt.combined); got != tt.want {
				t.Fatalf("playwrightNeedsAttach=%v want %v", got, tt.want)
			}
		})
	}
}

func TestExitCodeErrorCarriesCode(t *testing.T) {
	t.Parallel()
	var err error = &ExitCodeError{Code: 42}
	ec, ok := errors.AsType[*ExitCodeError](err)
	if !ok || ec.Code != 42 {
		t.Fatalf("AsType gave %v %v", ec, ok)
	}
}

// `cuttle pw` parses its own --context/--name because DisableFlagParsing means
// cobra parses nothing for it, root's persistent flags included.
func TestSplitInstanceFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		wantSel  instanceFlags
		wantArgs []string
		wantErr  bool
	}{
		{name: "no cuttle flags", args: []string{"snapshot"}, wantArgs: []string{"snapshot"}},
		{
			name:    "separate value",
			args:    []string{"--name", "scraper", "snapshot"},
			wantSel: instanceFlags{name: "scraper"}, wantArgs: []string{"snapshot"},
		},
		{
			name:    "--flag=value",
			args:    []string{"--context=box", "--name=scraper", "goto", "https://example.com"},
			wantSel: instanceFlags{contextName: "box", name: "scraper"}, wantArgs: []string{"goto", "https://example.com"},
		},
		{
			name:    "only the leading run is cuttle's",
			args:    []string{"--name", "scraper", "click", "--name", "ref"},
			wantSel: instanceFlags{name: "scraper"}, wantArgs: []string{"click", "--name", "ref"},
		},
		{name: "a driver flag ends the run", args: []string{"snapshot", "--name", "x"}, wantArgs: []string{"snapshot", "--name", "x"}},
		{name: "missing value", args: []string{"--name"}, wantErr: true},
		{name: "no verb left", args: []string{"--name", "scraper"}, wantSel: instanceFlags{name: "scraper"}, wantArgs: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel, args, err := splitInstanceFlags(instanceFlags{}, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("args %q: want an error, got sel %+v args %q", tc.args, sel, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("args %q: %v", tc.args, err)
			}
			if sel != tc.wantSel {
				t.Fatalf("selection = %+v, want %+v", sel, tc.wantSel)
			}
			if !slices.Equal(args, tc.wantArgs) {
				t.Fatalf("passthrough = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}

// echoExecer runs the argv it is handed through echo, so a test sees exactly
// what would be execed in the container.
type echoExecer struct{}

func (echoExecer) ExecCommand(_ string, argv []string) (string, []string) { return "echo", argv }

// `cuttle pw --help` is what an agent reaches for first, so it must end with the
// driver's own verb list rather than only the wrapper's.
func TestWriteDriverHelpRunsTheBundledDriversHelp(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := writeDriverHelp(context.Background(), echoExecer{}, &out, &errOut); err != nil {
		t.Fatalf("writeDriverHelp: %v", err)
	}
	for _, want := range []string{"cuttle pw <verb>", "cuttle pw --help <verb>", "playwright-cli --help\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("driver help missing %q:\n%s", want, out.String())
		}
	}
}

// With no instance to exec into, the wrapper help still prints and says where
// the driver's own help will come from, instead of failing the --help.
func TestPlaywrightHelpWithoutAnInstance(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withInstance(t, instanceFlags{contextName: "no-such-context"})
	cmd := newPlaywrightCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := playwrightHelp(cmd); err != nil {
		t.Fatalf("playwrightHelp: %v", err)
	}
	for _, want := range []string{"cuttle pw --help <verb>", "prints here once the\ninstance is running"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing %q:\n%s", want, out.String())
		}
	}
}
