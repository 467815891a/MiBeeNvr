package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/event"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/livetranscode"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/metrics"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/model"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/slogx"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/uvc"
)

var uvcLogger = slogx.Component("uvc-recorder")

// UVCRecorderConfig configures a UVCRecorder (a local USB/UVC camera).
type UVCRecorderConfig struct {
	CameraID string
	// Device identity: DevicePath (stable DirectShow symbolic link) is
	// preferred; DeviceName (friendly name) is the fallback. Both may be set.
	DevicePath string
	DeviceName string
	// Capture format pins (0 = device default).
	Width, Height, FPS int
	// Encoder selects the H.264 encoder for the capture→pipeline transcode:
	// "" | "libx264" (default) | "h264_qsv".
	Encoder string
	// FFmpegPath is the configured binary ("" = look up "ffmpeg" on PATH).
	FFmpegPath string
	// AudioDevice is the DirectShow friendly name of the microphone to record.
	// Empty = no audio capture. Requires AudioEnabled.
	AudioDevice string
	// AudioEnabled gates microphone capture (the camera's 录制声音 checkbox).
	// Audio is encoded AAC and fed to the same ingest pipeline as the video.
	AudioEnabled bool

	SegmentDur time.Duration

	Store   SegmentStore // satisfies *storage.Manager
	DB      RecordingDB
	Metrics *metrics.Metrics
	EventBus *event.EventBus
	// RecordEnabled: nil or true = record normally; false = live-only
	// (StreamHub fan-out without segment writes). Mirrors the other recorders.
	RecordEnabled *bool
}

// UVCRecorder captures a local USB (UVC) camera with an internal FFmpeg
// process (DirectShow input on Windows) and feeds the resulting H.264
// Annex-B stream into an IngestRecorder — reusing the push-ingest pipeline
// wholesale: StreamHub fan-out (HLS/WebRTC/FLV/WS), MP4 segment recording,
// health reporting and metrics all work unchanged.
//
// Lifecycle: Start() resolves the device (uvc.ResolveCaptureTarget — the
// friendly name is re-resolved from the stable device path at every capture
// start, so renamed devices keep working), spawns FFmpeg and pumps frames.
// FFmpeg exiting (unplug, driver hiccup) triggers a capped-backoff retry;
// Stop() kills the process and flushes the in-flight segment.
//
// It implements model.Recorder and satisfies the hub/HLS provider surfaces
// via the embedded *IngestRecorder (own Start/Stop shadow the embedded ones).
type UVCRecorder struct {
	cfg UVCRecorderConfig

	// Embedded (anonymous) so every hub/HLS/param surface is promoted:
	// SetHub/GetHub/HubSource (streamhub.HubHost — initStreamHub asserts
	// this interface; a named field would silently fail the assertion and
	// leave the camera without live preview), CodecParams (model.HLSProvider),
	// SPS/PPS, WriteNALU. Own Start/Stop/Status shadow the embedded ones.
	*IngestRecorder

	// epoch anchors the monotonic 90kHz PTS timeline; it survives reconnects
	// so HLS/WebRTC consumers never see DTS go backwards across a retry.
	epoch time.Time

	mu        sync.Mutex
	cancel    context.CancelFunc
	stopped   bool
	running   bool  // FFmpeg process currently alive
	audioRun  bool  // audio FFmpeg process currently alive
	lastErr   error
	audioErr  error
	wg        sync.WaitGroup

	// modeCache holds the device's best advertised capture mode (nil until
	// the first successful enumeration). The mode list cannot change while
	// the device stays plugged in, and enumerating it spawns an ffmpeg
	// process, so the result is reused across reconnects.
	modeCache *uvc.Mode
}

var _ model.Recorder = (*UVCRecorder)(nil)

// NewUVCRecorder constructs a UVCRecorder around a fresh IngestRecorder.
// The Hub is injected later by camera.initStreamHub (consistent with every
// other recorder) — promoted from the embedded IngestRecorder.
func NewUVCRecorder(cfg UVCRecorderConfig) *UVCRecorder {
	if cfg.SegmentDur <= 0 {
		cfg.SegmentDur = DefaultSegmentDur
	}
	return &UVCRecorder{
		cfg: cfg,
		IngestRecorder: NewIngestRecorder(IngestConfig{
			CameraID:      cfg.CameraID,
			Encoding:      string(model.FormatH264),
			SegmentDur:    cfg.SegmentDur,
			Store:         cfg.Store,
			DB:            cfg.DB,
			Metrics:       cfg.Metrics,
			EventBus:      cfg.EventBus,
			RecordEnabled: cfg.RecordEnabled,
		}),
		epoch: time.Now(),
	}
}

// Start enters the capture loop (retrying in the background on failures).
func (r *UVCRecorder) Start(_ context.Context) error {
	r.mu.Lock()
	if r.stopped || r.cancel != nil {
		r.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	if err := r.IngestRecorder.Start(ctx); err != nil {
		cancel()
		return fmt.Errorf("uvc %s: inner ingest recorder: %w", r.cfg.CameraID, err)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.captureLoop(ctx)
	}()
	if r.cfg.AudioEnabled && strings.TrimSpace(r.cfg.AudioDevice) != "" {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.audioLoop(ctx)
		}()
	}
	uvcLogger.Info("uvc recorder started",
		"camera_id", r.cfg.CameraID, "device", r.cfg.DeviceName, "device_path", r.cfg.DevicePath,
		"audio_device", r.cfg.AudioDevice, "audio_enabled", r.cfg.AudioEnabled)
	return nil
}

// Stop kills the capture process, waits for the loop to exit, and flushes
// the in-flight segment. Idempotent.
func (r *UVCRecorder) Stop() error {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.stopped = true
	r.mu.Unlock()

	r.wg.Wait()
	if err := r.IngestRecorder.Stop(); err != nil {
		return fmt.Errorf("uvc %s: inner stop: %w", r.cfg.CameraID, err)
	}
	uvcLogger.Info("uvc recorder stopped", "camera_id", r.cfg.CameraID)
	return nil
}

// Status reports the inner recorder's status: Recording while frames flow,
// Reconnecting while the capturer is idle/awaiting a working FFmpeg.
func (r *UVCRecorder) Status() model.RecorderStatus {
	return r.IngestRecorder.Status()
}

// LastError returns the most recent capture failure (nil when streaming).
func (r *UVCRecorder) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// ffmpegBinary resolves the FFmpeg executable for the capturer: the
// configured path first, then PATH (same precedence as the transcoding
// package). Empty string = not found.
func (r *UVCRecorder) ffmpegBinary() string {
	if p := strings.TrimSpace(r.cfg.FFmpegPath); p != "" {
		return p
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// captureLoop spawns FFmpeg and reconnects with capped backoff until the
// recorder is stopped. Backoff resets after a healthy run (>30s of frames).
func (r *UVCRecorder) captureLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		err := r.captureOnce(ctx)
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		if ctx.Err() != nil {
			return
		}

		healthy := time.Since(started) > 30*time.Second
		if err != nil {
			uvcLogger.Error("uvc capture failed, retrying",
				"camera_id", r.cfg.CameraID, "error", err, "ran_for", time.Since(started).Round(time.Second))
		}
		if healthy {
			backoff = time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// captureFormat is the resolution/frame rate the capture actually runs at.
// It can differ from the configured values: when the camera configuration
// leaves the size unset, the device's best advertised mode is used instead
// of the device default (see resolveCaptureFormat).
type captureFormat struct {
	Width  int
	Height int
	FPS    int
	// Codec is the pin format the mode was advertised with ("" = unknown).
	// Uncompressed pins (yuyv422) need several times the USB bandwidth of
	// mjpeg at the same resolution — the rate log reports the difference.
	Codec string
	// Auto is true when the format came from the device's mode list rather
	// than from the camera configuration.
	Auto bool
}

// String renders the capture format for the logs ("1920x1080@30", or
// "device default" when the size was left to the driver).
func (f captureFormat) String() string {
	if f.Width <= 0 || f.Height <= 0 {
		return "device default"
	}
	return fmt.Sprintf("%dx%d@%d", f.Width, f.Height, f.FPS)
}

// resolveCaptureFormat decides at which resolution the capture runs.
//
// An unset width/height means "whatever the device defaults to" — and
// DirectShow's default is routinely the LOWEST format the camera supports
// (640x480), even on devices that advertise 1080p or 4K. The result is a
// permanently soft picture that no camera setting can fix. So ask the device
// what it offers and take its best mode.
//
// The mode list is cached: it cannot change while the device stays plugged
// in, and enumerating it spawns an ffmpeg process (only the mode query —
// no capture).
func (r *UVCRecorder) resolveCaptureFormat(ffmpegPath, name string) captureFormat {
	format := captureFormat{Width: r.cfg.Width, Height: r.cfg.Height, FPS: r.cfg.FPS}
	if format.Width > 0 && format.Height > 0 {
		return format
	}

	r.mu.Lock()
	cached := r.modeCache
	r.mu.Unlock()
	hasCached := cached != nil

	if !hasCached {
		modes, err := uvc.ListDeviceModes(ffmpegPath, name)
		switch {
		case err != nil && !errors.Is(err, uvc.ErrUnsupportedPlatform):
			uvcLogger.Warn("uvc: capture mode enumeration failed, using the device default format",
				"camera_id", r.cfg.CameraID, "error", err)
		case err == nil:
			if mode, ok := uvc.BestMode(modes); ok {
				cached = &mode
				r.mu.Lock()
				r.modeCache = cached
				r.mu.Unlock()
			}
		}
	}

	if cached == nil {
		return format
	}
	format.Width, format.Height = cached.Width, cached.Height
	format.Codec = cached.Codec
	format.Auto = true
	if format.FPS <= 0 {
		format.FPS = int(math.Round(cached.FPS))
	}
	uvcLogger.Info("uvc: using the device's best capture mode",
		"camera_id", r.cfg.CameraID,
		"mode", fmt.Sprintf("%dx%d@%g", cached.Width, cached.Height, cached.FPS),
		"pin_codec", cached.Codec)
	return format
}

// captureOnce runs one FFmpeg capture attempt until the process exits or the
// context is cancelled. The input side first tries forcing an MJPEG capture
// pin (the pragmatic default for USB cameras — YUY2 at high resolutions
// usually exceeds USB bandwidth); if FFmpeg dies immediately without
// producing frames (camera without an MJPEG pin), it retries once with the
// device default format.
func (r *UVCRecorder) captureOnce(ctx context.Context) error {
	ffmpegPath := r.ffmpegBinary()
	if ffmpegPath == "" {
		return errors.New("ffmpeg not found (install ffmpeg or set transcoding.ffmpeg_path) — required for UVC capture")
	}

	// Re-resolve the friendly name from the stable device path at every
	// attempt: dshow only accepts the name, and the name can change across
	// replug. Same-name duplicates are disambiguated via video_device_number.
	name, deviceNumber, err := uvc.ResolveCaptureTarget(r.cfg.DevicePath, r.cfg.DeviceName)
	if err != nil {
		return fmt.Errorf("resolve device: %w", err)
	}

	format := r.resolveCaptureFormat(ffmpegPath, name)

	var lastErr error
	for attempt, forceMJPEG := range []bool{true, false} {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := r.runFFmpeg(ctx, ffmpegPath, name, deviceNumber, forceMJPEG, format)
		if err == nil || ctx.Err() != nil {
			return err
		}
		lastErr = err
		if attempt == 0 {
			uvcLogger.Warn("uvc capture attempt with forced MJPEG failed; retrying with device default format",
				"camera_id", r.cfg.CameraID, "error", err)
			time.Sleep(time.Second)
		}
	}
	return lastErr
}

// buildFFmpegArgs assembles the dshow→H.264 stdout pipeline. Kept pure for
// unit testing.
func buildFFmpegArgs(name string, deviceNumber int, forceMJPEG bool, width, height, fps int, encoder string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	// dshow input options (all must precede -i).
	args = append(args, "-f", "dshow", "-rtbufsize", "256M")
	if forceMJPEG {
		args = append(args, "-vcodec", "mjpeg")
	}
	if width > 0 && height > 0 {
		args = append(args, "-video_size", fmt.Sprintf("%dx%d", width, height))
	}
	if fps > 0 {
		args = append(args, "-framerate", strconv.Itoa(fps))
	}
	if deviceNumber > 0 {
		args = append(args, "-video_device_number", strconv.Itoa(deviceNumber))
	}
	args = append(args, "-i", fmt.Sprintf("video=%s", name))

	// Output: H.264 Annex-B on stdout, no audio (UVC video-only pipeline).
	if encoder == "" {
		encoder = "libx264"
	}
	args = append(args, "-c:v", encoder)
	if encoder == "libx264" {
		args = append(args, "-preset", "veryfast", "-tune", "zerolatency")
	} else {
		// WebRTC (WHEP) egress requires a browser-decodable profile — Chrome
		// only guarantees Constrained Baseline. QSV defaults to High, which
		// makes every WHEP attempt black-screen until the player falls back
		// to WS-FLV (the "5 seconds to load" report).
		args = append(args, "-profile:v", "baseline", "-level", "3.1")
	}
	// -bf 0: the pipeline stamps wall-clock PTS in decode-output order — any
	// B-frame reordering (h264_qsv enables them by default) breaks that
	// assumption and produces out-of-order timestamps (stutter + decoder
	// errors). Live streaming gains nothing from B-frames anyway.
	args = append(args, "-bf", "0")
	// 1-second GOP: live players (WS-FLV/WebRTC) chase the live edge between
	// keyframes — a long GOP turns any jitter into visible periodic stutter.
	gop := 30
	if fps > 0 {
		gop = fps
	}
	args = append(args, "-g", strconv.Itoa(gop), "-an", "-f", "h264", "pipe:1")
	return args
}

// runFFmpeg spawns one FFmpeg process and pumps stdout through the Annex-B
// parser into the inner ingest recorder. Returns when the process exits or
// ctx is cancelled. err is nil for clean EOF, non-nil otherwise.
func (r *UVCRecorder) runFFmpeg(ctx context.Context, ffmpegPath, name string, deviceNumber int, forceMJPEG bool, format captureFormat) error {
	args := buildFFmpegArgs(name, deviceNumber, forceMJPEG, format.Width, format.Height, format.FPS, r.cfg.Encoder)
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stderr = nil // captured below via a pipe

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	uvcLogger.Info("uvc ffmpeg capture started",
		"camera_id", r.cfg.CameraID, "device", name, "device_number", deviceNumber,
		"force_mjpeg", forceMJPEG, "encoder", orDefault(r.cfg.Encoder, "libx264"),
		"format", format.String(), "pin_codec", format.Codec, "auto_mode", format.Auto)

	// Drain stderr into a bounded tail so failures carry the driver's error
	// message (dshow reports device-busy/missing-pin reasons here).
	stderrTail := &limitedBuffer{cap: 8 * 1024}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrTail, stderr)
	}()

	parser := livetranscode.NewAnnexBStreamParser(livetranscode.CodecH264)
	gotFrames := false
	readErr := r.pumpAnnexB(ctx, stdout, parser, format, func() {
		if !gotFrames {
			gotFrames = true
			uvcLogger.Info("uvc frames flowing", "camera_id", r.cfg.CameraID)
		}
	})

	waitErr := cmd.Wait()
	<-stderrDone

	// CommandContext kills the process when ctx is cancelled — treat that as
	// a clean stop, not an error.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if readErr != nil {
		return fmt.Errorf("read stream: %w (stderr: %s)", readErr, stderrTail.String())
	}
	if waitErr != nil {
		if gotFrames {
			// A mid-stream exit after healthy frames is the unplug/reconnect
			// case — surface the driver stderr tail for diagnosis.
			return fmt.Errorf("ffmpeg exited: %w (stderr: %s)", waitErr, stderrTail.String())
		}
		return fmt.Errorf("ffmpeg exited before producing frames: %w (stderr: %s)", waitErr, stderrTail.String())
	}
	if !gotFrames {
		return errors.New("ffmpeg exited without producing frames")
	}
	return nil
}

// pumpAnnexB reads the Annex-B pipe, feeds the streaming parser and forwards
// complete access units to the inner ingest recorder with a monotonic 90kHz
// PTS. onFirstFrame fires once when the first AU arrives. Returned error is
// the read failure (nil on clean EOF).
//
// The PTS comes from a frame counter, not from the wall clock: the stdout
// pipe hands pictures over in bursts (ffmpeg's output buffering groups a
// couple of frames into one 64 KiB read), so the arrival gap of a frame
// alternates between ~1 ms and ~60 ms. Stamping the wall clock therefore
// produced a jittery timeline for everything downstream — the recording's
// stts ended up with 1 ms and 280 ms sample durations, and the live HLS part
// durations wobbled around the 1.5 s target. A regular 1/fps clock is what
// the audio pump already uses (pumpAudioADTS).
func (r *UVCRecorder) pumpAnnexB(ctx context.Context, stdout io.Reader, parser *livetranscode.AnnexBStreamParser, format captureFormat, onFirstFrame func()) error {
	buf := make([]byte, 64*1024)
	// Base the clock on the recorder epoch so the timeline stays monotonic
	// across an ffmpeg restart (a reconnect must not hand consumers a
	// backwards DTS).
	baseTicks := time.Since(r.epoch).Nanoseconds() * 90000 / int64(time.Second)
	ticksPerFrame := int64(0)
	if fps := format.FPS; fps > 0 {
		ticksPerFrame = 90000 / int64(fps)
		if ticksPerFrame < 1 {
			ticksPerFrame = 1
		}
	}

	// USB link rate diagnostics. The USB bus is the usual bottleneck of a
	// UVC camera: an uncompressed 1080p30 pin already needs ~1 Gbit/s, which
	// a USB 2.0 link cannot carry, and the symptom is a silently reduced
	// frame rate rather than an error. Sampling the delivered rate makes a
	// starved link visible in the log instead of only as a soft picture.
	var (
		rateFrames  int64
		rateBytes   int64
		windowStart = time.Now()
		windowFrame int64
		windowByte  int64
		reportCount int
	)
	var frameIndex int64

	emit := func(au livetranscode.AccessUnit) {
		if len(au) == 0 {
			return
		}
		onFirstFrame()
		rateFrames++
		windowFrame++
		var ptsTicks int64
		if ticksPerFrame > 0 {
			ptsTicks = baseTicks + frameIndex*ticksPerFrame
		} else {
			// No frame rate configured: fall back to the wall clock.
			ptsTicks = time.Since(r.epoch).Nanoseconds() * 90000 / int64(time.Second)
		}
		frameIndex++
		r.IngestRecorder.WriteNALU(au, ptsTicks, false)
	}

	reportRate := func() {
		elapsed := time.Since(windowStart)
		if elapsed < uvcRateLogInterval {
			return
		}
		windowStart = time.Now()
		// The per-window counters describe THIS window only — leaving them
		// accumulated would report a rate that grows with every window.
		fps := float64(windowFrame) / elapsed.Seconds()
		mbps := float64(windowByte) * 8 / elapsed.Seconds() / 1e6
		attrs := []any{
			"camera_id", r.cfg.CameraID,
			"format", format.String(),
			"pin_codec", format.Codec,
			"fps", math.Round(fps*10) / 10,
			"target_fps", format.FPS,
			"stream_mbps", math.Round(mbps*100) / 100,
			"frames", windowFrame,
			"total_frames", rateFrames,
			"total_bytes", rateBytes,
		}
		windowFrame = 0
		windowByte = 0
		if need, ok := usbBandwidthNeedMbps(format); ok {
			attrs = append(attrs, "usb_need_mbps", math.Round(need*100) / 100)
		} else {
			// A compressed pin's USB traffic cannot be derived from the
			// encoded stream — the measured frame rate is the reference.
			attrs = append(attrs, "usb_need_mbps", "unknown (compressed pin)")
		}
		reportCount++
		// The first window covers ffmpeg's warm-up and the device
		// negotiation — it is not a steady-state rate, so it must not be
		// judged against the target.
		if reportCount > 1 && format.FPS > 0 && fps > 0 && fps < float64(format.FPS)*0.9 {
			uvcLogger.Warn("uvc capture rate below target — the USB link may not keep up",
				append(attrs, "reason", "dropped frames")...)
			return
		}
		uvcLogger.Info("uvc capture rate", attrs...)
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, readErr := stdout.Read(buf)
		if n > 0 {
			rateBytes += int64(n)
			windowByte += int64(n)
			for _, au := range parser.Feed(buf[:n]) {
				emit(au)
			}
			reportRate()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				// Flush any pending AU before reporting EOF.
				for _, au := range parser.Flush() {
					emit(au)
				}
				return nil
			}
			return readErr
		}
	}
}

// uvcRateLogInterval is how often the delivered capture rate is logged.
const uvcRateLogInterval = 10 * time.Second

// usbBandwidthNeedMbps estimates the raw USB traffic the capture pin demands
// per second. Uncompressed pins are exact (YUYV422 carries 2 bytes per
// pixel), which is what makes a 1080p30 pin unusable on USB 2.0. Compressed
// pins (mjpeg, h264) have no fixed rate — ok is false for them.
func usbBandwidthNeedMbps(f captureFormat) (float64, bool) {
	if f.Width <= 0 || f.Height <= 0 || f.FPS <= 0 {
		return 0, false
	}
	bytesPerPixel := 0.0
	switch strings.ToLower(f.Codec) {
	case "yuyv422", "yuyv", "yuy2":
		bytesPerPixel = 2
	case "uyvy":
		bytesPerPixel = 2
	case "rgb24", "bgr24":
		bytesPerPixel = 3
	case "gray":
		bytesPerPixel = 1
	default:
		return 0, false
	}
	return float64(f.Width) * float64(f.Height) * bytesPerPixel * float64(f.FPS) * 8 / 1e6, true
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// audioLoop runs the microphone capture process with capped-backoff retry
// until the recorder stops. The audio process is fully independent from the
// video capture: a mic unplug or driver hiccup never affects the video.
func (r *UVCRecorder) audioLoop(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := r.audioOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		healthy := time.Since(started) > 30*time.Second
		if err != nil {
			uvcLogger.Error("uvc audio capture failed, retrying",
				"camera_id", r.cfg.CameraID, "device", r.cfg.AudioDevice,
				"error", err, "ran_for", time.Since(started).Round(time.Second))
		}
		if healthy {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// audioOnce runs one microphone capture attempt: ffmpeg reads the dshow audio
// device and emits AAC-ADTS frames on stdout, which we parse into raw AAC
// frames and feed to the ingest pipeline.
func (r *UVCRecorder) audioOnce(ctx context.Context) error {
	ffmpegPath := r.ffmpegBinary()
	if ffmpegPath == "" {
		return errors.New("ffmpeg not found — required for UVC audio capture")
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "dshow", "-i", fmt.Sprintf("audio=%s", r.cfg.AudioDevice),
		"-c:a", "aac", "-b:a", "64k", "-f", "adts", "pipe:1",
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrTail limitedBuffer
	cmd.Stderr = &stderrTail
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg (audio): %w", err)
	}
	uvcLogger.Info("uvc audio capture started", "camera_id", r.cfg.CameraID, "device", r.cfg.AudioDevice)

	pumpErr := r.pumpAudioADTS(ctx, stdout, func() {})
	waitErr := cmd.Wait()
	_ = pumpErr
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("ffmpeg (audio) exited: %w (stderr: %s)", waitErr, stderrTail.String())
	}
	return nil
}

// sampleRateOf maps an AAC sampling_frequency_index to Hz.
var aacSampleRates = [13]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func sampleRateOf(freqIdx int) int {
	if freqIdx >= 0 && freqIdx < len(aacSampleRates) {
		return aacSampleRates[freqIdx]
	}
	return 44100
}

// pumpAudioADTS reads the ADTS byte stream, splits it into raw AAC frames and
// feeds them to the ingest pipeline with a monotonic 90kHz audio clock.
func (r *UVCRecorder) pumpAudioADTS(ctx context.Context, stdout io.Reader, onFirstFrame func()) error {
	var buf []byte
	configured := false
	sampleRate := 44100
	channels := 1
	nextPts := int64(0)
	frameDurUs := int64(23220) // 1024 samples @ 44.1kHz, corrected on first frame

	emit := func(raw []byte, freqIdx, chanCfg int) {
		if !configured {
			sampleRate = sampleRateOf(freqIdx)
			channels = chanCfg
			// AudioSpecificConfig: AOT(5b)=2 (AAC-LC) + freqIdx(4b) + chanCfg(4b)
			aasc := []byte{
				byte((2 << 3) | (freqIdx >> 1)),
				byte(((freqIdx & 1) << 7) | (chanCfg << 3)),
			}
			r.IngestRecorder.SetAudioFormat("aac", sampleRate, channels)
			r.IngestRecorder.SetAudioConfig(aasc)
			configured = true
			onFirstFrame()
			uvcLogger.Info("uvc audio flowing", "camera_id", r.cfg.CameraID,
				"sample_rate", sampleRate, "channels", channels)
		}
		pts := nextPts
		dur := time.Duration(frameDurUs) * time.Microsecond
		nextPts += 1024 * 90000 / int64(sampleRate) // 1024 samples per AAC frame, 90kHz clock
		r.IngestRecorder.WriteAudio("aac", pts, raw, dur)
	}

	handle := func() {
		for {
			if len(buf) < 7 {
				return
			}
			// ADTS syncword: 12 bits all set
			if buf[0] != 0xFF || buf[1]&0xF0 != 0xF0 {
				buf = buf[1:] // resync: skip one byte
				continue
			}
			protectionAbsent := buf[1]&0x01 != 0
			headerLen := 7
			if !protectionAbsent {
				headerLen = 9
			}
			frameLen := (int(buf[3]&0x03) << 11) | (int(buf[4]) << 3) | (int(buf[5]) >> 5)
			if frameLen < headerLen {
				buf = buf[1:]
				continue
			}
			if len(buf) < frameLen {
				return // wait for the rest of the frame
			}
			freqIdx := int(buf[2]>>2) & 0x0F
			chanCfg := (int(buf[2]&0x01) << 2) | (int(buf[3]) >> 6)
			raw := make([]byte, frameLen-headerLen)
			copy(raw, buf[headerLen:frameLen])
			frameDurUs = 1024 * 1000000 / int64(sampleRateOf(freqIdx))
			emit(raw, freqIdx, chanCfg)
			buf = buf[frameLen:]
		}
	}

	chunk := make([]byte, 32*1024)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, readErr := stdout.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			handle()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// limitedBuffer is an io.Writer keeping only the last cap bytes (stderr tail).
type limitedBuffer struct {
	buf  []byte
	cap  int
	full bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	for _, c := range p {
		if len(b.buf) < b.cap {
			b.buf = append(b.buf, c)
		} else {
			b.full = true
			copy(b.buf, b.buf[1:])
			b.buf[len(b.buf)-1] = c
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	s := string(b.buf)
	if b.full {
		return "…" + s
	}
	return s
}
