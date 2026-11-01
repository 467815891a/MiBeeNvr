package uvc

import (
	"errors"
	"testing"
)

// TestListDevices exercises the real DirectShow enumeration path. On a
// machine without cameras it must return an empty (non-nil on Windows) list
// without error; on any machine it must not panic. Devices, when present,
// must have at least one identity field.
func TestListDevices(t *testing.T) {
	devices, err := ListDevices()
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	t.Logf("attached capture devices: %d", len(devices))
	for _, d := range devices {
		t.Logf("  name=%q path=%q", d.Name, d.Path)
		if d.Name == "" && d.Path == "" {
			t.Errorf("device with empty name and path: %+v", d)
		}
	}
}

// TestResolveCaptureTargetMiss asserts the not-found contract for an identity
// no attached device can have.
func TestResolveCaptureTargetMiss(t *testing.T) {
	_, _, err := ResolveCaptureTarget(`\\?\no-such-device-path`, "No Such Camera")
	if !errors.Is(err, ErrDeviceNotFound) && !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("want ErrDeviceNotFound/ErrUnsupportedPlatform, got %v", err)
	}
}
