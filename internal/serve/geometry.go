package serve

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

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
// Sizing the FRAMEBUFFER to the window instead settles it the other way: the seed
// keeps its own screen dimensions (the fork answers window.screen from
// --fingerprint-screen-*, never from the X display), and the human gets a browser
// that fills the viewer. The entrypoint needs the size before it starts Xvnc,
// which is why this is a command and not a daemon-time resize.
//
// It takes the daemon's own command line so it resolves mode, data dir and
// durability through serve's flags-over-env precedence. Reading the environment
// alone was wrong the moment anyone passed a flag instead - which the helm chart
// does for --keep-profile and --data-dir, and the pool-mode docs do for --mode.
func newViewerGeometryCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "viewer-geometry [cuttle serve ...]",
		Short:  "print the WxH the session browser's window will occupy",
		Hidden: true, // read by the image entrypoint, not a user verb
		// The daemon's argv carries its own flags (and a `--` Chrome passthrough);
		// none of them are ours to interpret.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			geometry, err := viewerGeometry(serveArgsOf(args))
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), geometry)
			return nil
		},
	}
}

// serveArgsOf extracts the flags of the `cuttle serve ...` command line the
// entrypoint was given. Anything before the `serve` verb is the binary and its
// wrappers; an argv with no `serve` at all leaves env as the only source, which
// is what a hand-written `docker run` with a bare CMD gets.
func serveArgsOf(argv []string) []string {
	if i := slices.Index(argv, "serve"); i >= 0 {
		return argv[i+1:]
	}
	return nil
}

// viewerGeometry resolves the reserved seed's window size from the daemon's own
// config, so both agree by construction. It errors - and the entrypoint keeps its
// default geometry - whenever the answer is not knowable ahead of the launch: a
// pool daemon has no single browser, and a non-durable profile draws a fresh
// random seed on every launch.
func viewerGeometry(serveArgs []string) (string, error) {
	fs := newServeCmd().Flags()
	if err := fs.Parse(serveArgs); err != nil {
		return "", fmt.Errorf("%w: %w", errNoViewerGeometry, err)
	}
	cfg, err := serveConfigFromFlags(fs)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoViewerGeometry, err)
	}
	if cfg.mode != modeSession {
		return "", fmt.Errorf("%w: pool mode runs one browser per seed", errNoViewerGeometry)
	}
	if !durableProfile(cfg.keepProfile, cfg.ephemeral) {
		return "", fmt.Errorf("%w: a non-durable profile picks a new fingerprint each launch", errNoViewerGeometry)
	}
	// Checked before resolving the seed, because resolving it PERSISTS it: without
	// this the command would write a seed file and then report it had no answer.
	if !fingerprint.ScreenPinned() {
		return "", fmt.Errorf("%w: this build pins no screen for the seed", errNoViewerGeometry)
	}
	width, height, ok := fingerprint.WindowSize(defaultFingerprintSeedIn(cfg.dataDir, true), cfg.screen)
	if !ok {
		return "", fmt.Errorf("%w: this build pins no screen for the seed", errNoViewerGeometry)
	}
	return strconv.Itoa(width) + "x" + strconv.Itoa(height), nil
}
