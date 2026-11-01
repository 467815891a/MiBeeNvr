package recorder

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/livetranscode"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/mediaprobe"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/storage"
	"github.com/stretchr/testify/require"
)

// annexBStream builds an Annex-B byte stream from the given NAL units.
func annexBStream(nalus ...[]byte) []byte {
	var out []byte
	for _, nalu := range nalus {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, nalu...)
	}
	return out
}

// recordedMP4s walks the camera's recording tree and returns the finalized
// segment files (the store nests them under YYYY/MM/DD/HH).
func recordedMP4s(t *testing.T, store *storage.Manager, cameraID string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(store.RootDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".mp4") {
			out = append(out, path)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestIngestRecorder_PreservesEmulationPreventionBytes drives the UVC ingest
// path end to end — Annex-B bytes → AnnexBStreamParser → AU assembler → MP4 —
// and asserts the recorded sample carries the slice payload byte for byte.
//
// The payload contains an escaped 0x00 0x00 sequence (0x00 0x00 0x03). The
// Annex-B splitter used to strip the emulation prevention byte, which rewrote
// the bitstream: the decoder un-escapes what it finds and loses a real byte,
// so the slice decodes into garbage — "half the picture is corrupted" in the
// downloaded video. MP4/AVCC stores the ESCAPED form, so the bytes must
// survive the split untouched.
func TestIngestRecorder_PreservesEmulationPreventionBytes(t *testing.T) {
	rec, store, _ := newIngestRecorder(t, 10*time.Minute)
	t.Cleanup(func() { _ = rec.Stop() })

	// Parameter sets with an escaped sequence in the SPS, an IDR and a P slice
	// whose payload carries 0x00 0x00 0x03.
	sps := []byte{0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	idr := []byte{0x65, 0x88, 0x84, 0x00, 0x00, 0x03, 0x01}
	pSlice := []byte{0x41, 0x9a, 0x00, 0x00, 0x03, 0x02, 0x77}

	parser := livetranscode.NewAnnexBStreamParser(livetranscode.CodecH264)
	pts := int64(0)
	for _, au := range parser.Feed(annexBStream(sps, pps, idr, pSlice)) {
		rec.WriteNALU(au, pts, false)
		pts += 3000
	}
	for _, au := range parser.Flush() {
		rec.WriteNALU(au, pts, false)
	}
	require.NoError(t, rec.Stop())

	files := recordedMP4s(t, store, "push-cam")
	require.Len(t, files, 1, "exactly one segment must have been recorded")

	info, err := mediaprobe.ParseSegment(files[0])
	require.NoError(t, err)
	require.Equal(t, 2, info.SampleCount, "IDR + P slice must be two samples")

	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)

	// Each video sample is a 4-byte length prefix followed by the NAL unit
	// exactly as it arrived.
	for i, want := range [][]byte{idr, pSlice} {
		s := info.Samples[i]
		require.Greater(t, s.Size, uint32(4))
		got := raw[s.Offset : s.Offset+int64(s.Size)]
		require.Equal(t, uint32(len(want)), uint32(len(got)-4), "sample %d payload length", i)
		require.Equal(t, want, got[4:], "sample %d payload bytes must be byte-identical", i)
	}
}
