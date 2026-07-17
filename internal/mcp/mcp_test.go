package mcp

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildConfigDefaultGolden(t *testing.T) {
	t.Parallel()
	if DefaultDriver != "agent-browser" {
		t.Fatalf("DefaultDriver=%q, want agent-browser", DefaultDriver)
	}
	d, err := Lookup(DefaultDriver)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	got, err := Marshal(BuildConfig(d, "http://127.0.0.1:9222?fingerprint=linkedin"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{
  "mcpServers": {
    "agent-browser": {
      "command": "agent-browser",
      "args": [
        "--cdp",
        "http://127.0.0.1:9222?fingerprint=linkedin",
        "mcp"
      ]
    }
  }
}`
	if string(got) != want {
		t.Fatalf("config JSON mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildConfigBrowserUseGolden(t *testing.T) {
	t.Parallel()
	d, err := Lookup("browser-use")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	got, err := Marshal(BuildConfig(d, "http://127.0.0.1:9222?fingerprint=linkedin"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{
  "mcpServers": {
    "browser-use": {
      "command": "browser-use",
      "args": [
        "--mcp"
      ],
      "env": {
        "BU_CDP_URL": "http://127.0.0.1:9222?fingerprint=linkedin"
      }
    }
  }
}`
	if string(got) != want {
		t.Fatalf("config JSON mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestLookupUnknownEnumeratesDrivers(t *testing.T) {
	t.Parallel()
	_, err := Lookup("nope")
	if !errors.Is(err, errUnknownDriver) {
		t.Fatalf("want errUnknownDriver, got %v", err)
	}
	for _, name := range []string{"agent-browser", "browser-use"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not mention %q", err, name)
		}
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got := DefaultConfigPath("browser-use"); got != "/cfg/cuttle/mcp/browser-use.json" {
		t.Fatalf("path=%q", got)
	}
}
