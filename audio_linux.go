//go:build linux

package main

import "log"

// Linux no-op stubs for the Windows default-playback-device check in
// win_audio.go, so the client links and the release CI (Linux runner) builds.
// The OBS-level *silence* monitor in obs.go is cross-platform (it talks to
// obs-websocket, not the OS); only the OS default-device helper needs stubbing.
//
// These deliberately report "unknown" rather than "ok": we cannot inspect the
// PipeWire/PulseAudio default sink here, so winDefaultPlayback returns "" and
// winDeviceOK returns false. A real PipeWire/PulseAudio check can replace these
// later. Signatures match win_audio.go exactly.

// winDefaultPlayback returns "" (unknown) on Linux -- the default sink is not
// inspected.
func winDefaultPlayback() string {
	return ""
}

// winDeviceOK reports false on Linux: with no way to read the default device we
// cannot confirm it is the expected one, so we do not claim it is ok.
func winDeviceOK(name string) bool {
	return false
}

// winAudioMonitor is a no-op on Linux (the device check is Windows-only).
func winAudioMonitor() {
	log.Println("winaudio: default-device check not available on this platform (no-op)")
}
