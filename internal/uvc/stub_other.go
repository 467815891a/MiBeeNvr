//go:build !windows

package uvc

// Non-Windows stubs. The control plane runs through a WinRT/PowerShell
// helper on Windows (pshelper_windows.go); a Linux implementation would talk
// to the UVC controls through V4L2 ioctls (V4L2_CID_PAN_ABSOLUTE /
// TILT_ABSOLUTE / ZOOM_ABSOLUTE + the relative variants) behind these same
// signatures.

import "fmt"

// ListDevices returns every locally attached capture device.
func ListDevices() ([]Device, error) {
	return nil, ErrUnsupportedPlatform
}

// ListAudioDevices returns every locally attached audio capture device.
func ListAudioDevices() ([]Device, error) {
	return nil, ErrUnsupportedPlatform
}

// ResolveCaptureTarget maps the configured device identity onto the values
// the platform's capturer needs (see the Windows implementation).
func ResolveCaptureTarget(devicePath, deviceName string) (name string, deviceNumber int, err error) {
	return "", 0, fmt.Errorf("%w: cannot resolve %q/%q", ErrUnsupportedPlatform, devicePath, deviceName)
}

// Mode is one capture mode advertised by a device's dshow pin.
type Mode struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	FPS    float64 `json:"fps"`
	Codec  string  `json:"codec"`
}

// ListDeviceModes enumerates a device's capture modes.
func ListDeviceModes(ffmpegPath, deviceName string) ([]Mode, error) {
	return nil, ErrUnsupportedPlatform
}
