package backend

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Runner is the exec seam every backend goes through, so command construction is
// unit-testable without docker/kubectl/ssh installed.
type Runner interface {
	// Output runs a command to completion and captures its output. A non-zero
	// exit is reported in Result.Code with a nil error (mirroring the Python
	// check=False); a nil error means only that the command ran.
	Output(ctx context.Context, name string, args ...string) (Result, error)
	// Start launches a long-running command (a tunnel) and returns a handle to
	// stop it.
	Start(ctx context.Context, name string, args ...string) (Process, error)
	// LookPath reports the resolved path of an executable, or an error if absent.
	LookPath(name string) (string, error)
}

// Result is a finished command's captured output and exit code.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Process is a running command that can be stopped.
type Process interface {
	Stop() error
}

// ExecRunner is the production Runner backed by os/exec.
type ExecRunner struct{}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers build argv from config, not user input
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.Code = exitErr.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, err //nolint:wrapcheck // the exec error is already descriptive
	}
	return res, nil
}

func (ExecRunner) Start(ctx context.Context, name string, args ...string) (Process, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers build argv from config, not user input
	if err := cmd.Start(); err != nil {
		return nil, err //nolint:wrapcheck
	}
	return &execProcess{cmd: cmd}, nil
}

func (ExecRunner) LookPath(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err //nolint:wrapcheck
	}
	return p, nil
}

type execProcess struct{ cmd *exec.Cmd }

func (e *execProcess) Stop() error {
	if e.cmd.Process == nil {
		return nil
	}
	_ = e.cmd.Process.Kill()
	_ = e.cmd.Wait()
	return nil
}
