package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/config"
	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/mcp"
	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/profile"
)

func newMCPCmd() *cobra.Command {
	var cf commonFlags
	var output string
	cmd := &cobra.Command{
		Use:   "mcp [driver]",
		Short: "install a CDP driver and write its MCP config pointed at this context + profile",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runMCP(cmd, cf, args, output) },
	}
	addCommonFlags(cmd, &cf)
	addProfileFlag(cmd, &cf.profile)
	cmd.Flags().StringVar(&output, "output", "", "write the config here ('-' = stdout; default $XDG_CONFIG_HOME/cuttle/mcp/<driver>.json)")
	return cmd
}

func runMCP(cmd *cobra.Command, cf commonFlags, args []string, output string) error {
	driverName := mcp.DefaultDriver
	if len(args) == 1 {
		driverName = args[0]
	}
	d, err := mcp.Lookup(driverName)
	if err != nil {
		return err
	}
	if cf.profile != "" && !profile.ValidName(cf.profile) {
		return fmt.Errorf("%w: %q", errInvalidProfile, cf.profile)
	}

	_, ctx, _, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	cdpURL := mcpCDPURL(ctx, cf)

	if ierr := mcp.EnsureInstalled(cmd.Context(), d); ierr != nil {
		return ierr
	}
	data, err := mcp.Marshal(mcp.BuildConfig(d, cdpURL))
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if output == "-" {
		fmt.Fprintln(out, string(data))
		return nil
	}
	path := output
	if path == "" {
		path = mcp.DefaultConfigPath(d.Name)
	}
	if err := mcp.WriteConfig(path, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s MCP config -> %s\n", d.Name, path)
	fmt.Fprintf(out, "  CDP  %s\n", cdpURL)
	return nil
}

// mcpCDPURL is the CDP endpoint the driver attaches to: the configured URL for a
// direct context, else the conventional loopback CDP port a local container or a
// held `cuttle connect` forward exposes. The profile seed is appended as
// ?fingerprint so the driver lands on the profile's browser.
func mcpCDPURL(ctx config.Context, cf commonFlags) string {
	base := "http://127.0.0.1:" + strconv.Itoa(cf.cdpPort)
	if ctx.Backend == config.BackendDirect && ctx.CDPURL != "" {
		base = ctx.CDPURL
	}
	return withFingerprint(base, cf.profile)
}
