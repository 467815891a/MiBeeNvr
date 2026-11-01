package uvc

import "testing"

// TestBestMode covers the mode pick that decides the capture resolution.
// The bug it guards: an unset resolution made DirectShow open the pin on the
// device's default format, which is routinely the lowest one (640x480).
func TestBestMode(t *testing.T) {
	tests := []struct {
		name    string
		modes   []Mode
		wantOK  bool
		wantW   int
		wantH   int
		wantFPS float64
	}{
		{
			name:  "no modes",
			modes: nil,
		},
		{
			name: "empty list",
			modes: []Mode{
				{Width: 0, Height: 0, FPS: 30},
			},
		},
		{
			name: "picks the highest resolution over the device default",
			modes: []Mode{
				{Width: 640, Height: 480, FPS: 30, Codec: "yuyv422"},
				{Width: 1280, Height: 720, FPS: 30, Codec: "mjpeg"},
				{Width: 1920, Height: 1080, FPS: 30, Codec: "mjpeg"},
			},
			wantOK: true, wantW: 1920, wantH: 1080, wantFPS: 30,
		},
		{
			name: "same resolution breaks the tie on frame rate",
			modes: []Mode{
				{Width: 1920, Height: 1080, FPS: 15, Codec: "mjpeg"},
				{Width: 1920, Height: 1080, FPS: 30, Codec: "mjpeg"},
			},
			wantOK: true, wantW: 1920, wantH: 1080, wantFPS: 30,
		},
		{
			name: "skips entries without a usable size",
			modes: []Mode{
				{Width: 0, Height: 0, FPS: 60},
				{Width: 800, Height: 600, FPS: 30, Codec: "mjpeg"},
			},
			wantOK: true, wantW: 800, wantH: 600, wantFPS: 30,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BestMode(tc.modes)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Width != tc.wantW || got.Height != tc.wantH {
				t.Fatalf("mode = %dx%d, want %dx%d", got.Width, got.Height, tc.wantW, tc.wantH)
			}
			if got.FPS != tc.wantFPS {
				t.Fatalf("fps = %g, want %g", got.FPS, tc.wantFPS)
			}
		})
	}
}
