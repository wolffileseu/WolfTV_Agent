package main

import (
	"log"
	"strconv"
	"strings"
	"sync"
)

// Resolution presets. The stream resolution used to live in et_args as raw
// r_mode / r_customwidth / r_customheight, which is fiddly to edit and easy to
// get wrong. The `resolution` config field ("720p".."2160p") is translated into
// those cvars when the launch args are built, for both the live and the replay
// instance.

var resolutionPresets = map[string][2]int{
	"720p":  {1280, 720},
	"1080p": {1920, 1080},
	"1440p": {2560, 1440},
	"2160p": {3840, 2160},
}

const defaultResolution = "1080p"

// resolutionArgs returns the `+set` args to append for a preset, and two flags:
// overridden = et_args already sets a custom resolution so the preset is skipped
// (explicit wins); unknown = the preset name was not recognised and 1080p was
// substituted. An empty preset returns no args (leave et_args untouched). Pure,
// so the translation and the override case are unit-tested.
func resolutionArgs(preset string, existing []string) (add []string, overridden, unknown bool) {
	if strings.TrimSpace(preset) == "" {
		return nil, false, false
	}
	// explicit r_custom* in et_args wins over the preset
	if argValue(existing, "r_customwidth") != "" || argValue(existing, "r_customheight") != "" {
		return nil, true, false
	}
	dims, ok := resolutionPresets[strings.ToLower(strings.TrimSpace(preset))]
	if !ok {
		dims = resolutionPresets[defaultResolution]
		unknown = true
	}
	add = []string{
		"+set", "r_mode", "-1",
		"+set", "r_customwidth", strconv.Itoa(dims[0]),
		"+set", "r_customheight", strconv.Itoa(dims[1]),
	}
	return add, false, unknown
}

// log the override / unknown-fallback at most once each, so repeated launches
// (live + replay, restarts) do not spam.
var (
	resOverrideOnce sync.Once
	resUnknownOnce  sync.Once
)

// withResolution appends the preset's cvars to a copy of args (later +set wins,
// so the preset overrides any earlier r_mode). Explicit r_custom* in et_args is
// respected. Non-mutating.
func withResolution(args []string, preset string) []string {
	add, overridden, unknown := resolutionArgs(preset, args)
	if overridden {
		resOverrideOnce.Do(func() {
			log.Printf("resolution: et_args already sets a custom resolution -- preset %q ignored", preset)
		})
	}
	if unknown {
		resUnknownOnce.Do(func() {
			log.Printf("resolution: unknown preset %q -- falling back to %s", preset, defaultResolution)
		})
	}
	out := append([]string{}, args...)
	return append(out, add...)
}
