//go:build windows

package uvc

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// Device enumeration for UVC cameras (Windows).
//
// The control-plane operations (PTZ) live in the PowerShell helper
// (pshelper_windows.go + client_windows.go); this file only provides the
// device enumeration used by GET /api/uvc/devices and by the capture-start
// device resolution.

// ListDevices returns every locally attached capture device.
func ListDevices() ([]Device, error) {
	return withCOM(enumVideoDevices)
}

// ListAudioDevices returns every locally attached audio capture (microphone)
// device — the input picker for UVC cameras that record sound.
func ListAudioDevices() ([]Device, error) {
	return withCOM(func() ([]Device, error) {
		return enumDeviceCategory(&clsidAudioInputDeviceCategory, "audio-input")
	})
}

// ResolveCaptureTarget maps the configured device identity onto the two
// values FFmpeg's dshow demuxer accepts: the current friendly name and a
// 0-based device_number (needed only when several attached devices share the
// same friendly name). The device path is preferred as the identity because
// it survives replug and rename; the name is re-resolved from it at every
// capture start so a renamed device keeps working.
func ResolveCaptureTarget(devicePath, deviceName string) (name string, deviceNumber int, err error) {
	devices, enumErr := withCOM(enumVideoDevices)
	if enumErr != nil {
		return "", 0, enumErr
	}

	var matches []Device
	if devicePath != "" {
		for _, d := range devices {
			if d.Path != "" && equalFoldPath(d.Path, devicePath) {
				matches = append(matches, d)
			}
		}
	}
	if len(matches) == 0 && deviceName != "" {
		for _, d := range devices {
			if d.Name == deviceName {
				matches = append(matches, d)
			}
		}
	}
	if len(matches) == 0 {
		return "", 0, fmt.Errorf("%w: path=%q name=%q (attached: %d)", ErrDeviceNotFound, devicePath, deviceName, len(devices))
	}

	// Same-name duplicates are disambiguated by position in the enumeration,
	// which is exactly the order FFmpeg's video_device_number uses.
	name = matches[0].Name
	if name == "" {
		return "", 0, fmt.Errorf("%w: no friendly name for path=%q", ErrDeviceNotFound, devicePath)
	}
	if len(matches) > 1 {
		sameNameIdx := 0
		for _, d := range devices {
			if d.Name != name {
				continue
			}
			if d.Path == matches[0].Path {
				deviceNumber = sameNameIdx
				break
			}
			sameNameIdx++
		}
	}
	return name, deviceNumber, nil
}

// equalFoldPath compares device paths case-insensitively (the symbolic links
// are case-stable, but hand-edited configs are not).
func equalFoldPath(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// comReleaseUnused keeps the windows import anchored for the COM plumbing in
// com_windows.go when this file is compiled standalone in some tool views.
var _ = windows.CreateFile
