package fingerprint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// versionsEnvValue reads one key out of packages/browser/versions.env, the file
// that calls itself the single source of version truth.
func versionsEnvValue(t *testing.T, key string) string {
	t.Helper()
	env, err := os.ReadFile(filepath.Join("..", "..", "packages", "browser", "versions.env"))
	if err != nil {
		t.Fatalf("versions.env: %v", err)
	}
	for line := range strings.SplitSeq(string(env), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.TrimSpace(strings.SplitN(after, "#", 2)[0])
		}
	}
	t.Fatalf("versions.env: %s not found", key)
	return ""
}

// The Go side cannot read versions.env at build time (go:embed cannot escape the
// package dir), so chromiumVersion is a literal. It is the one the browser
// actually is: navigator.userAgent follows the real binary version regardless of
// --user-agent, so a stale literal here yields a UA that disagrees with UA-CH.
func TestChromiumVersionPin(t *testing.T) {
	t.Parallel()
	if want := versionsEnvValue(t, "CHROMIUM_VERSION"); chromiumVersion != want {
		t.Errorf("chromiumVersion=%s but versions.env CHROMIUM_VERSION=%s - the persona "+
			"would advertise a version the shipped binary does not have", chromiumVersion, want)
	}
}

// The smoke gate asserts the persona's UA-CH platformVersion, so its copy has to
// be the one production emits. Neither side can read the other (Go literal vs
// Python literal), and a drift here means the gate would pass a persona the
// daemon never ships. The browser version itself is not checked: smoke.py reads
// that from versions.env, which TestChromiumVersionPin already covers.
func TestPersonaVersionsMatchSmoke(t *testing.T) {
	smoke, err := os.ReadFile(filepath.Join("..", "..", "packages", "browser", "validate", "smoke.py"))
	if err != nil {
		t.Fatalf("smoke.py: %v", err)
	}
	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	for _, tc := range []struct{ arch, constant string }{
		{"amd64", "WINDOWS_PLATFORM_VERSION"},
		{"arm64", "MACOS_PLATFORM_VERSION"},
	} {
		args := forkParityArgsFor(tc.arch, "en-US", "")

		var want string
		for _, arg := range args {
			if after, ok := strings.CutPrefix(arg, "--fingerprint-platform-version="); ok {
				want = after
			}
		}
		if want == "" {
			t.Fatalf("%s: ForkParityArgs emitted no --fingerprint-platform-version", tc.arch)
		}
		if line := tc.constant + ` = "` + want + `"`; !strings.Contains(string(smoke), line) {
			t.Errorf("smoke.py is missing %s - production emits %s for %s",
				line, want, tc.arch)
		}
	}
}

// smoke.py reads the pinned version from versions.env instead of hardcoding it,
// which makes the file a runtime input to the gate. The gate runs in a container
// that only sees what run-build.sh mounts, and it exits rather than skipping when
// an input is missing - so a forgotten mount cannot publish an unvalidated binary,
// but it does throw away a full build. Cheaper to assert the mount.
func TestSmokeHarnessInputsAreMounted(t *testing.T) {
	t.Parallel()
	runBuild, err := os.ReadFile(filepath.Join("..", "..", "packages", "browser", "build", "run-build.sh"))
	if err != nil {
		t.Fatalf("run-build.sh: %v", err)
	}
	// Match the MOUNT form (":<dest>:ro"), not the bare path. Checking the bare
	// path passes on `-e GOLDEN_JSON=/work/golden.json` alone, so the gate would
	// look mounted while the file is absent.
	for _, mount := range []string{
		":/work/packages/browser/versions.env:ro",
		// smoke.py reads baseChromeArgs out of the golden so it launches Chrome
		// the way the daemon does; unmounted, the gate aborts after a full
		// compile.
		":/work/golden.json:ro",
	} {
		if !strings.Contains(string(runBuild), mount) {
			t.Errorf("run-build.sh does not mount %s - validate/smoke.py reads it at "+
				"startup, so the gate would abort after a full compile", mount)
		}
	}
}

// versions.env is the single source of version truth, but the image pulls the
// browser via literals in ops/docker/Dockerfile (ADD --checksum cannot take an
// ARG). Two hand-synced copies of a sha256 pin is exactly the drift the golden
// tripwire exists to prevent elsewhere, so assert they agree.
func TestDockerfilePinsMatchVersionsEnv(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	dockerfile, err := os.ReadFile(filepath.Join(root, "ops", "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	envVal := func(key string) string { return versionsEnvValue(t, key) }

	for _, key := range []string{"BROWSER_SHA256_X64", "BROWSER_SHA256_ARM64"} {
		want := envVal(key)
		if want == "" {
			t.Errorf("%s is empty in versions.env", key)
			continue
		}
		if !strings.Contains(string(dockerfile), "sha256:"+want) {
			t.Errorf("%s=%s is not pinned in ops/docker/Dockerfile - the image would "+
				"pull a different binary than versions.env records", key, want)
		}
	}

	tag := envVal("BROWSER_RELEASE_TAG")
	tagRE := regexp.MustCompile(`ARG BROWSER_TAG=(\S+)`)
	matches := tagRE.FindAllStringSubmatch(string(dockerfile), -1)
	if len(matches) == 0 {
		t.Fatal("Dockerfile: no ARG BROWSER_TAG found")
	}
	for _, m := range matches {
		if m[1] != tag {
			t.Errorf("Dockerfile ARG BROWSER_TAG=%s but versions.env BROWSER_RELEASE_TAG=%s", m[1], tag)
		}
	}
}

// smoke.py hand-writes its launch flags rather than deriving them from the
// production arg-builders, and it had silently drifted five flags behind:
// --enable-blink-features, --enable-features, --blink-settings,
// --use-fake-device-for-media-stream and --window-size. Every assertion in the
// gate therefore ran against a browser we do not ship, and the gap was
// invisible because the missing flags only ADD capabilities - nothing fails,
// the gate just stops observing WebShare, BarcodeDetector, navigator.bluetooth,
// the dark color scheme and the pointer/hover pair.
//
// This pins the flag KEYS, not their values. Values legitimately differ (the
// gate fixes a seed, a timezone and a locale so its assertions are
// deterministic); the key set is what must not drift. A new production flag now
// fails here until the gate is taught about it.
func TestSmokeMatchesProductionFlags(t *testing.T) {
	// No t.Parallel: t.Setenv below is incompatible with it.
	smoke, err := os.ReadFile(filepath.Join("..", "..", "packages", "browser", "validate", "smoke.py"))
	if err != nil {
		t.Fatalf("smoke.py: %v", err)
	}
	// Only flags inside quoted string literals count. The first version of this
	// test scanned the whole file, so it went green on prose: the comment
	// explaining --enable-features kept the assertion satisfied after the flag
	// itself was deleted. A gate that its own documentation can satisfy is the
	// exact defect this test exists to catch.
	source := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(string(smoke), "")
	flag := regexp.MustCompile(`--[a-z0-9-]+`)
	present := map[string]bool{}
	for _, lit := range regexp.MustCompile(`"[^"\n]*"`).FindAllString(source, -1) {
		for _, m := range flag.FindAllString(lit, -1) {
			present[m] = true
		}
	}

	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	for _, arch := range []string{"amd64", "arm64"} {
		production := append([]string{}, stealthArgsFor(arch, 42069)...)
		production = append(production, forkParityArgsFor(arch, "en-US", "")...)
		production = append(production, screenArgsFor(arch, "42069")...)
		production = append(production, appleSiliconArgsFor(arch, "42069")...)
		production = append(production, windowsMachineArgsFor(arch, "42069")...)

		for _, arg := range production {
			key, _, _ := strings.Cut(arg, "=")
			if !strings.HasPrefix(key, "--") {
				continue
			}
			if !present[key] {
				t.Errorf("%s: production emits %s but smoke.py never passes it - the gate "+
					"would validate a browser configuration we do not ship", arch, key)
			}
		}
	}
}

// Pinning keys alone is not enough for the flags whose VALUE is a fixed constant
// in args.go. Add a feature to blinkFeatures() and the key --enable-blink-features
// is still present in smoke.py, this stays green, and the gate silently stops
// observing the new feature - the same defect one level down.
//
// Only these four are pinned. Every other production flag carries a value the
// gate deliberately fixes for determinism (the seed, timezone, locale, and the
// machine/screen tuples), so requiring those to match would be wrong.
func TestSmokeMatchesProductionFlagValues(t *testing.T) {
	// No t.Parallel: t.Setenv below is incompatible with it.
	raw, err := os.ReadFile(filepath.Join("..", "..", "packages", "browser", "validate", "smoke.py"))
	if err != nil {
		t.Fatalf("smoke.py: %v", err)
	}
	source := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(string(raw), "")
	// Join Python adjacent-string-literal concatenation so a value split across
	// two quoted lines is still found as one string.
	source = regexp.MustCompile(`"\s*\n\s*"`).ReplaceAllString(source, "")

	pinned := map[string]bool{
		"--enable-blink-features": true,
		"--enable-features":       true,
		"--disable-features":      true,
		"--blink-settings":        true,
	}

	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	for _, arch := range []string{"amd64", "arm64"} {
		production := append([]string{}, stealthArgsFor(arch, 42069)...)
		production = append(production, forkParityArgsFor(arch, "en-US", "")...)

		for _, arg := range production {
			key, value, found := strings.Cut(arg, "=")
			if !found || !pinned[key] {
				continue
			}
			if !strings.Contains(source, value) {
				t.Errorf("%s: production emits %s=%s but that value appears nowhere in "+
					"smoke.py - the gate pins the flag but not what it is set to",
					arch, key, value)
			}
		}
	}
}
