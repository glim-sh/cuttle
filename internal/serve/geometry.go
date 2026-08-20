package serve

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/cli"
	"github.com/glim-sh/cuttle/internal/fingerprint"
)

func init() { cli.AddCommand(newViewerGeometryCmd()) }

var errNoViewerGeometry = errors.New("no fixed viewer geometry for this daemon")

// The session browser's window is sized to the seed's fake screen, not to the
// display: a browser claiming a 1440x900 screen while filling a 1920x1080 window
// is an obvious lie, so the window loses. That left the viewer showing a small
// window marooned on a big black framebuffer.
//
// Sizing the FRAMEBUFFER to the window instead settles it in the other
// direction: the seed keeps its own screen dimensions (the fork answers
// window.screen from --fingerprint-screen-*, never from the X display), and the
// human gets a browser that fills the viewer. The entrypoint asks for the
// geometry before it starts Xvnc, which is why this is a command and not a
// daemon-time resize.
func newViewerGeometryCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "viewer-geometry",
		Short:  "print the WxH the session browser's window will occupy",
		Hidden: true, // read by the image entrypoint, not a user verb
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			geometry, err := viewerGeometry(defaultEnvProbe())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), geometry)
			return nil
		},
	}
}

// viewerGeometry resolves the reserved seed's window size from the same env the
// daemon will read, so both agree without the entrypoint having to parse flags.
// It errors - and the entrypoint keeps its default geometry - whenever the answer
// is not knowable ahead of the launch: a pool daemon has no single browser, and a
// non-durable profile draws a fresh random seed on every launch.
func viewerGeometry(e envProbe) (string, error) {
	if serveMode(e.getenv(modeEnv)) == modePool {
		return "", fmt.Errorf("%w: pool mode runs one browser per seed", errNoViewerGeometry)
	}
	durable := parseBoolEnv(e.getenv("CUTTLE_KEEP_PROFILE")) && !parseBoolEnv(e.getenv(ephemeralEnv))
	if !durable {
		return "", fmt.Errorf("%w: a non-durable profile picks a new fingerprint each launch", errNoViewerGeometry)
	}
	dataDir := e.getenv("CUTTLE_DATA_DIR")
	if dataDir == "" {
		dataDir = defaultDataDir(e)
	}
	size, ok := windowSizeArg(fingerprint.ScreenArgs(defaultFingerprintSeedIn(dataDir, true)))
	if !ok {
		return "", fmt.Errorf("%w: this build pins no screen for the seed", errNoViewerGeometry)
	}
	return size, nil
}

// windowSizeArg reads the "WxH" out of ScreenArgs' --window-size=W,H.
func windowSizeArg(args []string) (string, bool) {
	for _, a := range args {
		spec, found := strings.CutPrefix(a, "--window-size=")
		if !found {
			continue
		}
		w, h, ok := strings.Cut(spec, ",")
		if !ok {
			return "", false
		}
		if _, err := strconv.Atoi(w); err != nil {
			return "", false
		}
		if _, err := strconv.Atoi(h); err != nil {
			return "", false
		}
		return w + "x" + h, true
	}
	return "", false
}
