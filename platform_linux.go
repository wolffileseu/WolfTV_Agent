//go:build linux

package main

// Linux-specific process control and defaults.
// Selected automatically by the Go build tag on `go build` for Linux.

import "os/exec"

// killProcessByName terminates all processes with the given name.
// pkill -f matches against the full command line, which is what we want for
// ET (the binary may be launched via a path) and OBS.
func killProcessByName(exeName string) error {
	// Strip a trailing .exe if a Windows-style name sneaks in from config.
	name := exeName
	if len(name) > 4 && name[len(name)-4:] == ".exe" {
		name = name[:len(name)-4]
	}
	return exec.Command("pkill", "-f", name).Run()
}

// defaultDiskPath is the volume the system monitor reports by default.
func defaultDiskPath() string {
	return "/"
}

// platformName is reported in /status for debugging.
const platformName = "linux"
