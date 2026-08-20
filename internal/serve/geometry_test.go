package serve

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/glim-sh/cuttle/internal/fingerprint"
)

// seedFileExists reports whether the daemon's persisted default-seed file is
// there - the side effect resolving a geometry has, and must not have when it
// refuses.
func seedFileExists(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, reservedSeed+".seed"))
	return err == nil
}

// The framebuffer has to match the window the daemon will open, and the seed that
// decides that window has to be the SAME one the daemon later launches -
// otherwise the geometry is right for a browser nobody runs.
func TestViewerGeometryMatchesTheSeedTheDaemonWillLaunch(t *testing.T) {
	t.Setenv(fingerprint.BinaryPathEnv, "/opt/browser/chrome")
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)
	t.Setenv(keepProfileEnv, "1")

	got, err := viewerGeometry(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Session mode pins the persona's largest screen unless told otherwise; the
	// daemon resolves the same default, so the framebuffer matches the window.
	w, h, ok := fingerprint.WindowSize(defaultFingerprintSeedIn(dir, true), fingerprint.LargestScreen())
	if !ok {
		t.Fatal("this build pins no screen")
	}
	if want := strconv.Itoa(w) + "x" + strconv.Itoa(h); got != want {
		t.Fatalf("geometry = %s, want the launched seed's window %s", got, want)
	}
	if !strings.Contains(got, "x") {
		t.Fatalf("geometry %q is not WxH", got)
	}

	// Stable across calls: the seed is persisted, so a restart keeps the geometry.
	again, err := viewerGeometry(nil)
	if err != nil || again != got {
		t.Fatalf("geometry drifted across calls: %s then %s (%v)", got, again, err)
	}
}

// The daemon resolves flags over env; reading only the env made this command
// disagree with every operator who passed a flag - which the helm chart does for
// --keep-profile and --data-dir, and the pool-mode docs do for --mode.
func TestViewerGeometryHonorsFlagsOverEnv(t *testing.T) {
	t.Setenv(fingerprint.BinaryPathEnv, "/opt/browser/chrome")

	t.Run("--keep-profile as a flag counts as durable", func(t *testing.T) {
		t.Setenv(dataDirEnv, t.TempDir())
		if _, err := viewerGeometry([]string{"--headless=false", "--keep-profile"}); err != nil {
			t.Fatalf("a --keep-profile flag must resolve a geometry: %v", err)
		}
	})

	t.Run("--mode=pool as a flag beats a session env", func(t *testing.T) {
		t.Setenv(dataDirEnv, t.TempDir())
		t.Setenv(keepProfileEnv, "1")
		t.Setenv(modeEnv, string(modeSession))
		if got, err := viewerGeometry([]string{"--mode=pool"}); err == nil {
			t.Fatalf("pool mode has no single browser; got geometry %q", got)
		}
	})

	t.Run("--ephemeral as a flag beats a keep-profile env", func(t *testing.T) {
		t.Setenv(dataDirEnv, t.TempDir())
		t.Setenv(keepProfileEnv, "1")
		if got, err := viewerGeometry([]string{"--ephemeral"}); err == nil {
			t.Fatalf("an ephemeral profile has no stable fingerprint; got %q", got)
		}
	})

	t.Run("--data-dir as a flag decides where the seed lives", func(t *testing.T) {
		flagDir, envDir := t.TempDir(), t.TempDir()
		t.Setenv(dataDirEnv, envDir)
		t.Setenv(keepProfileEnv, "1")
		if _, err := viewerGeometry([]string{"--data-dir=" + flagDir}); err != nil {
			t.Fatal(err)
		}
		if !seedFileExists(flagDir) {
			t.Fatal("the seed must be resolved from the --data-dir flag")
		}
		if seedFileExists(envDir) {
			t.Fatal("the env data dir must be ignored when the flag is set")
		}
	})
}

// A pool daemon must not have its seed file created by a command run from the
// entrypoint - it would never create that seed itself.
func TestViewerGeometryWritesNoSeedWhenItRefuses(t *testing.T) {
	t.Setenv(fingerprint.BinaryPathEnv, "/opt/browser/chrome")
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)
	t.Setenv(keepProfileEnv, "1")

	if _, err := viewerGeometry([]string{"--mode=pool"}); err == nil {
		t.Fatal("expected a refusal in pool mode")
	}
	if seedFileExists(dir) {
		t.Fatal("a refusal must not persist a default-seed file")
	}
}

// Without a pinned screen there is no answer, and the command must not persist a
// seed on its way to saying so.
func TestViewerGeometryRefusesWithNoPinnedScreen(t *testing.T) {
	t.Setenv(fingerprint.BinaryPathEnv, "")
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)
	t.Setenv(keepProfileEnv, "1")

	if got, err := viewerGeometry(nil); err == nil {
		t.Fatalf("expected a refusal, got %q", got)
	}
	if seedFileExists(dir) {
		t.Fatal("a refusal must not persist a default-seed file")
	}
}

func TestServeArgsOf(t *testing.T) {
	t.Parallel()
	got := serveArgsOf([]string{"cuttle", "serve", "--mode=pool", "--", "about:blank"})
	if want := []string{"--mode=pool", "--", "about:blank"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("serveArgsOf = %v want %v", got, want)
	}
	if got := serveArgsOf([]string{"bash", "-c", "true"}); got != nil {
		t.Fatalf("an argv with no serve verb has no flags to read, got %v", got)
	}
}

// An operator-pinned --screen (or CUTTLE_SCREEN) decides the window, so it must
// decide the framebuffer too; an off-table value is refused the way the daemon
// refuses it, so the entrypoint falls back instead of sizing for a browser that
// will never start.
func TestViewerGeometryFollowsScreenOverride(t *testing.T) {
	t.Setenv(fingerprint.BinaryPathEnv, "/opt/browser/chrome")
	t.Setenv(dataDirEnv, t.TempDir())
	t.Setenv(keepProfileEnv, "1")

	choices := fingerprint.ScreenOptions()
	smallest := choices[0]
	got, err := viewerGeometry([]string{"--screen=" + smallest})
	if err != nil {
		t.Fatal(err)
	}
	w, h, _ := fingerprint.WindowSize("ignored", smallest)
	if want := strconv.Itoa(w) + "x" + strconv.Itoa(h); got != want {
		t.Fatalf("geometry = %s, want %s for --screen=%s", got, want, smallest)
	}

	t.Setenv(screenEnv, "640x480")
	if _, err := viewerGeometry(nil); err == nil {
		t.Fatal("an off-table CUTTLE_SCREEN must be refused")
	}
}
