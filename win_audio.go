//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

/* Windows default playback device check.
 * Detects the classic RDP hijack: a remote session switches the default
 * output to "Remoteaudio" and ET plays into the void. We ask Windows for
 * the current default render endpoint (via a small PowerShell/COM shim)
 * and compare it against win_expect_default (e.g. "CABLE Input").
 * Checked once before /start and every 60s afterwards. */

const psDefaultAudio = `Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public enum EDataFlow { eRender, eCapture, eAll }
public enum ERole { eConsole, eMultimedia, eCommunications }
[ComImport, Guid("BCDE0395-E52F-467C-8E3D-C4579291692E")]
public class MMDeviceEnumeratorCom {}
[Guid("A95664D2-9614-4F35-A746-DE8DB63617E6"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMMDeviceEnumerator {
    int EnumAudioEndpoints(EDataFlow dataFlow, int dwStateMask, out IntPtr devices);
    int GetDefaultAudioEndpoint(EDataFlow dataFlow, ERole role, out IMMDevice endpoint);
}
[Guid("D666063F-1587-4E43-81F1-B948E807363F"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMMDevice {
    int Activate(ref Guid id, int clsCtx, IntPtr activationParams, out IntPtr iface);
    int OpenPropertyStore(int stgmAccess, out IPropertyStore properties);
}
[Guid("886d8eeb-8cf2-4446-8d02-cdba1dbdcf99"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IPropertyStore {
    int GetCount(out int count);
    int GetAt(int index, out PropertyKey key);
    int GetValue(ref PropertyKey key, out PropVariant value);
}
[StructLayout(LayoutKind.Sequential)]
public struct PropertyKey { public Guid fmtid; public int pid; }
[StructLayout(LayoutKind.Explicit)]
public struct PropVariant {
    [FieldOffset(0)] public short vt;
    [FieldOffset(8)] public IntPtr pointerValue;
}
public static class WtvAudio {
    public static string GetDefaultPlayback() {
        var enumerator = (IMMDeviceEnumerator)new MMDeviceEnumeratorCom();
        IMMDevice dev;
        if (enumerator.GetDefaultAudioEndpoint(EDataFlow.eRender, ERole.eMultimedia, out dev) != 0) return "";
        IPropertyStore store;
        if (dev.OpenPropertyStore(0, out store) != 0) return "";
        var key = new PropertyKey { fmtid = new Guid("a45c254e-df1c-4efd-8020-67d146a850e0"), pid = 14 };
        PropVariant val;
        if (store.GetValue(ref key, out val) != 0) return "";
        return Marshal.PtrToStringUni(val.pointerValue);
    }
}
'@
[WtvAudio]::GetDefaultPlayback()
`

// winDefaultPlayback returns the friendly name of the current default
// playback device ("" on any failure).
func winDefaultPlayback() string {
	p := filepath.Join(os.TempDir(), "wtv_defaultaudio.ps1")
	if _, err := os.Stat(p); err != nil {
		if err := os.WriteFile(p, []byte(psDefaultAudio), 0644); err != nil {
			return ""
		}
	}
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-File", p).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func winDeviceOK(name string) bool {
	return name != "" &&
		strings.Contains(strings.ToLower(name), strings.ToLower(cfg.WinExpectDefault))
}

func winAudioMonitor() {
	if cfg.WinExpectDefault == "" {
		log.Println("winaudio: device check disabled (win_expect_default empty)")
		return
	}
	for {
		name := winDefaultPlayback()
		st.mu.Lock()
		prev := st.winDefaultOut
		st.winDefaultOut = name
		st.mu.Unlock()
		if name != prev {
			if winDeviceOK(name) {
				log.Printf("winaudio: default playback = %q (ok)", name)
			} else {
				log.Printf("winaudio: WARNING default playback = %q, expected %q -- RDP hijack?",
					name, cfg.WinExpectDefault)
			}
		}
		time.Sleep(60 * time.Second)
	}
}
