package merge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMergeMP4Segments_MixedTimescalesShareOneClock guards the time-base
// mismatch that appears once recordings from before the 90 kHz switch
// (timescale 1000) are merged with recordings made after it (timescale
// 90000). The merged track runs on ONE clock, so every input's durations must
// be converted into it — otherwise the newer segment's frames render 90x too
// long (3 s per frame) in a file that claims a millisecond time base.
func TestMergeMP4Segments_MixedTimescalesShareOneClock(t *testing.T) {
	dir := t.TempDir()
	sps := h264SPS1920Fixture
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	idrNAL := []byte{0x65, 0x88, 0x80, 0x40}
	pNAL := []byte{0x41, 0x10, 0x00, 0x0c}

	legacyPath := createH264SegmentWithSamples(t, dir, "legacy.mp4", sps, pps, [][]byte{idrNAL, pNAL})
	newPath := createH264SegmentWithSamples(t, dir, "new.mp4", sps, pps, [][]byte{idrNAL, pNAL, pNAL})

	legacyInfo, err := ParseSegment(legacyPath)
	require.NoError(t, err)
	newInfo, err := ParseSegment(newPath)
	require.NoError(t, err)
	require.Equal(t, uint32(mergeVideoTimescale), newInfo.Timescale)

	// Re-express the first segment in the legacy millisecond time base — the
	// way a recording made before the 90 kHz switch carries its durations.
	legacy := *legacyInfo
	legacy.Timescale = 1000
	legacy.Samples = append([]SampleEntry(nil), legacyInfo.Samples...)
	for i := range legacy.Samples {
		legacy.Samples[i].Duration = scaleTicks(legacy.Samples[i].Duration, legacyInfo.Timescale, 1000)
	}

	out := filepath.Join(dir, "merged.mp4")
	_, err = MergeMP4Segments(context.Background(), []*SegmentInfo{&legacy, newInfo}, out)
	require.NoError(t, err)

	merged, err := ParseSegment(out)
	require.NoError(t, err)
	require.Equal(t, uint32(mergeVideoTimescale), merged.Timescale)
	require.Equal(t, 5, merged.SampleCount)

	// Every frame lands on the same ~33 ms grid. Before the fix the second
	// segment's durations (2970 ticks at 90 kHz) were read as milliseconds.
	for i, s := range merged.Samples {
		frameDur := time.Duration(s.Duration) * time.Second / time.Duration(merged.Timescale)
		require.InDelta(t, float64(33*time.Millisecond), float64(frameDur), float64(time.Millisecond),
			"sample %d duration drifted", i)
	}
	require.InDelta(t, float64(165*time.Millisecond), float64(merged.TotalDuration), float64(time.Millisecond))
	require.NoError(t, ValidateMergedMP4(out))
}

// TestMergeMP4Segments_UniformTimescaleUnchanged pins the no-op case: inputs
// that already share the canonical clock must come out untouched.
func TestMergeMP4Segments_UniformTimescaleUnchanged(t *testing.T) {
	dir := t.TempDir()
	sps := h264SPS1920Fixture
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	idrNAL := []byte{0x65, 0x88, 0x80, 0x40}
	pNAL := []byte{0x41, 0x10, 0x00, 0x0c}

	seg1 := createH264SegmentWithSamples(t, dir, "seg1.mp4", sps, pps, [][]byte{idrNAL, pNAL})
	seg2 := createH264SegmentWithSamples(t, dir, "seg2.mp4", sps, pps, [][]byte{idrNAL, pNAL, pNAL})

	info1, err := ParseSegment(seg1)
	require.NoError(t, err)
	info2, err := ParseSegment(seg2)
	require.NoError(t, err)

	out := filepath.Join(dir, "merged.mp4")
	_, err = MergeMP4Segments(context.Background(), []*SegmentInfo{info1, info2}, out)
	require.NoError(t, err)

	merged, err := ParseSegment(out)
	require.NoError(t, err)
	require.Equal(t, uint32(mergeVideoTimescale), merged.Timescale)
	require.Equal(t, 5, merged.SampleCount)
	require.InDelta(t, float64(165*time.Millisecond), float64(merged.TotalDuration), float64(time.Millisecond))
}

func TestScaleTicks(t *testing.T) {
	// 33 ms at 90 kHz → 2970 ticks; back to 1/1000 → 33 ticks.
	require.Equal(t, uint32(2970), scaleTicks(33, 1000, 90000))
	require.Equal(t, uint32(33), scaleTicks(2970, 90000, 1000))
	// Identity.
	require.Equal(t, uint32(3000), scaleTicks(3000, 90000, 90000))
	// Zero stays zero (no phantom duration).
	require.Equal(t, uint32(0), scaleTicks(0, 1000, 90000))
	// A non-zero duration never collapses to zero — a zero delta makes the
	// demuxer emit a non-increasing DTS.
	require.Equal(t, uint32(1), scaleTicks(1, 90000, 1000))
	// Rounds to nearest, not down.
	require.Equal(t, uint32(2970), scaleTicks(33, 1000, 90000))
	require.Equal(t, uint32(33), scaleTicks(2970, 90000, 1000))
}
