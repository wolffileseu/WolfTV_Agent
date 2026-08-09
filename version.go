package main

import "fmt"

// version is the single source of truth for the agent's release identifier.
// The default "dev" is what a plain `go build` yields (no ldflags), so an
// untagged binary can never claim to be a release. Release builds pass
// `-ldflags "-X main.version=$(git describe --tags --always --dirty)"`, which
// on a tagged commit is a clean SemVer (v1.0.0) and off a tag is something
// like v1.0.0-3-gabc1234 -- obviously not a release.
var version = "dev"

// startupBanner is the line logged when the agent starts serving. Extracted so
// tests can assert it interpolates the runtime `version` variable rather than
// a hardcoded literal (which would drift from the git tag).
func startupBanner(listen string) string {
	return fmt.Sprintf("wolftv-agent %s listening on %s", version, listen)
}

// detectVersionFlag reports whether -version / --version appears in args, so
// `wolftv-agent.exe -version` prints and exits without touching config.json.
// Split from the print+exit so it is trivially testable without spawning a
// subprocess or capturing stdout.
func detectVersionFlag(args []string) bool {
	for _, a := range args {
		if a == "-version" || a == "--version" {
			return true
		}
	}
	return false
}
