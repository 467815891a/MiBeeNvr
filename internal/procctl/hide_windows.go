//go:build windows

package procctl

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideConsoleWindow suppresses the console window a console-mode child would
// otherwise open on the Windows desktop.
//
// WHY: the NVR ships as a GUI-subsystem binary (-H windowsgui), so it has no
// console of its own. Every child it spawns (ffmpeg.exe for the capture, the
// microphone, the transcoder and the mergers; ffprobe.exe for the probes;
// powershell.exe for the UVC PTZ helper) is a console application, and a
// console application started without a console to inherit ALLOCATES one —
// a black window that shows up on the taskbar and stays there for the whole
// lifetime of the capture. A single UVC camera spawns two of them (video +
// microphone), and the camera-settings page's device-mode query flashes a
// third one every time it opens.
//
// WHAT: CREATE_NO_WINDOW runs the child without allocating a console at all;
// HideWindow additionally hides the window handle should the child create one
// anyway. Neither flag touches the standard handles — Go hands the child the
// pipe handles the parent installed (or os.DevNull when no writer is set), so
// ffmpeg's stdout/stderr plumbing is unaffected.
func HideConsoleWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
