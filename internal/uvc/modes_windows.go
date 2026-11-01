//go:build windows

package uvc

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Mode is one capture mode advertised by a device's dshow pin: a resolution
// with its maximum achievable frame rate (dshow pins report min/max fps;
// the max is what ffmpeg actually delivers).
type Mode struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	FPS    float64 `json:"fps"`
	Codec  string  `json:"codec"` // "mjpeg", "yuyv422", "h264", ...
}

var (
	modeLineRe = regexp.MustCompile(`(?:vcodec=(\S+)|pixel_format=(\S+))\s+.*?s=(\d+)x(\d+)`)
	modeFpsRe  = regexp.MustCompile(`fps=([\d.]+)`)
)

// ListDeviceModes enumerates a device's capture modes via
// `ffmpeg -list_options` (dshow prints the pin capabilities to stderr and
// then exits non-zero — that exit code is expected, the stderr IS the data).
func ListDeviceModes(ffmpegPath, deviceName string) ([]Mode, error) {
	if ffmpegPath == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = p
		} else {
			return nil, fmt.Errorf("ffmpeg not found (required to query device modes)")
		}
	}
	cmd := exec.Command(ffmpegPath, "-hide_banner", "-list_options", "true",
		"-f", "dshow", "-i", fmt.Sprintf("video=%s", deviceName))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run()

	modes := []Mode{}
	seen := map[string]bool{}
	for _, line := range strings.Split(stderr.String(), "\n") {
		m := modeLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		codec := m[1]
		if codec == "" {
			codec = m[2]
		}
		w, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		// the line carries "min ... fps=X max ... fps=Y" — the LAST fps is
		// the maximum the pin can deliver at this resolution.
		fpsAll := modeFpsRe.FindAllStringSubmatch(line, -1)
		if len(fpsAll) == 0 {
			continue
		}
		fps, _ := strconv.ParseFloat(fpsAll[len(fpsAll)-1][1], 64)
		key := fmt.Sprintf("%dx%d@%g|%s", w, h, fps, codec)
		if seen[key] {
			continue
		}
		seen[key] = true
		modes = append(modes, Mode{Width: w, Height: h, FPS: fps, Codec: codec})
	}
	sort.Slice(modes, func(i, j int) bool {
		pi, pj := modes[i].Width*modes[i].Height, modes[j].Width*modes[j].Height
		if pi != pj {
			return pi > pj
		}
		return modes[i].FPS > modes[j].FPS
	})
	return modes, nil
}
