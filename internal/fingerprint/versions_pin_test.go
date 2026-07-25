package fingerprint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// versions.env calls itself the single source of version truth, but the image
// pulls the browser via literals in ops/docker/Dockerfile (ADD --checksum cannot
// take an ARG). Two hand-synced copies of a sha256 pin is exactly the drift the
// golden tripwire exists to prevent elsewhere, so assert they agree.
func TestDockerfilePinsMatchVersionsEnv(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	env, err := os.ReadFile(filepath.Join(root, "packages", "browser", "versions.env"))
	if err != nil {
		t.Fatalf("versions.env: %v", err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "ops", "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	envVal := func(key string) string {
		for line := range strings.SplitSeq(string(env), "\n") {
			if after, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
				return strings.TrimSpace(strings.SplitN(after, "#", 2)[0])
			}
		}
		t.Fatalf("versions.env: %s not found", key)
		return ""
	}

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
