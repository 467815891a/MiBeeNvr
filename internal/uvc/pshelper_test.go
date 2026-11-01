package uvc

import (
	"testing"
)

// TestPSHelperRoundTrip drives the PowerShell helper end to end against the
// real device: caps dump, position read, an actual pan move and restore.
// Skipped when no capture device is attached.
func TestPSHelperRoundTrip(t *testing.T) {
	devices, err := ListDevices()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devices) == 0 {
		t.Skip("no capture device attached")
	}
	dev := devices[0]
	t.Logf("device: %q", dev.Name)

	ctrl := NewUVCController(dev)
	defer ctrl.Stop()

	caps, err := ctrl.GetCapabilities()
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	t.Logf("pan:  %+v", caps.Pan)
	t.Logf("tilt: %+v", caps.Tilt)
	t.Logf("zoom: %+v", caps.Zoom)

	pos, err := ctrl.GetPosition()
	if err != nil {
		t.Fatalf("GetPosition: %v", err)
	}
	t.Logf("position: %+v", pos)

	if !caps.Pan.Supported || caps.Pan.Min == caps.Pan.Max {
		t.Skip("pan not supported on this device")
	}

	target := pos.Pan + 1
	if target > caps.Pan.Max {
		target = pos.Pan - 1
	}
	if err := ctrl.MoveAbsolute(&target, nil, nil); err != nil {
		t.Fatalf("MoveAbsolute pan: %v", err)
	}
	after, err := ctrl.GetPosition()
	if err != nil {
		t.Fatalf("GetPosition after move: %v", err)
	}
	t.Logf("pan after move: %d (want %d)", after.Pan, target)
	if after.Pan != target {
		t.Errorf("pan did not reach target: got %d want %d", after.Pan, target)
	}
	if err := ctrl.MoveAbsolute(&pos.Pan, nil, nil); err != nil {
		t.Fatalf("pan restore: %v", err)
	}

	// Relative path (MoveBy) — one nudge and one back.
	if err := ctrl.MoveBy(1, 0); err != nil {
		t.Fatalf("MoveBy: %v", err)
	}
	if err := ctrl.MoveBy(-1, 0); err != nil {
		t.Fatalf("MoveBy back: %v", err)
	}

	// Zoom round-trip when supported.
	if caps.Zoom.Supported && caps.Zoom.Min != caps.Zoom.Max {
		if err := ctrl.ZoomStep(caps.Zoom.Step); err != nil {
			t.Fatalf("ZoomStep: %v", err)
		}
		if err := ctrl.MoveAbsolute(nil, nil, &pos.Zoom); err != nil {
			t.Fatalf("zoom restore: %v", err)
		}
		t.Logf("zoom round-trip ok")
	}
}
