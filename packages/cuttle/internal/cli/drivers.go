package cli

// driverPlaywright is the executable name of the driver the cuttle image bundles.
const driverPlaywright = "playwright-cli"

// BundledPlaywrightCLIVersion is the playwright-cli the cuttle image bundles and
// `cuttle pw` execs. It is a literal because the Go build cannot read
// packages/browser/versions.env; TestBundledPlaywrightCLIPin cross-checks it
// against that file's PLAYWRIGHT_CLI_VERSION and the Dockerfile's ARG, so the
// briefing can never name a version the image does not carry.
const BundledPlaywrightCLIVersion = "0.1.20"
