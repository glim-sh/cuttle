package mcp

import (
	"errors"
	"testing"
)

func TestBuildConfigGolden(t *testing.T) {
	t.Parallel()
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

func TestLookupUnknown(t *testing.T) {
	t.Parallel()
	if _, err := Lookup("nope"); !errors.Is(err, errUnknownDriver) {
		t.Fatalf("want errUnknownDriver, got %v", err)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got := DefaultConfigPath("browser-use"); got != "/cfg/cuttle/mcp/browser-use.json" {
		t.Fatalf("path=%q", got)
	}
}
