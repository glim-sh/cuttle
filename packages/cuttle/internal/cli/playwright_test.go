package cli

import (
	"errors"
	"slices"
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
	tests := []struct {
		name     string
		args     []string
		combined string
		want     bool
	}{
		{name: "a verb with no session", args: []string{"snapshot"}, combined: notOpen, want: true},
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
