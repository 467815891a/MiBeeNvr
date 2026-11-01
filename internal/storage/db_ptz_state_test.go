package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPTZState_SaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	// Nothing stored yet.
	_, ok, err := db.LoadPTZState(ctx, "cam-1")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, db.SavePTZState(ctx, PTZState{
		CameraID: "cam-1",
		Kind:     PTZKindUVCAbsolute,
		Pan:      -22,
		Tilt:     -8,
		Zoom:     100,
	}))

	st, ok, err := db.LoadPTZState(ctx, "cam-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "cam-1", st.CameraID)
	require.Equal(t, PTZKindUVCAbsolute, st.Kind)
	require.InDelta(t, -22, st.Pan, 1e-9)
	require.InDelta(t, -8, st.Tilt, 1e-9)
	require.InDelta(t, 100, st.Zoom, 1e-9)
	require.False(t, st.UpdatedAt.IsZero())

	// A second save overwrites the row (one position per camera).
	require.NoError(t, db.SavePTZState(ctx, PTZState{
		CameraID: "cam-1",
		Kind:     PTZKindONVIFNormalized,
		Pan:      0.25,
		Tilt:     -0.5,
		Zoom:     0.1,
		UpdatedAt: time.Now(),
	}))

	st, ok, err = db.LoadPTZState(ctx, "cam-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, PTZKindONVIFNormalized, st.Kind)
	require.InDelta(t, 0.25, st.Pan, 1e-9)
	require.InDelta(t, -0.5, st.Tilt, 1e-9)
	require.InDelta(t, 0.1, st.Zoom, 1e-9)
}

func TestPTZState_Delete(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.SavePTZState(ctx, PTZState{CameraID: "cam-1", Kind: PTZKindUVCAbsolute}))
	require.NoError(t, db.DeletePTZState(ctx, "cam-1"))

	_, ok, err := db.LoadPTZState(ctx, "cam-1")
	require.NoError(t, err)
	require.False(t, ok)

	// Deleting an unknown camera is a no-op, not an error.
	require.NoError(t, db.DeletePTZState(ctx, "cam-unknown"))
}

func TestPTZState_RequiresCameraID(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	require.Error(t, db.SavePTZState(context.Background(), PTZState{Kind: PTZKindUVCAbsolute}))
}
