package uvc

import (
	"testing"
)

// TestListDeviceModes runs the real ffmpeg -list_options enumeration against
// the first attached device. Skipped when no device is present.
func TestListDeviceModes(t *testing.T) {
	devices, err := ListDevices()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devices) == 0 {
		t.Skip("no capture device attached")
	}
	modes, err := ListDeviceModes("", devices[0].Name)
	if err != nil {
		t.Fatalf("ListDeviceModes: %v", err)
	}
	if len(modes) == 0 {
		t.Fatalf("no modes reported for %q", devices[0].Name)
	}
	t.Logf("modes for %q: %d", devices[0].Name, len(modes))
	for i, m := range modes {
		if i >= 12 {
			t.Logf("  ... (%d more)", len(modes)-12)
			break
		}
		t.Logf("  %dx%d @ %.0ffps [%s]", m.Width, m.Height, m.FPS, m.Codec)
	}
}
