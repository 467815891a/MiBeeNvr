package recorder

import (
	"math"
	"testing"
)

// TestUSBBandwidthNeedMbps covers the USB traffic estimate that goes with
// the capture-rate log. Uncompressed pins have an exact requirement — that
// is the number that shows a 1080p30 pin cannot fit on a USB 2.0 link.
func TestUSBBandwidthNeedMbps(t *testing.T) {
	tests := []struct {
		name   string
		format captureFormat
		want   float64
		wantOK bool
	}{
		{
			name:   "yuyv422 is exact",
			format: captureFormat{Width: 1920, Height: 1080, FPS: 30, Codec: "yuyv422"},
			want:   1920 * 1080 * 2 * 30 * 8 / 1e6,
			wantOK: true,
		},
		{
			name:   "rgb24 carries three bytes per pixel",
			format: captureFormat{Width: 640, Height: 480, FPS: 30, Codec: "rgb24"},
			want:   640 * 480 * 3 * 30 * 8 / 1e6,
			wantOK: true,
		},
		{
			name:   "gray carries one byte per pixel",
			format: captureFormat{Width: 640, Height: 480, FPS: 30, Codec: "gray"},
			want:   640 * 480 * 1 * 30 * 8 / 1e6,
			wantOK: true,
		},
		{
			name:   "compressed pins have no fixed rate",
			format: captureFormat{Width: 1920, Height: 1080, FPS: 30, Codec: "mjpeg"},
		},
		{
			name:   "unknown codec",
			format: captureFormat{Width: 1920, Height: 1080, FPS: 30, Codec: "h264"},
		},
		{
			name:   "no size",
			format: captureFormat{FPS: 30, Codec: "yuyv422"},
		},
		{
			name:   "no frame rate",
			format: captureFormat{Width: 1920, Height: 1080, Codec: "yuyv422"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := usbBandwidthNeedMbps(tc.format)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("need = %v Mbps, want %v", got, tc.want)
			}
		})
	}
}

// TestCaptureFormatString documents how the capture format is rendered in
// the logs — an unset size must not look like a real resolution.
func TestCaptureFormatString(t *testing.T) {
	if got := (captureFormat{Width: 1920, Height: 1080, FPS: 30}).String(); got != "1920x1080@30" {
		t.Fatalf("string = %q, want %q", got, "1920x1080@30")
	}
	if got := (captureFormat{}).String(); got != "device default" {
		t.Fatalf("string = %q, want %q", got, "device default")
	}
}
