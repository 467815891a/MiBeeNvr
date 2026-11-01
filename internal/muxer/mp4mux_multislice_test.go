package muxer

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/merge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWriteSampleMulti_MultiSlicePicture locks the AVCC layout of a picture
// that spans several slices: one MP4 sample, one length prefix per NAL unit.
// Writing only the first slice drops the rest of the encoded data and the
// decoder renders the missing macroblock rows as garbage.
func TestWriteSampleMulti_MultiSlicePicture(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "multislice.mp4")

	m := NewMP4Muxer(path)
	track, err := m.AddH264Track(testSPS, testPPS)
	require.NoError(t, err)

	// An IDR picture split into two slices (continuation slice has
	// first_mb_in_slice != 0, i.e. the top bit of the byte after the NAL
	// header is clear).
	slice1 := []byte{0x65, 0x88, 0x84, 0x11, 0x22}
	slice2 := []byte{0x65, 0x1a, 0x33, 0x44}
	// A plain P picture in one piece.
	pSlice := []byte{0x41, 0x9a, 0x55}

	require.NoError(t, m.WriteSampleMulti(track, [][]byte{slice1, slice2}, 0, 100*time.Millisecond))
	require.NoError(t, m.WriteSampleMulti(track, [][]byte{pSlice}, 100*time.Millisecond, 100*time.Millisecond))
	require.NoError(t, m.Close())

	info, err := merge.ParseSegment(path)
	require.NoError(t, err)
	require.Equal(t, 2, info.SampleCount, "two pictures, not three samples")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	// Sample 0 carries both slices, each with its own length prefix.
	s0 := info.Samples[0]
	got := raw[s0.Offset : s0.Offset+int64(s0.Size)]
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(slice1)))
	require.True(t, bytes.HasPrefix(got, lenBuf[:]), "first slice must start with its length prefix")
	require.Equal(t, slice1, got[4:4+len(slice1)], "first slice bytes")
	off := 4 + len(slice1)
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(slice2)))
	require.Equal(t, lenBuf[:], got[off:off+4], "second slice length prefix")
	require.Equal(t, slice2, got[off+4:off+4+len(slice2)], "second slice bytes")

	// The IDR picture is a sync sample; the P picture is not.
	require.True(t, info.Samples[0].IsKeyFrame, "multi-slice IDR picture must be a sync sample")
	require.False(t, info.Samples[1].IsKeyFrame, "P picture must not be a sync sample")

	// Sample 1 is the plain P picture, byte-identical to its NAL unit.
	s1 := info.Samples[1]
	got1 := raw[s1.Offset : s1.Offset+int64(s1.Size)]
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pSlice)))
	require.True(t, bytes.HasPrefix(got1, lenBuf[:]))
	assert.Equal(t, pSlice, got1[4:])
}
