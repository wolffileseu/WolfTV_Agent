package main

// HTTP handler for /system — the panel dashboard polls this to draw its
// CPU/memory/disk/network meters. Returns zeroes until the first sample lands.

import "net/http"

func handleSystem(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	if sysmon == nil {
		writeJSON(w, 200, SystemStats{})
		return
	}
	writeJSON(w, 200, sysmon.snapshot())
}