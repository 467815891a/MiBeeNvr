package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/model"
	"github.com/go-chi/chi/v5"
)

// Range export limits. A day is the UI's unit of playback; the cap keeps a
// runaway request from pinning the encoder/IO for hours.
const (
	maxRangeDownloadSpan       = 24 * time.Hour
	maxRangeDownloadRecordings = 200
)

// handleDownloadRange streams a downloadable MP4 for a camera + time range
// (GET /api/cameras/{cameraID}/download-range?start=...&end=...&name=...).
//
// Recordings are 30 minutes wide, so the requested window almost always sits
// INSIDE one recording instead of matching one. The handler therefore selects
// the recordings that OVERLAP the window and trims each one to the part that
// falls inside it — that trim is what makes the requested start/end take
// effect. (Selecting on started_at alone, as a plain "recordings started
// between T1 and T2" query does, finds nothing at all for such a window.)
//
// The cut is anchored on the keyframe at or before the requested start: a
// stream copy can only begin on a keyframe, so the output starts at most one
// GOP early and ends exactly at the requested end. The codec is copied (no
// re-encode), so the download completes at IO speed.
func (h *Handler) handleDownloadRange(w http.ResponseWriter, r *http.Request) {
	cameraID := chi.URLParam(r, "cameraID")
	q := r.URL.Query()
	startStr, endStr := q.Get("start"), q.Get("end")
	if startStr == "" || endStr == "" {
		WriteError(w, http.StatusBadRequest, "start and end (RFC3339) are required")
		return
	}
	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid start: "+err.Error())
		return
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid end: "+err.Error())
		return
	}
	if !end.After(start) {
		WriteError(w, http.StatusBadRequest, "end must be after start")
		return
	}
	if end.Sub(start) > maxRangeDownloadSpan {
		WriteError(w, http.StatusBadRequest, "range exceeds 24h")
		return
	}

	// Find the recordings overlapping [start, end].
	if h.db == nil {
		WriteError(w, http.StatusInternalServerError, "database not available")
		return
	}
	recs, err := h.db.ListRecordingsOverlapping(r.Context(), cameraID, start, end,
		[]model.Format{model.FormatH264, model.FormatH265}, maxRangeDownloadRecordings)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "list recordings: "+err.Error())
		return
	}
	clips := h.planRangeClips(recs, start, end)
	if len(clips) == 0 {
		WriteError(w, http.StatusNotFound, "no recordings in the requested time range")
		return
	}

	// Find ffmpeg
	ffmpegPath := "ffmpeg"
	if h.config != nil && h.config.Transcoding.FFmpegPath != "" {
		ffmpegPath = h.config.Transcoding.FFmpegPath
	}
	// Fail loudly BEFORE the 200 header goes out: an empty MP4 body is
	// indistinguishable from "no footage" for the user.
	if _, err := exec.LookPath(ffmpegPath); err != nil {
		WriteError(w, http.StatusInternalServerError, "ffmpeg not available: "+err.Error())
		return
	}

	// Determine the download filename
	filename := strings.TrimSpace(q.Get("name"))
	if filename == "" {
		filename = "recording_" + cameraID + "_" + start.Format("20060102_150405")
	}
	filename = strings.Map(func(rr rune) rune {
		switch rr {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return rr
	}, filename)
	if !strings.HasSuffix(strings.ToLower(filename), ".mp4") {
		filename += ".mp4"
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(filename)))
	w.Header().Set("Cache-Control", "no-cache")

	if err := h.streamRangeClips(w, r, cameraID, clips, ffmpegPath); err != nil {
		logger.Error("download range failed",
			"camera_id", cameraID, "error", err)
	}
}

// rangeClip is one recording's contribution to the requested window.
type rangeClip struct {
	// Path is the recording file on disk.
	Path string
	// Offset is where the clip starts inside the file.
	Offset time.Duration
	// Duration is the clip length.
	Duration time.Duration
}

// planRangeClips maps the requested window onto the recordings that overlap
// it and trims each one to the part that falls inside the window.
//
// The clip start is snapped back to the keyframe at or before the requested
// start (a stream copy can only begin on a keyframe — starting mid-GOP leaves
// the first frames undecodable). The clip end is clamped to the recording's
// own end, so a window reaching past the last recording does not invent
// footage, and the start is clamped to the recording's start for the same
// reason.
func (h *Handler) planRangeClips(recs []model.Recording, start, end time.Time) []rangeClip {
	clips := make([]rangeClip, 0, len(recs))
	for _, rec := range recs {
		if rec.FilePath == "" || rec.StartedAt.IsZero() {
			continue
		}
		recEnd := rec.EndedAt
		if recEnd.IsZero() {
			// Still being written — it covers the window up to its end.
			recEnd = end
		}
		clipStart := start
		if rec.StartedAt.After(clipStart) {
			clipStart = rec.StartedAt
		}
		clipEnd := end
		if recEnd.Before(clipEnd) {
			clipEnd = recEnd
		}
		if !clipEnd.After(clipStart) {
			continue
		}
		if _, err := os.Stat(rec.FilePath); err != nil {
			// The file was reclaimed (retention/archival) after the row was
			// written — skip it instead of handing ffmpeg a missing path.
			logger.Warn("download range: recording file missing",
				"recording_id", rec.ID, "file_path", rec.FilePath, "error", err)
			continue
		}

		// Offsets inside the recording's own media timeline.
		clipStartInFile := clipStart.Sub(rec.StartedAt)
		clipEndInFile := clipEnd.Sub(rec.StartedAt)
		if anchor, ok := h.vodMgr.KeyframeAnchor(rec, clipStartInFile.Seconds()); ok {
			if anchorDur := time.Duration(anchor * float64(time.Second)); anchorDur < clipStartInFile {
				clipStartInFile = anchorDur
			}
		}
		dur := clipEndInFile - clipStartInFile
		if dur <= 0 {
			continue
		}
		clips = append(clips, rangeClip{
			Path:     rec.FilePath,
			Offset:   clipStartInFile,
			Duration: dur,
		})
	}
	return clips
}

// streamRangeClips renders the planned clips into a single MP4 and streams it
// to the client.
//
// One clip → a single ffmpeg pass over the recording file. Several clips →
// the concat DEMUXER over the trimmed windows: it keeps the stream copy. The
// concat FILTER would force a re-encode ("Filtering and streamcopy cannot be
// used together") and used to be the only option here, which is why a
// multi-recording range came back as full 30-minute segments.
func (h *Handler) streamRangeClips(w http.ResponseWriter, r *http.Request, cameraID string, clips []rangeClip, ffmpegPath string) error {
	ctx := r.Context()
	cw := &countingWriter{w: w}
	if len(clips) == 1 {
		err := runFFmpeg(ctx, cw, ffmpegPath, singleClipArgs(clips[0])...)
		if err == nil {
			return nil
		}
		if cw.n > 0 {
			// Partial output already reached the client — retrying would
			// append a second MP4 to the first.
			return err
		}
		logger.Warn("download range: stream copy failed, retrying with re-encode",
			"camera_id", cameraID, "error", err)
		return runFFmpeg(ctx, cw, ffmpegPath, singleClipReencodeArgs(clips[0])...)
	}

	listPath, err := writeConcatList(clips)
	if err != nil {
		return err
	}
	defer os.Remove(listPath)

	err = runFFmpeg(ctx, cw, ffmpegPath, concatCopyArgs(listPath)...)
	if err == nil {
		return nil
	}
	if cw.n > 0 {
		return err
	}
	logger.Warn("download range: concat stream copy failed, retrying with re-encode",
		"camera_id", cameraID, "error", err)
	return runFFmpeg(ctx, cw, ffmpegPath, concatReencodeArgs(listPath)...)
}

// singleClipArgs builds the ffmpeg invocation for a clip that lives inside a
// single recording file.
//
// -ss before -i is an input (fast) seek: ffmpeg lands on the keyframe at or
// before the offset, so the clip starts on a decodable frame. -t bounds the
// output to the clip length measured from the seek point, which keeps the END
// of the clip exact.
func singleClipArgs(c rangeClip) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", formatDuration(c.Offset),
		"-i", c.Path,
		"-t", formatDuration(c.Duration),
		"-c", "copy",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	}
}

// singleClipReencodeArgs re-encodes a single clip — the fallback when the
// stream copy fails (e.g. a container ffmpeg cannot copy out of).
func singleClipReencodeArgs(c rangeClip) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", formatDuration(c.Offset),
		"-i", c.Path,
		"-t", formatDuration(c.Duration),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	}
}

// concatCopyArgs stitches the trimmed windows of several recordings with the
// concat demuxer, keeping the stream copy.
func concatCopyArgs(listPath string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	}
}

// concatReencodeArgs re-encodes the stitched windows — the fallback when the
// recordings' codec parameters differ and the copy cannot be concatenated.
func concatReencodeArgs(listPath string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	}
}

// writeConcatList writes the ffmpeg concat-demuxer list describing the
// trimmed windows: one entry per recording, with the in/out points inside
// that recording.
//
// The demuxer needs absolute paths with forward slashes — on Windows a
// backslash inside a concat list is an escape character.
func writeConcatList(clips []rangeClip) (string, error) {
	var b strings.Builder
	for _, c := range clips {
		abs, err := filepath.Abs(c.Path)
		if err != nil {
			abs = c.Path
		}
		abs = filepath.ToSlash(abs)
		fmt.Fprintf(&b, "file '%s'\n", strings.ReplaceAll(abs, "'", `'\''`))
		fmt.Fprintf(&b, "inpoint %.3f\n", c.Offset.Seconds())
		fmt.Fprintf(&b, "outpoint %.3f\n", (c.Offset + c.Duration).Seconds())
	}
	f, err := os.CreateTemp("", "mibee-range-*.txt")
	if err != nil {
		return "", fmt.Errorf("create concat list: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write concat list: %w", err)
	}
	return f.Name(), nil
}

// countingWriter tracks how many bytes already reached the client, so a
// failed ffmpeg run knows whether it may retry with a different pipeline
// (retrying after partial output would append a second MP4 to the first).
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// runFFmpeg runs ffmpeg with stdout wired to w and returns an error carrying
// ffmpeg's stderr. The context cancels the process when the client goes away,
// so an abandoned download does not leave an encoder behind.
func runFFmpeg(ctx context.Context, w io.Writer, ffmpegPath string, args ...string) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("ffmpeg: %w", err)
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, msg)
	}
	return nil
}

// formatDuration renders a time.Duration in ffmpeg's [[HH:]MM:]SS[.mmm] format.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60
	millis := int(d.Milliseconds()) % 1000
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d.%03d", hours, minutes, seconds, millis)
	}
	return fmt.Sprintf("%02d:%02d.%03d", minutes, seconds, millis)
}
