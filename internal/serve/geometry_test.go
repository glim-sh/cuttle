package serve

import (
	"strings"
	"testing"

	"github.com/glim-sh/cuttle/internal/fingerprint"
)

// geometryEnv fakes the env the entrypoint runs the command under.
func geometryEnv(t *testing.T, dataDir string, env map[string]string) envProbe {
	t.Helper()
	e := defaultEnvProbe()
	e.getenv = func(k string) string {
		if k == "CUTTLE_DATA_DIR" {
			return dataDir
		}
		return env[k]
	}
	return e
}

// The framebuffer has to match the window the daemon will open, and the seed
// that decides that window has to be the SAME one the daemon later launches -
// otherwise the geometry is right for a browser nobody runs.
func TestViewerGeometryMatchesTheSeedTheDaemonWillLaunch(t *testing.T) {
	t.Setenv(fingerprint.BinaryPathEnv, "/opt/browser/chrome")
	dir := t.TempDir()
	e := geometryEnv(t, dir, map[string]string{"CUTTLE_KEEP_PROFILE": "1"})

	got, err := viewerGeometry(e)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := windowSizeArg(fingerprint.ScreenArgs(defaultFingerprintSeedIn(dir, true)))
	if !ok {
		t.Fatal("ScreenArgs pinned no window size")
	}
	if got != want {
		t.Fatalf("geometry = %s, want the launched seed's window %s", got, want)
	}
	if !strings.Contains(got, "x") {
		t.Fatalf("geometry %q is not WxH", got)
	}

	// Stable across calls: the seed is persisted, so a restart keeps the geometry.
	again, err := viewerGeometry(e)
	if err != nil || again != got {
		t.Fatalf("geometry drifted across calls: %s then %s (%v)", got, again, err)
	}
}

// When the size is not knowable before the launch, the command must fail so the
// entrypoint keeps its default geometry rather than guessing wrong.
func TestViewerGeometryRefusesWhenUnknowable(t *testing.T) {
	t.Setenv(fingerprint.BinaryPathEnv, "/opt/browser/chrome")
	dir := t.TempDir()
	cases := map[string]map[string]string{
		"pool mode has no single browser":   {"CUTTLE_MODE": string(modePool), "CUTTLE_KEEP_PROFILE": "1"},
		"a fresh fingerprint every launch":  {"CUTTLE_KEEP_PROFILE": "0"},
		"ephemeral discards what it picked": {"CUTTLE_KEEP_PROFILE": "1", "CUTTLE_EPHEMERAL": "1"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := viewerGeometry(geometryEnv(t, dir, env)); err == nil {
				t.Fatalf("expected a refusal, got geometry %q", got)
			}
		})
	}
}

func TestWindowSizeArg(t *testing.T) {
	t.Parallel()
	if got, ok := windowSizeArg([]string{"--foo", "--window-size=1440,805"}); !ok || got != "1440x805" {
		t.Fatalf("windowSizeArg = %q,%v want 1440x805,true", got, ok)
	}
	for _, args := range [][]string{
		nil,
		{"--window-size=1440"},
		{"--window-size=wide,805"},
	} {
		if got, ok := windowSizeArg(args); ok {
			t.Fatalf("windowSizeArg(%v) = %q, want no geometry", args, got)
		}
	}
}
