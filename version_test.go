package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestVersionDefaultIsDev pins the contract that a plain `go build` (no
// -ldflags) yields "dev". A future change that hardcodes a fake release
// number here trips this test -- the whole point of the git-tag wiring is
// that the code never carries a version string.
func TestVersionDefaultIsDev(t *testing.T) {
	if version != "dev" {
		t.Errorf("version = %q, want %q -- a plain build must not claim to be a release", version, "dev")
	}
}

// TestStartupBannerReferencesVersion is the load-bearing check: the banner
// must interpolate the runtime `version` variable, not a literal. Set the
// version to a marker string and require it to appear in the output.
func TestStartupBannerReferencesVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v9.9.9-testmarker"

	got := startupBanner("0.0.0.0:8788")
	if !strings.Contains(got, "v9.9.9-testmarker") {
		t.Errorf("startupBanner did not interpolate version: %q", got)
	}
	if !strings.Contains(got, "0.0.0.0:8788") {
		t.Errorf("startupBanner missing listen addr: %q", got)
	}
}

// TestNoHardcodedVersionInMain grep-checks main.go for a stray SemVer-looking
// literal. Belt and braces: if someone re-hardcodes "v1.0.0" in the log line
// after a merge conflict, this catches it before it ships.
func TestNoHardcodedVersionInMain(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Match a plausible SemVer literal appearing inside a Go string; permits
	// dev-comments to reference "v1.0.0" in prose without failing.
	semverInString := regexp.MustCompile(`"[^"]*v\d+\.\d+\.\d+[^"]*"`)
	if m := semverInString.Find(data); m != nil {
		t.Errorf("main.go still contains a hardcoded version literal: %s -- version must come from the `version` variable (set via ldflags from the git tag)", m)
	}
}

// TestDetectVersionFlag covers the exact-match cases (positive + negative) so
// the -version handling in main() cannot silently swallow config paths that
// happen to be named "-versionsomething".
func TestDetectVersionFlag(t *testing.T) {
	positive := [][]string{
		{"-version"},
		{"--version"},
		{"config.json", "-version"},
		{"-dry-run", "--version"},
	}
	for _, args := range positive {
		if !detectVersionFlag(args) {
			t.Errorf("detectVersionFlag(%v) = false, want true", args)
		}
	}
	negative := [][]string{
		nil,
		{},
		{"config.json"},
		{"-dry-run"},
		{"-versionfoo"},   // not an exact match
		{"version"},       // missing the leading dash
		{"--versionfoo"},
	}
	for _, args := range negative {
		if detectVersionFlag(args) {
			t.Errorf("detectVersionFlag(%v) = true, want false", args)
		}
	}
}

// TestVersionBinaryPrintsAndExits builds the binary with a marker version via
// ldflags, then runs `<bin> -version` and asserts it prints the marker and
// exits 0 without needing a config.json. This is the end-to-end proof that the
// -ldflags injection reaches the CLI, so the release workflow does what the
// README says.
func TestVersionBinaryPrintsAndExits(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a binary; skip under -short")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "wolftv-agent-test")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	marker := "v0.0.0-testldflags"
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+marker, "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	// Run in a scratch directory that has NO config.json, to prove -version
	// short-circuits BEFORE loadConfig (fresh box, just downloaded the binary).
	run := exec.Command(bin, "-version")
	run.Dir = tmp
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("`%s -version` failed: %v\n%s", bin, err, out)
	}
	if !strings.Contains(string(out), marker) {
		t.Errorf("`-version` output = %q, want to contain %q", out, marker)
	}
	// -config.json must not have been created (proof we exited early).
	if _, err := os.Stat(filepath.Join(tmp, "config.json")); err == nil {
		t.Error("-version created config.json; it must exit before loadConfig")
	}
}
