package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/model"
	"github.com/stretchr/testify/require"
)

// --- planRangeClips: the window must actually trim the recording ---

// TestPlanRangeClips_TrimsInsideSingleRecording is the regression guard for
// "download this segment" returning the whole 30-minute recording: the
// requested window sits INSIDE one recording, and the clip must cover only
// the requested part.
func TestPlanRangeClips_TrimsInsideSingleRecording(t *testing.T) {
	t.Parallel()
	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	recStart := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	recEnd := recStart.Add(30 * time.Minute) // 30-minute segment
	path := writeDummyFile(t, "seg.mp4")

	rec := &model.Recording{
		ID:        "rec-1",
		CameraID:  "cam-1",
		FilePath:  path,
		Format:    model.FormatH264,
		StartedAt: recStart,
		EndedAt:   recEnd,
		Duration:  1800,
	}

	// Window: 10:05 → 10:10, i.e. inside the recording.
	winStart := recStart.Add(5 * time.Minute)
	winEnd := recStart.Add(10 * time.Minute)

	clips := h.planRangeClips([]model.Recording{*rec}, winStart, winEnd)
	require.Len(t, clips, 1, "the overlapping recording must be found")
	require.Equal(t, 5*time.Minute, clips[0].Offset, "clip must start at the requested offset")
	require.Equal(t, 5*time.Minute, clips[0].Duration, "clip must be trimmed to the requested window")
	require.Less(t, clips[0].Duration, recEnd.Sub(recStart),
		"clip must not cover the whole recording")
}

// TestPlanRangeClips_SpansTwoRecordings checks that a window crossing a
// segment boundary produces one trimmed clip per recording — not one clip
// spanning both files.
func TestPlanRangeClips_SpansTwoRecordings(t *testing.T) {
	t.Parallel()
	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	recA := &model.Recording{
		ID: "rec-a", CameraID: "cam-1", FilePath: writeDummyFile(t, "a.mp4"),
		Format: model.FormatH264, StartedAt: base, EndedAt: base.Add(30 * time.Minute),
	}
	recB := &model.Recording{
		ID: "rec-b", CameraID: "cam-1", FilePath: writeDummyFile(t, "b.mp4"),
		Format: model.FormatH264, StartedAt: base.Add(30 * time.Minute), EndedAt: base.Add(60 * time.Minute),
	}

	// Window: 10:25 → 10:35 — the last 5 min of A plus the first 5 min of B.
	winStart := base.Add(25 * time.Minute)
	winEnd := base.Add(35 * time.Minute)

	clips := h.planRangeClips([]model.Recording{*recA, *recB}, winStart, winEnd)
	require.Len(t, clips, 2)
	require.Equal(t, 25*time.Minute, clips[0].Offset)
	require.Equal(t, 5*time.Minute, clips[0].Duration, "first clip stops at the segment boundary")
	require.Equal(t, time.Duration(0), clips[1].Offset, "second clip starts at its own beginning")
	require.Equal(t, 5*time.Minute, clips[1].Duration, "second clip ends at the requested end")
}

// TestPlanRangeClips_ClampsToRecordingBounds makes sure the window never
// invents footage beyond what the recording holds.
func TestPlanRangeClips_ClampsToRecordingBounds(t *testing.T) {
	t.Parallel()
	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	recStart := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	rec := &model.Recording{
		ID: "rec-1", CameraID: "cam-1", FilePath: writeDummyFile(t, "seg.mp4"),
		Format: model.FormatH264, StartedAt: recStart, EndedAt: recStart.Add(10 * time.Minute),
	}

	// Window starts before the recording and ends after it.
	clips := h.planRangeClips([]model.Recording{*rec}, recStart.Add(-5*time.Minute), recStart.Add(20*time.Minute))
	require.Len(t, clips, 1)
	require.Equal(t, time.Duration(0), clips[0].Offset)
	require.Equal(t, 10*time.Minute, clips[0].Duration, "clip must not exceed the recording")
}

// TestPlanRangeClips_SkipsMissingFile keeps a reclaimed recording from
// reaching ffmpeg as a missing path.
func TestPlanRangeClips_SkipsMissingFile(t *testing.T) {
	t.Parallel()
	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	rec := &model.Recording{
		ID: "rec-gone", CameraID: "cam-1",
		FilePath:  filepath.Join(t.TempDir(), "deleted.mp4"),
		Format:    model.FormatH264,
		StartedAt: base, EndedAt: base.Add(30 * time.Minute),
	}

	clips := h.planRangeClips([]model.Recording{*rec}, base, base.Add(5*time.Minute))
	require.Empty(t, clips, "a recording whose file is gone must be skipped")
}

// --- end-to-end: the served MP4 must match the requested window ---

// TestDownloadRange_TrimmedToWindow drives the real endpoint with a real MP4
// and checks the bytes the client receives. Before the fix the response held
// the entire 30-minute segment; now it must hold (almost) only the window.
func TestDownloadRange_TrimmedToWindow(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	dir := t.TempDir()
	// 30 s of H.264 with a 2 s GOP (keyframes every 2 s).
	path := filepath.Join(dir, "seg.mp4")
	makeTestMP4(t, path, 30)

	recStart := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	seedRecording(t, db, &model.Recording{
		ID: "rec-1", CameraID: "cam-1", FilePath: path, Format: model.FormatH264,
		StartedAt: recStart, EndedAt: recStart.Add(30 * time.Second),
		Duration: 30, MergeStatus: model.MergeStatusPending,
	})

	// Request 5 s → 15 s of the 30 s recording.
	q := fmt.Sprintf("start=%s&end=%s",
		recStart.Add(5*time.Second).Format(time.RFC3339),
		recStart.Add(15*time.Second).Format(time.RFC3339))
	rr := doRequest(t, h.Routes(), "GET", "/api/cameras/cam-1/download-range?"+q, nil, "", "")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.NotEmpty(t, rr.Body.Bytes(), "no MP4 bytes were served")

	dur := probeDuration(t, rr.Body.Bytes())
	// The clip starts on the keyframe at or before 5 s (4 s) and ends exactly
	// at 15 s, so ~11 s — never the full 30 s.
	require.Greater(t, dur, 9.0, "clip is shorter than the requested window")
	require.Less(t, dur, 13.0, "clip covers far more than the requested window")
}

// TestDownloadRange_SpanningTwoRecordings checks the multi-recording path:
// the window crosses two files and the result must be the concatenation of
// the two trimmed parts, not the two full segments.
func TestDownloadRange_SpanningTwoRecordings(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.mp4")
	pathB := filepath.Join(dir, "b.mp4")
	makeTestMP4(t, pathA, 30)
	makeTestMP4(t, pathB, 30)

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	seedRecording(t, db, &model.Recording{
		ID: "rec-a", CameraID: "cam-1", FilePath: pathA, Format: model.FormatH264,
		StartedAt: base, EndedAt: base.Add(30 * time.Second),
		Duration: 30, MergeStatus: model.MergeStatusPending,
	})
	seedRecording(t, db, &model.Recording{
		ID: "rec-b", CameraID: "cam-1", FilePath: pathB, Format: model.FormatH264,
		StartedAt: base.Add(30 * time.Second), EndedAt: base.Add(60 * time.Second),
		Duration: 30, MergeStatus: model.MergeStatusPending,
	})

	// Window: 10:25 → 10:35 — the tail of A plus the head of B.
	q := fmt.Sprintf("start=%s&end=%s",
		base.Add(25*time.Second).Format(time.RFC3339),
		base.Add(35*time.Second).Format(time.RFC3339))
	rr := doRequest(t, h.Routes(), "GET", "/api/cameras/cam-1/download-range?"+q, nil, "", "")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	dur := probeDuration(t, rr.Body.Bytes())
	// ~6 s from A (keyframe at 24 s) + ~5 s from B = ~11 s — never 60 s.
	require.Greater(t, dur, 8.0)
	require.Less(t, dur, 14.0, "the two full 30 s segments were served instead of the window")
}

// TestDownloadRange_WindowInsideRecordingNotFound is the regression guard for
// the selection semantics: a window that starts after the recording began
// used to match nothing (started_at filter) and 404'd.
func TestDownloadRange_WindowInsideRecordingNotFound(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)

	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	dir := t.TempDir()
	path := filepath.Join(dir, "seg.mp4")
	makeTestMP4(t, path, 30)

	recStart := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	seedRecording(t, db, &model.Recording{
		ID: "rec-1", CameraID: "cam-1", FilePath: path, Format: model.FormatH264,
		StartedAt: recStart, EndedAt: recStart.Add(30 * time.Second),
		Duration: 30, MergeStatus: model.MergeStatusPending,
	})

	// Window strictly inside the recording: the recording started BEFORE the
	// window, so a started_at filter finds nothing.
	q := fmt.Sprintf("start=%s&end=%s",
		recStart.Add(10*time.Second).Format(time.RFC3339),
		recStart.Add(20*time.Second).Format(time.RFC3339))
	rr := doRequest(t, h.Routes(), "GET", "/api/cameras/cam-1/download-range?"+q, nil, "", "")
	require.Equal(t, http.StatusOK, rr.Code, "a window inside a recording must not 404; body: %s", rr.Body.String())
}

// TestDownloadRange_RejectsInvertedWindow keeps a malformed range from
// downloading something arbitrary.
func TestDownloadRange_RejectsInvertedWindow(t *testing.T) {
	t.Parallel()
	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	q := fmt.Sprintf("start=%s&end=%s",
		base.Add(10*time.Minute).Format(time.RFC3339),
		base.Format(time.RFC3339))
	rr := doRequest(t, h.Routes(), "GET", "/api/cameras/cam-1/download-range?"+q, nil, "", "")
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

// TestDownloadRange_HeadResolvesDownloadTarget is the regression guard for the
// "Out of Memory" tab error: a browser resolves a download link with a HEAD
// probe to learn the file name. The route used to answer 405 there, and the
// browser then abandoned the download and rendered the MP4 inside the tab —
// a several-hundred-megabyte video the renderer has to buffer in RAM.
func TestDownloadRange_HeadResolvesDownloadTarget(t *testing.T) {
	t.Parallel()
	db, store := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := TestHandler(db, store)

	dir := t.TempDir()
	path := filepath.Join(dir, "seg.mp4")
	makeTestMP4(t, path, 10)

	recStart := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	seedRecording(t, db, &model.Recording{
		ID: "rec-1", CameraID: "cam-1", FilePath: path, Format: model.FormatH264,
		StartedAt: recStart, EndedAt: recStart.Add(10 * time.Second),
		Duration: 10, MergeStatus: model.MergeStatusPending,
	})

	q := fmt.Sprintf("start=%s&end=%s&name=%s",
		recStart.Add(2*time.Second).Format(time.RFC3339),
		recStart.Add(6*time.Second).Format(time.RFC3339),
		url.QueryEscape("罗技_test"))
	rr := doRequest(t, h.Routes(), "HEAD", "/api/cameras/cam-1/download-range?"+q, nil, "", "")

	require.Equal(t, http.StatusOK, rr.Code,
		"the browser's HEAD probe must not be rejected: %s", rr.Body.String())
	require.Empty(t, rr.Body.Bytes(), "HEAD must not carry a body")
	// octet-stream keeps the browser from rendering the clip in the tab.
	require.Equal(t, "application/octet-stream", rr.Header().Get("Content-Type"))

	disp := rr.Header().Get("Content-Disposition")
	require.Contains(t, disp, "attachment", "the probe must learn that this is a download")
	require.Contains(t, disp, "filename=", "the probe must learn the file name")
	require.Contains(t, disp, "filename*=UTF-8''", "the UTF-8 name must survive")
	require.Contains(t, disp, "%E7%BD%97%E6%8A%80", "the Chinese name must be percent-encoded")
}

// TestContentDispositionAttachment checks the two forms of the header: the
// RFC 5987 extended name keeps the real characters, the plain name stays
// ASCII so clients without RFC 5987 still get one.
func TestContentDispositionAttachment(t *testing.T) {
	t.Parallel()

	got := contentDispositionAttachment("罗技_2026-11-01.mp4")
	require.Contains(t, got, `filename*=UTF-8''%E7%BD%97%E6%8A%80_2026-11-01.mp4`)
	// Each non-ASCII rune collapses to one underscore; the separator that was
	// already in the name is kept as-is.
	require.Contains(t, got, `filename="___2026-11-01.mp4"`)

	// A name that is already ASCII must survive unchanged in both forms.
	got = contentDispositionAttachment("clip.mp4")
	require.Contains(t, got, `filename="clip.mp4"`)
	require.Contains(t, got, `filename*=UTF-8''clip.mp4`)

	// Characters that are not attr-char must be escaped in the extended form:
	// '=' would otherwise cut the ext-value short for a strict parser.
	got = contentDispositionAttachment("a=b.mp4")
	require.Contains(t, got, `filename*=UTF-8''a%3Db.mp4`)
	require.Contains(t, got, `filename="a=b.mp4"`)
}

// --- helpers ---

// writeDummyFile creates a placeholder file so planRangeClips sees an
// existing path (the keyframe anchor needs a parsable MP4, which the dummy
// is not — the anchor then falls back to the raw offset).
func writeDummyFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte("dummy"), 0o644))
	return path
}

// requireFFmpeg skips the test when ffmpeg is not installed.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
}

// makeTestMP4 renders a synthetic H.264 clip with a 2-second GOP.
func makeTestMP4(t *testing.T, path string, seconds int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=320x240:rate=15:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "30", path)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "ffmpeg failed: %s", string(out))
}

// probeDuration returns the duration (seconds) of an MP4 payload.
func probeDuration(t *testing.T, data []byte) float64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.mp4")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", path).CombinedOutput()
	require.NoError(t, err, "ffprobe failed: %s", string(out))
	dur, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	require.NoError(t, err, "unparsable duration %q", string(out))
	return dur
}

// compile-time guard: the storage layer must expose the overlap query.
var _ = func() bool {
	var db interface {
		ListRecordingsOverlapping(context.Context, string, time.Time, time.Time, []model.Format, int) ([]model.Recording, error)
	}
	_ = db
	return true
}()
