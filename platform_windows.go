//go:build windows

package main

// Windows-specific process control and defaults.
// Selected automatically by the Go build tag on `go build` for Windows.

import "os/exec"

// killProcessByName terminates all processes with the given image name.
func killProcessByName(exeName string) error {
	return exec.Command("taskkill", "/IM", exeName, "/F").Run()
}

// defaultDiskPath is the volume the system monitor reports by default.
func defaultDiskPath() string {
	return "C:\\"
}

// platformName is reported in /status for debugging.
const platformName = "windows"
