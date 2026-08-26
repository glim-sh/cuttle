package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A password with a stray newline fails a login in a way nothing explains, and
// every natural way to pipe one in appends exactly one.
func TestReadSecretStdinStripsOneTrailingNewline(t *testing.T) {
	for in, want := range map[string]string{
		"hunter2\n":      "hunter2",
		"hunter2\r\n":    "hunter2",
		"hunter2":        "hunter2",
		"two words\n":    "two words",
		"keeps\ninner\n": "keeps\ninner",
	} {
		got, err := readSecretStdin(strings.NewReader(in))
		if err != nil {
			t.Fatalf("readSecretStdin(%q): %v", in, err)
		}
		if string(got) != want {
			t.Errorf("readSecretStdin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadSecretStdinRefusesNothing(t *testing.T) {
	for _, in := range []string{"", "\n", "\r\n"} {
		if _, err := readSecretStdin(strings.NewReader(in)); !errors.Is(err, errSecretNoInput) {
			t.Errorf("readSecretStdin(%q) error = %v, want errSecretNoInput", in, err)
		}
	}
}

// `set` with no source must not fall back to reading a value from argv - that is
// the whole point of the verb - and it must not silently pick one of two.
func TestSecretSetNeedsExactlyOneSource(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want error
	}{
		"no source": {[]string{"GH_PASS"}, errSecretNeedSource},
		"both":      {[]string{"GH_PASS", "--stdin", "--exec", "echo hi"}, errSecretBothInputs},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newSecretSetCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A resolver runs on the host and its stdout IS the value; its stderr never
// propagates, because vault error text quotes item names and partial values.
func TestResolveExec(t *testing.T) {
	got, err := resolveExec(t.Context(), "printf 'hunter2\n'")
	if err != nil {
		t.Fatalf("resolveExec: %v", err)
	}
	if string(got) != "hunter2" {
		t.Fatalf("value = %q, want hunter2 with the trailing newline stripped", got)
	}
	if _, emptyErr := resolveExec(t.Context(), "printf ''"); !errors.Is(emptyErr, errSecretExecEmpty) {
		t.Fatalf("empty resolver error = %v, want errSecretExecEmpty", emptyErr)
	}
	_, err = resolveExec(t.Context(), "echo 'ERROR: item \"prod db\" has secret abc' >&2; exit 1")
	if !errors.Is(err, errSecretExecFailed) {
		t.Fatalf("failing resolver error = %v, want errSecretExecFailed", err)
	}
	if strings.Contains(err.Error(), "prod db") || strings.Contains(err.Error(), "abc") {
		t.Fatalf("the resolver's stderr reached the error text: %v", err)
	}
}

// `secret prompt` must never fall back to reading a pipe: "ask the human" that
// silently reads whatever was piped is a different verb, and --stdin is it.
func TestSecretPromptRefusesAPipe(t *testing.T) {
	cmd := newSecretPromptCmd()
	cmd.SetArgs([]string{"SMS_CODE"})
	cmd.SetIn(strings.NewReader("123456\n"))
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, errSecretNotATTY) {
		t.Fatalf("error = %v, want errSecretNotATTY", err)
	}
}

// A credential written into a repo is one `git add -A` from being published, and
// driver scratch state has been swept into a commit before.
func TestCaptureSinkRefusesAGitWorkingTree(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatalf("seeding a repo: %v", err)
	}
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("seeding a subdir: %v", err)
	}
	if _, _, err := parseSink("file:"+filepath.Join(nested, "key.txt"), false); !errors.Is(err, errCaptureInRepo) {
		t.Fatalf("error = %v, want errCaptureInRepo", err)
	}
	if _, _, err := parseSink("file:"+filepath.Join(nested, "key.txt"), true); err != nil {
		t.Fatalf("--force must override: %v", err)
	}

	// A `git worktree` checkout has .git as a FILE, and this feature was built in
	// one - recognizing only the directory form would miss exactly that case.
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatalf("seeding a worktree: %v", err)
	}
	if _, _, err := parseSink("file:"+filepath.Join(worktree, "key.txt"), false); !errors.Is(err, errCaptureInRepo) {
		t.Fatalf("worktree error = %v, want errCaptureInRepo", err)
	}
}

func TestCaptureSinkParsing(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "key.txt")
	for _, tc := range []struct{ to, wantSink, wantArg string }{
		{"", sinkMemory, ""},
		{"memory", sinkMemory, ""},
		{"file:" + outside, sinkFile, outside},
		{"exec:gh secret set X", sinkExec, "gh secret set X"},
	} {
		sink, arg, err := parseSink(tc.to, false)
		if err != nil {
			t.Fatalf("parseSink(%q): %v", tc.to, err)
		}
		if sink != tc.wantSink || arg != tc.wantArg {
			t.Errorf("parseSink(%q) = %q, %q; want %q, %q", tc.to, sink, arg, tc.wantSink, tc.wantArg)
		}
	}
	for _, bad := range []string{"stdout", "file:", "exec:"} {
		if _, _, err := parseSink(bad, false); !errors.Is(err, errCaptureSink) {
			t.Errorf("parseSink(%q) error = %v, want errCaptureSink", bad, err)
		}
	}
}

// The sink runs the command with the value on ITS STDIN: a value in argv is
// world-readable in /proc and lands in shell history.
func TestCaptureExecSinkFeedsStdin(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	if err := writeToSink(t.Context(), sinkExec, "cat > "+dest, []byte("s3cret")); err != nil {
		t.Fatalf("writeToSink: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading what the sink got: %v", err)
	}
	if string(got) != "s3cret" {
		t.Fatalf("the sink received %q, want the value on stdin", got)
	}
}

func TestCaptureFileSinkIs0600(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "key.txt")
	if err := writeToSink(t.Context(), sinkFile, dest, []byte("s3cret")); err != nil {
		t.Fatalf("writeToSink: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// Exactly one source, and never a silent default: a capture that quietly read
// the wrong thing has already touched the credential.
func TestCaptureNeedsExactlyOneSource(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want error
	}{
		"no source": {[]string{"API_KEY"}, errCaptureNoSelector},
		"both":      {[]string{"API_KEY", "--selector", "#x", "--from-clipboard"}, errCaptureTwoSources},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newSecretCaptureCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// os.WriteFile's mode applies only on create, so writing a credential over an
// existing world-readable scratch file would leave it world-readable.
func TestWriteSecretFileTightensAnExistingFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(dest, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := writeSecretFile(dest, []byte("s3cret")); err != nil {
		t.Fatalf("writeSecretFile: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 even over an existing file", info.Mode().Perm())
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "s3cret" {
		t.Fatalf("content = %q, want the new value", got)
	}
}

// Nothing irreversible may happen before the name is checked: a resolver spends
// a one-time code, and `prompt` asks a human to type one. A 400 from the daemon
// after the fact wastes exactly what this feature protects.
func TestSecretVerbsRefuseABadNameBeforeTakingAValue(t *testing.T) {
	for _, name := range []string{"GH-PASS", "9lives", "has space", "", strings.Repeat("x", 65)} {
		if err := checkSecretName(name); !errors.Is(err, errSecretBadName) {
			t.Errorf("checkSecretName(%q) = %v, want errSecretBadName", name, err)
		}
	}
	for _, name := range []string{"GH_PASS", "_x", "a1"} {
		if err := checkSecretName(name); err != nil {
			t.Errorf("checkSecretName(%q) = %v, want it accepted", name, err)
		}
	}

	// End to end: the verb fails on the name without reaching the network - a
	// daemon that is not running would otherwise be the error the user sees.
	cmd := newSecretSetCmd()
	cmd.SetArgs([]string{"GH-PASS", "--stdin"})
	cmd.SetIn(strings.NewReader("hunter2\n"))
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, errSecretBadName) {
		t.Fatalf("error = %v, want errSecretBadName", err)
	}
}
