package transcoding

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	probeOnce  sync.Once
	cachedCaps *HardwareCapabilities

	cachedEncoderList   map[string]bool
	cachedEncoderStderr string

	cachedDecoderList   map[string]bool
	cachedDecoderStderr string
)

func resetProbe() {
	probeOnce = sync.Once{}
	cachedCaps = nil
	cachedEncoderList = nil
	cachedEncoderStderr = ""
	cachedDecoderList = nil
	cachedDecoderStderr = ""
}

// ResetProbe clears the cached probe result (for testing).
// Exported because cross-package tests (e.g. internal/timelapse) need to reset
// the cache between subtests.
func ResetProbe() { resetProbe() }

// ProbeHardwareCapabilities probes the system for transcoding capabilities.
// Uses sync.Once for idempotent caching — zero overhead after first call.
func ProbeHardwareCapabilities(ffmpegPath string) *HardwareCapabilities {
	probeOnce.Do(func() {
		cachedCaps = probeHardware(ffmpegPath)
	})
	return cachedCaps
}

// ProbeHardwareCapabilitiesExplicit probes with a specific FFmpeg path without caching.
// Used when FFmpeg was downloaded after startup and the cached probe result is stale.
func ProbeHardwareCapabilitiesExplicit(ffmpegPath string) *HardwareCapabilities {
	return probeHardware(ffmpegPath)
}

func probeHardware(ffmpegPath string) *HardwareCapabilities {
	caps := &HardwareCapabilities{
		Arch:            runtime.GOARCH,
		H264Encoder:     "libx264",
		H265Encoder:     "libx265",
		H264EncoderType: EncoderSoftware,
		H265EncoderType: EncoderSoftware,
	}

	// Detect CPU and memory from /proc
	caps.TotalCores = detectCPU()
	caps.TotalMemoryMB = detectMemory()

	// Resolve FFmpeg path
	if ffmpegPath == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = p
		}
	}

	if ffmpegPath != "" {
		// Verify the binary actually exists
		if _, err := os.Stat(ffmpegPath); err == nil {
			caps.FFmpegAvailable = true
			caps.FFmpegPath = ffmpegPath
			caps = probeEncoders(ffmpegPath, caps)
			caps = probeDecoders(ffmpegPath, caps)
			setResolutionLimits(caps)
		}
	}

	// Detect V4L2 devices
	caps.Devices = detectVideoDevices()

	// Estimate performance
	caps.EstimatedFPS = estimateFPS(caps)
	caps.MaxConcurrentStreams = estimateMaxConcurrent(caps)

	return caps
}

// detectCPU counts CPU cores. Linux reads /proc/cpuinfo; Windows has no
// cpuinfo file — runtime.NumCPU() is exact there, so skip the read (and its
// warning log).
func detectCPU() int {
	if runtime.GOOS == "windows" {
		return runtime.NumCPU()
	}
	return detectCPUFromFile("/proc/cpuinfo")
}

// detectCPUFromFile reads a cpuinfo-format file and counts processor entries.
// Extracted for testability.
func detectCPUFromFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Failed to read cpuinfo", "path", path, "error", err)
		return runtime.NumCPU()
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	if count == 0 {
		return runtime.NumCPU()
	}
	return count
}

// detectMemory returns total RAM in MB. Linux reads /proc/meminfo; Windows
// uses GlobalMemoryStatusEx (probe_windows.go).
func detectMemory() uint64 {
	if runtime.GOOS == "windows" {
		return windowsTotalMemoryMB()
	}
	return detectMemoryFromFile("/proc/meminfo")
}

// detectMemoryFromFile reads a meminfo-format file and returns total MB.
// Extracted for testability.
func detectMemoryFromFile(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Failed to read meminfo", "path", path, "error", err)
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					return kb / 1024 // KB → MB
				}
			}
		}
	}
	return 0
}

// probeEncoders probes available hardware encoders via ffmpeg -encoders output parsing.
// Falls back to software on failure.
func probeEncoders(ffmpegPath string, caps *HardwareCapabilities) *HardwareCapabilities {
	type encoder struct {
		name    string
		encType EncoderType
	}

	var h264Candidates, h265Candidates []encoder

	switch runtime.GOARCH {
	case "arm64", "arm":
		h264Candidates = []encoder{
			{"h264_v4l2m2m", EncoderV4L2M2M},
			{"libx264", EncoderSoftware},
		}
		h265Candidates = []encoder{
			{"hevc_v4l2m2m", EncoderV4L2M2M},
			{"libx265", EncoderSoftware},
		}
	case "amd64":
		if runtime.GOOS == "windows" {
			// Windows: no VAAPI. Intel Quick Sync first (the primary Windows
			// desktop target), then NVENC for NVIDIA GPUs, then software.
			// QSV candidates are smoke-tested in testEncoder — being listed in
			// ffmpeg -encoders does not imply a working GPU/driver.
			h264Candidates = []encoder{
				{"h264_qsv", EncoderQSV},
				{"h264_nvenc", EncoderNVENC},
				{"libx264", EncoderSoftware},
			}
			h265Candidates = []encoder{
				{"hevc_qsv", EncoderQSV},
				{"hevc_nvenc", EncoderNVENC},
				{"libx265", EncoderSoftware},
			}
		} else {
			h264Candidates = []encoder{
				{"h264_vaapi", EncoderVAAPI},
				{"h264_nvenc", EncoderNVENC},
				{"libx264", EncoderSoftware},
			}
			h265Candidates = []encoder{
				{"hevc_vaapi", EncoderVAAPI},
				{"hevc_nvenc", EncoderNVENC},
				{"libx265", EncoderSoftware},
			}
		}
	default:
		h264Candidates = []encoder{{"libx264", EncoderSoftware}}
		h265Candidates = []encoder{{"libx265", EncoderSoftware}}
	}

	// Probe H.264 — pick first working encoder
	for _, enc := range h264Candidates {
		if testEncoder(ffmpegPath, enc.name) {
			caps.H264Encoder = enc.name
			caps.H264EncoderType = enc.encType
			break
		}
	}

	// Probe H.265 — pick first working encoder
	for _, enc := range h265Candidates {
		if testEncoder(ffmpegPath, enc.name) {
			caps.H265Encoder = enc.name
			caps.H265EncoderType = enc.encType
			break
		}
	}

	return caps
}

// parseEncoderListFromOutput parses ffmpeg -encoders output and returns video encoder names.
func parseEncoderListFromOutput(output string) map[string]bool {
	encoders := map[string]bool{}
	afterHeader := false
	for _, line := range strings.Split(output, "\n") {
		if !afterHeader {
			if strings.HasPrefix(strings.TrimSpace(line), "------") {
				afterHeader = true
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if len(fields[0]) >= 1 && fields[0][0] == 'V' {
			encoders[fields[1]] = true
		}
	}
	return encoders
}

// parseEncoderList runs ffmpeg -encoders and returns video encoder names + stderr for diagnostics.
func parseEncoderList(ffmpegPath string) (map[string]bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath, "-encoders")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.Debug("ffmpeg -encoders failed", "path", ffmpegPath, "error", err, "stderr", stderr.String())
		return map[string]bool{}, stderr.String()
	}

	return parseEncoderListFromOutput(stdout.String()), stderr.String()
}

// videoDeviceExists checks for /dev/video* devices. Overridden in tests.
var videoDeviceExists = func() bool {
	matches, _ := filepath.Glob("/dev/video*")
	return len(matches) > 0
}

// vaapiDeviceExists checks for DRI render devices (e.g. /dev/dri/renderD128).
// VAAPI encoders are compiled into many FFmpeg builds but require a GPU
// device. Without this check, the probe selects VAAPI even in Docker containers
// that lack GPU passthrough, causing runtime encode failures (exit code 234).
// Overridden in tests.
var vaapiDeviceExists = func() bool {
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	return len(matches) > 0
}

// testEncoder checks if an encoder is available via ffmpeg -encoders list parsing.
// For V4L2M2M encoders, also verifies /dev/video* device existence AND does a smoke test
// (encoders may be listed but the device may only support decode, not encode).
func testEncoder(ffmpegPath, encoder string) bool {
	if cachedEncoderList == nil {
		cachedEncoderList, cachedEncoderStderr = parseEncoderList(ffmpegPath)
		slog.Debug("ffmpeg -encoders probe complete", "path", ffmpegPath, "stderr", cachedEncoderStderr)
	}

	if !cachedEncoderList[encoder] {
		slog.Debug("Encoder not in ffmpeg -encoders list", "encoder", encoder, "stderr", cachedEncoderStderr)
		return false
	}

	if strings.Contains(encoder, "v4l2m2m") {
		if !videoDeviceExists() {
			slog.Debug("V4L2M2M encoder listed but no video devices", "encoder", encoder)
			return false
		}
		// Device exists but may only support decode. Smoke test: encode 1 frame.
		if !smokeTestEncoder(ffmpegPath, encoder) {
			slog.Debug("V4L2M2M encoder smoke test failed — device likely lacks encode support", "encoder", encoder)
			return false
		}
	}

	// VAAPI encoders are compiled into FFmpeg on most Linux distros but the GPU
	// device (/dev/dri/renderD128) may not be accessible (e.g. Docker without GPU
	// passthrough). Verify device existence to avoid runtime failures.
	if strings.Contains(encoder, "vaapi") {
		if !vaapiDeviceExists() {
			slog.Debug("VAAPI encoder listed but no DRI render device — falling back", "encoder", encoder)
			return false
		}
	}

	// QSV encoders are compiled into Windows/Linux FFmpeg builds but fail at
	// runtime without an Intel GPU and driver. Smoke test: encode 1 frame.
	if strings.Contains(encoder, "qsv") {
		if !smokeTestEncoder(ffmpegPath, encoder) {
			slog.Debug("QSV encoder listed but smoke test failed — no Intel GPU or driver", "encoder", encoder)
			return false
		}
	}

	return true
}

// smokeTestEncoder runs a minimal FFmpeg encode (1 frame of black) to verify the encoder
// actually works. Some V4L2 devices advertise encoders in ffmpeg -encoders but only support decode.
var smokeTestEncoder = func(ffmpegPath, encoder string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		ctx, ffmpegPath,
		"-f", "lavfi", "-i", "color=c=black:s=320x240:d=0.1:r=1",
		"-frames:v", "1",
		"-c:v", encoder,
		"-f", "null", "-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.Debug("Encoder smoke test failed", "encoder", encoder, "error", err, "stderr", stderr.String())
		return false
	}
	return true
}

// detectVideoDevices checks for /dev/video* devices.
func detectVideoDevices() []string {
	return detectVideoDevicesInDir("/dev")
}

// detectVideoDevicesInDir checks for video* device files in the given directory.
// Extracted for testability.
func detectVideoDevicesInDir(dir string) []string {
	var devices []string
	for i := range 33 {
		name := fmt.Sprintf("video%d", i)
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			devices = append(devices, path)
		}
	}
	return devices
}

// estimateFPS estimates max transcode FPS for 1080p based on hardware capabilities.
func estimateFPS(caps *HardwareCapabilities) float64 {
	var fps float64

	switch caps.H264EncoderType {
	case EncoderV4L2M2M:
		fps = float64(caps.TotalCores) * 5.0
	case EncoderVAAPI:
		fps = 30.0
	case EncoderNVENC, EncoderQSV:
		fps = 60.0
	case EncoderSoftware:
		switch caps.Arch {
		case "arm64", "arm":
			fps = float64(caps.TotalCores) * 1.0
		default:
			fps = float64(caps.TotalCores) * 5.0
		}
	}

	// Memory constraint: halve estimate if < 512 MB
	if caps.TotalMemoryMB > 0 && caps.TotalMemoryMB < 512 {
		fps *= 0.5
	}

	return fps
}

// estimateMaxConcurrent returns a safe concurrency limit based on core count.
func estimateMaxConcurrent(caps *HardwareCapabilities) int {
	switch {
	case caps.TotalCores <= 2:
		return 1
	case caps.TotalCores <= 4:
		return 2
	default:
		return 4
	}
}

// CheckSufficient checks whether the hardware can handle transcoding for the given codec.
// Returns (true, "") if sufficient, or (false, reason) if not.
func CheckSufficient(caps *HardwareCapabilities, requiredCodec string) (bool, string) {
	if !caps.FFmpegAvailable {
		return false, "FFmpeg is not installed"
	}

	switch requiredCodec {
	case "h264":
		if caps.H264Encoder == "" {
			return false, "no H.264 encoder available"
		}
	case "h265":
		if caps.H265Encoder == "" {
			return false, "no H.265 encoder available"
		}
	}

	if caps.EstimatedFPS < 1.0 {
		return false, fmt.Sprintf("estimated FPS too low (%.1f) for practical transcoding", caps.EstimatedFPS)
	}

	return true, ""
}

// probeDecoders probes available hardware decoders via ffmpeg -decoders output parsing.
// Mirrors probeEncoders pattern. On ARM, software decoders are too slow → treat as unavailable.
func probeDecoders(ffmpegPath string, caps *HardwareCapabilities) *HardwareCapabilities {
	type decoder struct {
		name    string
		decType EncoderType
	}

	var h264Candidates, h265Candidates []decoder

	switch caps.Arch {
	case "arm64", "arm":
		h264Candidates = []decoder{
			{"h264_v4l2m2m", EncoderV4L2M2M},
		}
		h265Candidates = []decoder{
			{"hevc_v4l2m2m", EncoderV4L2M2M},
		}
	case "amd64":
		if runtime.GOOS == "windows" {
			// Windows: prefer QSV hardware decoding (smoke-tested in
			// testDecoder), falling back to software — always fast enough on x86.
			h264Candidates = []decoder{
				{"h264_qsv", EncoderQSV},
				{"h264", EncoderSoftware},
			}
			h265Candidates = []decoder{
				{"hevc_qsv", EncoderQSV},
				{"hevc", EncoderSoftware},
			}
		} else {
			// Software decoders always fast enough on x86
			h264Candidates = []decoder{{"h264", EncoderSoftware}}
			h265Candidates = []decoder{{"hevc", EncoderSoftware}}
		}
	default:
		h264Candidates = []decoder{{"h264", EncoderSoftware}}
		h265Candidates = []decoder{{"hevc", EncoderSoftware}}
	}

	// Probe H.264 decoder
	for _, dec := range h264Candidates {
		if testDecoder(ffmpegPath, dec.name) {
			caps.H264Decoder = dec.name
			caps.H264DecoderType = dec.decType
			break
		}
	}

	// Probe H.265 decoder
	for _, dec := range h265Candidates {
		if testDecoder(ffmpegPath, dec.name) {
			caps.H265Decoder = dec.name
			caps.H265DecoderType = dec.decType
			break
		}
	}

	return caps
}

// parseDecoderListFromOutput parses ffmpeg -decoders output and returns video decoder names.
func parseDecoderListFromOutput(output string) map[string]bool {
	decoders := map[string]bool{}
	afterHeader := false
	for _, line := range strings.Split(output, "\n") {
		if !afterHeader {
			if strings.HasPrefix(strings.TrimSpace(line), "------") {
				afterHeader = true
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if len(fields[0]) >= 1 && fields[0][0] == 'V' {
			decoders[fields[1]] = true
		}
	}
	return decoders
}

// parseDecoderList runs ffmpeg -decoders and returns video decoder names + stderr for diagnostics.
func parseDecoderList(ffmpegPath string) (map[string]bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath, "-decoders")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.Debug("ffmpeg -decoders failed", "path", ffmpegPath, "error", err, "stderr", stderr.String())
		return map[string]bool{}, stderr.String()
	}

	return parseDecoderListFromOutput(stdout.String()), stderr.String()
}

// testDecoder checks if a decoder is available via ffmpeg -decoders list parsing.
// For V4L2M2M decoders, also verifies /dev/video* device existence.
func testDecoder(ffmpegPath, decoder string) bool {
	if cachedDecoderList == nil {
		cachedDecoderList, cachedDecoderStderr = parseDecoderList(ffmpegPath)
		slog.Debug("ffmpeg -decoders probe complete", "path", ffmpegPath, "stderr", cachedDecoderStderr)
	}

	if !cachedDecoderList[decoder] {
		slog.Debug("Decoder not in ffmpeg -decoders list", "decoder", decoder, "stderr", cachedDecoderStderr)
		return false
	}

	if strings.Contains(decoder, "v4l2m2m") && !videoDeviceExists() {
		slog.Debug("V4L2M2M decoder listed but no video devices", "decoder", decoder)
		return false
	}

	// QSV decoders are compiled into most FFmpeg builds but fail at runtime
	// without an Intel GPU and driver. Smoke test: decode 1 generated frame.
	if strings.Contains(decoder, "qsv") && !smokeTestDecoder(ffmpegPath, decoder) {
		slog.Debug("QSV decoder listed but smoke test failed — no Intel GPU or driver", "decoder", decoder)
		return false
	}

	return true
}

// smokeTestDecoder verifies a hardware decoder can actually decode: it first
// encodes two tiny frames with a software encoder (always available), then
// pushes them through the candidate decoder. Some FFmpeg builds list QSV
// decoders even when the machine has no Intel GPU — the probe must not select
// them, or every transcode dies at decoder init.
var smokeTestDecoder = func(ffmpegPath, decoder string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	format := "h264"
	swEncoder := "libx264"
	if strings.HasPrefix(decoder, "hevc") {
		format = "hevc"
		swEncoder = "libx265"
	}

	// Generate a tiny raw bitstream to a temp file, then decode one frame of it.
	tmp := filepath.Join(os.TempDir(),
		fmt.Sprintf("mibee-smoke-%s-%d.%s", format, os.Getpid(), format))
	defer os.Remove(tmp)

	gen := exec.CommandContext(ctx, ffmpegPath,
		"-f", "lavfi", "-i", "color=c=black:s=320x240:d=0.2:r=10",
		"-frames:v", "2", "-c:v", swEncoder, "-f", format, "-y", tmp)
	var genStderr bytes.Buffer
	gen.Stderr = &genStderr
	if err := gen.Run(); err != nil {
		slog.Debug("Decoder smoke test: sample generation failed",
			"decoder", decoder, "error", err, "stderr", genStderr.String())
		return false
	}

	dec := exec.CommandContext(ctx, ffmpegPath, decodeSmokeArgs(decoder, format, tmp)...)
	var decStderr bytes.Buffer
	dec.Stderr = &decStderr
	if err := dec.Run(); err != nil {
		slog.Debug("Decoder smoke test failed",
			"decoder", decoder, "error", err, "stderr", decStderr.String())
		return false
	}
	return true
}

// decodeSmokeArgs builds the decode command for a decoder smoke test. QSV
// decoders run in hwaccel form requiring hardware (qsv) output frames: on a
// machine without a working Intel GPU/driver the hwaccel init fails, so a
// silent software fallback can never false-positive the probe.
func decodeSmokeArgs(decoder, format, samplePath string) []string {
	if strings.Contains(decoder, "qsv") {
		return []string{
			"-hwaccel", "qsv", "-hwaccel_output_format", "qsv",
			"-c:v", strings.TrimSuffix(decoder, "_qsv"),
			"-f", format, "-i", samplePath,
			"-frames:v", "1", "-f", "null", "-",
		}
	}
	return []string{
		"-c:v", decoder,
		"-f", format, "-i", samplePath,
		"-frames:v", "1", "-f", "null", "-",
	}
}

// setResolutionLimits sets MaxEncodeWidth/MaxEncodeHeight based on encoder type.
func setResolutionLimits(caps *HardwareCapabilities) {
	switch caps.H264EncoderType {
	case EncoderV4L2M2M:
		caps.MaxEncodeWidth = 1920
		caps.MaxEncodeHeight = 1440
	case EncoderVAAPI, EncoderNVENC, EncoderQSV:
		caps.MaxEncodeWidth = 4096
		caps.MaxEncodeHeight = 4096
	default:
		// Software encoder: unlimited
	}
}
