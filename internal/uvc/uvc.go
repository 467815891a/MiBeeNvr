// Package uvc talks to local USB (UVC) cameras: device enumeration and PTZ
// (pan/tilt/zoom) control via the UVC CameraTerminal controls, exposed by
// DirectShow as IAMCameraControl.
//
// The capture path itself (pulling video frames) belongs to the recorder
// (internal/recorder/uvc.go spawns an FFmpeg dshow process); this package only
// performs control-plane operations: listing devices, reading/writing camera
// controls. On Windows everything is implemented with the standard no-cgo COM
// vtable pattern (see internal/install/install_windows.go for the in-repo
// precedent); other platforms return ErrUnsupportedPlatform (a Linux v4l2
// implementation would slot in behind the same signatures).
package uvc

import "errors"

// Device is one locally attached capture device.
type Device struct {
	// Name is the DirectShow friendly name (e.g. "HD Pro Webcam C920").
	Name string `json:"name"`
	// Path is the stable device path (the DirectShow symbolic link, e.g.
	// `\\?\usb#vid_046d&pid_0825&mi_00#...#{65e8773d-...}\global`). It
	// survives replug and re-enumeration and is therefore the preferred
	// persistent identity; it may be empty for non-PnP (software) devices.
	Path string `json:"path"`
}

// PTZAxis describes one camera-control axis (pan / tilt / zoom) as reported
// by IAMCameraControl::GetRange.
type PTZAxis struct {
	// Supported is false when the device rejected GetRange for this axis
	// (E_PROP_ID_UNSUPPORTED) — most fixed webcams have no pan/tilt at all.
	Supported bool `json:"supported"`
	// Value is the current position (0 when unsupported).
	Value int `json:"value"`
	// Min/Max/Step/Default come straight from GetRange.
	Min     int `json:"min"`
	Max     int `json:"max"`
	Step    int `json:"step"`
	Default int `json:"default"`
	// Relative/Absolute report the flags the device advertises. Relative
	// means Set(value, Flags_Relative) performs a directional step of the
	// given magnitude; Absolute means Set(value, Flags_Absolute) seeks to a
	// fixed position (presets rely on this).
	Relative bool `json:"relative"`
	Absolute bool `json:"absolute"`
	// Auto/Manual report automatic control support (mainly zoom/exposure).
	Auto   bool `json:"auto"`
	Manual bool `json:"manual"`
}

// PTZCapabilities is the per-axis capability dump for one device.
type PTZCapabilities struct {
	Pan  PTZAxis `json:"pan"`
	Tilt PTZAxis `json:"tilt"`
	Zoom PTZAxis `json:"zoom"`
}

// PTZPosition is an absolute pan/tilt/zoom position (stored in camera PTZ
// presets; UVC devices have no device-side preset memory).
type PTZPosition struct {
	Pan  int `json:"pan"`
	Tilt int `json:"tilt"`
	Zoom int `json:"zoom"`
}

var (
	// ErrUnsupportedPlatform is returned by every operation on platforms the
	// package does not implement (Windows is the first implementation).
	ErrUnsupportedPlatform = errors.New("uvc: local USB camera control is only supported on Windows")
	// ErrDeviceNotFound is returned when the configured device is not
	// currently attached (unplugged, or the friendly name changed).
	ErrDeviceNotFound = errors.New("uvc: device not found")
	// ErrAxisUnsupported is returned when a device does not expose the
	// requested camera control (e.g. pan/tilt on a fixed webcam).
	ErrAxisUnsupported = errors.New("uvc: camera control not supported by device")
)
