package cli

import (
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

// `cuttle auth status` answers the question every session opens with - "am I
// already signed in here?" - without a login attempt. Sessions were being
// re-logged-in over and over because nothing surfaced what the profile already
// held, and each of those attempts is a credential handling event: a password
// typed, sometimes a second factor burned, sometimes a human interrupted.
//
// It reports what is knowable and nothing more. A cookie for an origin is NOT
// proof of a valid session - the server can have expired it - so the output says
// "has cookies", never "logged in", and the honest next step is to navigate and
// look.

func init() { AddCommand(newAuthCmd()) }

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "what the session's profile is already signed into",
	}
	cmd.AddCommand(newAuthStatusCmd())
	return cmd
}

func newAuthStatusCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:   "status [origin]",
		Short: "list the origins the browser holds cookies for (never their values)",
		Long: `Reads the running browser's cookie jar and reports, per domain, how many
cookies it holds, how many are session cookies, and when the first one expires.
Values and names never leave the daemon.

Check this BEFORE driving a login. Cookies are not proof of a valid session -
the server can have expired it - so treat a hit as "probably still signed in,
navigate and look", not as a guarantee.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			origin := ""
			if len(args) == 1 {
				origin = args[0]
			}
			return runAuthStatus(cmd, cf, origin)
		},
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

func runAuthStatus(cmd *cobra.Command, cf commonFlags, origin string) error {
	base, release, err := secretEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()

	endpoint := base + "/auth"
	if origin != "" {
		endpoint += "?origin=" + url.QueryEscape(origin)
	}
	var payload struct {
		Origins []struct {
			Domain        string `json:"domain"`
			Cookies       int    `json:"cookies"`
			Session       int    `json:"session_cookies"`
			SoonestExpiry string `json:"soonest_expiry"`
		} `json:"origins"`
	}
	if err := getJSON(cmd.Context(), endpoint, &payload); err != nil {
		return fmt.Errorf("reading auth status: %w", err)
	}
	out := cmd.OutOrStdout()
	if len(payload.Origins) == 0 {
		if origin != "" {
			fmt.Fprintf(out, "no cookies for %s - assume signed out\n", origin)
			return nil
		}
		fmt.Fprintln(out, "the browser holds no cookies - assume signed out")
		return nil
	}
	for _, o := range payload.Origins {
		line := fmt.Sprintf("%s\t%d cookies", o.Domain, o.Cookies)
		if o.Session > 0 {
			line += fmt.Sprintf(" (%d session)", o.Session)
		}
		if o.SoonestExpiry != "" {
			line += "\tfirst expiry " + humanExpiry(o.SoonestExpiry)
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, "cookies are not proof of a valid session - navigate and look before assuming you are signed in")
	return nil
}

// humanExpiry renders an RFC3339 expiry with how far away it is, and says so
// plainly when it has already passed.
func humanExpiry(stamp string) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp
	}
	d := time.Until(t)
	if d <= 0 {
		return stamp + " (expired)"
	}
	return fmt.Sprintf("%s (in %s)", stamp, d.Round(time.Hour))
}
