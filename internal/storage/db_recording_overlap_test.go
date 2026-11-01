package storage

import (
	"context"
	"testing"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/model"
	"github.com/stretchr/testify/require"
)

// insertRangeRecording seeds one recording covering [start, end).
func insertRangeRecording(t *testing.T, db *DB, id, cameraID string, start, end time.Time, format model.Format) {
	t.Helper()
	rec := &model.Recording{
		ID:          id,
		CameraID:    cameraID,
		FilePath:    "/tmp/" + id + ".mp4",
		Format:      format,
		StartedAt:   start,
		EndedAt:     end,
		Duration:    end.Sub(start).Seconds(),
		FileSize:    1024,
		MergeStatus: model.MergeStatusPending,
	}
	require.NoError(t, db.InsertRecording(context.Background(), rec))
}

// TestListRecordingsOverlapping_FindsWindowInsideRecording is the core
// regression guard: ListRecordings' time filter matches on started_at, so a
// window that sits INSIDE a longer recording matches nothing there. The
// overlap query must find it.
func TestListRecordingsOverlapping_FindsWindowInsideRecording(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	insertRangeRecording(t, db, "rec-30min", "cam-1", base, base.Add(30*time.Minute), model.FormatH264)

	// Window strictly inside the recording.
	got, err := db.ListRecordingsOverlapping(ctx, "cam-1",
		base.Add(5*time.Minute), base.Add(10*time.Minute),
		[]model.Format{model.FormatH264, model.FormatH265}, 100)
	require.NoError(t, err)
	require.Len(t, got, 1, "the recording overlapping the window must be found")
	require.Equal(t, "rec-30min", got[0].ID)

	// The started_at filter misses the same window — that is the bug.
	byStart, err := db.ListRecordings(ctx, model.RecordingFilter{
		CameraID:  "cam-1",
		StartTime: base.Add(5 * time.Minute),
		EndTime:   base.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	require.Empty(t, byStart, "started_at filtering is expected to miss an inner window")
}

// TestListRecordingsOverlapping_SpansBoundary checks that a window crossing
// a segment boundary returns both segments, oldest first.
func TestListRecordingsOverlapping_SpansBoundary(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	insertRangeRecording(t, db, "rec-a", "cam-1", base, base.Add(30*time.Minute), model.FormatH264)
	insertRangeRecording(t, db, "rec-b", "cam-1", base.Add(30*time.Minute), base.Add(60*time.Minute), model.FormatH264)

	got, err := db.ListRecordingsOverlapping(ctx, "cam-1",
		base.Add(25*time.Minute), base.Add(35*time.Minute),
		[]model.Format{model.FormatH264}, 100)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "rec-a", got[0].ID, "results must be ordered by start time")
	require.Equal(t, "rec-b", got[1].ID)
}

// TestListRecordingsOverlapping_ExcludesNonOverlapping keeps footage outside
// the window out of the export.
func TestListRecordingsOverlapping_ExcludesNonOverlapping(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	insertRangeRecording(t, db, "rec-before", "cam-1", base.Add(-60*time.Minute), base.Add(-30*time.Minute), model.FormatH264)
	insertRangeRecording(t, db, "rec-inside", "cam-1", base, base.Add(30*time.Minute), model.FormatH264)
	insertRangeRecording(t, db, "rec-after", "cam-1", base.Add(60*time.Minute), base.Add(90*time.Minute), model.FormatH264)
	insertRangeRecording(t, db, "rec-other-cam", "cam-2", base, base.Add(30*time.Minute), model.FormatH264)

	got, err := db.ListRecordingsOverlapping(ctx, "cam-1",
		base.Add(5*time.Minute), base.Add(10*time.Minute),
		[]model.Format{model.FormatH264}, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "rec-inside", got[0].ID)
}

// TestListRecordingsOverlapping_ExcludesOtherFormats keeps non-video
// recordings (e.g. timelapse JPEG sequences) out of an MP4 export.
func TestListRecordingsOverlapping_ExcludesOtherFormats(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	insertRangeRecording(t, db, "rec-video", "cam-1", base, base.Add(30*time.Minute), model.FormatH264)
	insertRangeRecording(t, db, "rec-tl", "cam-1", base, base.Add(30*time.Minute), model.Format("timelapse"))

	got, err := db.ListRecordingsOverlapping(ctx, "cam-1",
		base, base.Add(10*time.Minute),
		[]model.Format{model.FormatH264, model.FormatH265}, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "rec-video", got[0].ID)
}

// TestListRecordingsOverlapping_IncludesOpenRecording treats a recording
// that has not ended yet as running until the end of the window.
func TestListRecordingsOverlapping_IncludesOpenRecording(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	// No EndedAt — still recording.
	rec := &model.Recording{
		ID: "rec-live", CameraID: "cam-1", FilePath: "/tmp/rec-live.mp4",
		Format: model.FormatH264, StartedAt: base, MergeStatus: model.MergeStatusPending,
	}
	require.NoError(t, db.InsertRecording(ctx, rec))

	got, err := db.ListRecordingsOverlapping(ctx, "cam-1",
		base.Add(5*time.Minute), base.Add(10*time.Minute),
		[]model.Format{model.FormatH264}, 100)
	require.NoError(t, err)
	require.Len(t, got, 1, "a recording still in progress must be included")
}
