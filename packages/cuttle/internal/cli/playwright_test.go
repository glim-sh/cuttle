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
		{name: "open would launch its own browser", args: []string{"open", "https://example.com"}, err: errPlaywrightOpen},
		{name: "--endpoint redirects the driver", args: []string{"attach", "--endpoint=ws://elsewhere"}, err: errPlaywrightRedirect},
		{name: "--endpoint as a bare flag", args: []string{"attach", "--endpoint", "ws://elsewhere"}, err: errPlaywrightRedirect},
		{name: "--extension redirects the driver", args: []string{"attach", "--extension"}, err: errPlaywrightRedirect},
		// open is only rejected as the verb: a page titled "open" is a fine argument.
		{name: "open as an argument is fine", args: []string{"click", "open"}, want: []string{"playwright-cli", "click", "open"}},
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

func TestExitCodeErrorCarriesCode(t *testing.T) {
	t.Parallel()
	var err error = &ExitCodeError{Code: 42}
	ec, ok := errors.AsType[*ExitCodeError](err)
	if !ok || ec.Code != 42 {
		t.Fatalf("AsType gave %v %v", ec, ok)
	}
}
