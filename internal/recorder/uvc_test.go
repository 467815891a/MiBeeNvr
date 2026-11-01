package recorder

import (
	"strings"
	"testing"
)

// TestBuildFFmpegArgsDefault asserts the default dshow→H.264 stdout pipeline:
// MJPEG-forced input, libx264 veryfast/zerolatency, sane GOP, no audio.
func TestBuildFFmpegArgsDefault(t *testing.T) {
	args := buildFFmpegArgs("HD Webcam", 0, true, 1920, 1080, 30, "", "")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-f dshow", "-rtbufsize 256M", "-vcodec mjpeg",
		"-video_size 1920x1080", "-framerate 30",
		"-i video=HD Webcam",
		"-c:v libx264", "-preset veryfast", "-tune zerolatency",
		"-bf 0", "-g 30", "-an", "-f h264", "pipe:1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	// Input options must precede -i (dshow demuxer options are input-side).
	if i := indexOf(args, "-i"); i < 0 || indexOf(args, "-video_size") > i || indexOf(args, "-framerate") > i {
		t.Errorf("dshow input options must precede -i:\n%s", joined)
	}
	// Output options must follow -i.
	if i := indexOf(args, "-i"); i < 0 || indexOf(args, "-c:v") < i {
		t.Errorf("output options must follow -i:\n%s", joined)
	}
}

// TestBuildFFmpegArgsQSV asserts the QSV encoder selection: no x264-specific
// presets/tune, GOP derived from fps, and explicit device number when given.
func TestBuildFFmpegArgsQSV(t *testing.T) {
	args := buildFFmpegArgs("PTZ Cam", 1, false, 0, 0, 25, "h264_qsv", "")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c:v h264_qsv", "-bf 0", "-g 25", "-video_device_number 1", "video=PTZ Cam"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	for _, banned := range []string{"-preset veryfast", "-tune zerolatency", "-vcodec mjpeg"} {
		if strings.Contains(joined, banned) {
			t.Errorf("args must not contain %q:\n%s", banned, joined)
		}
	}
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}
