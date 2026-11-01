package api

import (
	"net/http"
	"testing"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/storage"
	"github.com/stretchr/testify/require"
)

// The NVR remembers where the operator left the camera so it can put the
// camera back after a restart (see internal/camera/ptz_restore.go).

func TestPTZMove_ONVIFStoresPosition(t *testing.T) {
	t.Parallel()
	h, mock := newONVIFPTZTestHandler(t)

	w := postPTZ(t, h, "/api/cameras/onvif-cam/ptz/move", `{"mode":"continuous","pan":0.25,"tilt":0,"zoom":0}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	st, ok, err := h.db.LoadPTZState(t.Context(), "onvif-cam")
	require.NoError(t, err)
	require.True(t, ok, "the PTZ move must be remembered")
	require.Equal(t, storage.PTZKindONVIFNormalized, st.Kind)
	require.InDelta(t, mock.Position.Pan, st.Pan, 1e-9)
	require.InDelta(t, mock.Position.Tilt, st.Tilt, 1e-9)
	require.InDelta(t, mock.Position.Zoom, st.Zoom, 1e-9)
}

func TestPTZStop_ONVIFStoresPosition(t *testing.T) {
	t.Parallel()
	h, mock := newONVIFPTZTestHandler(t)

	w := postPTZ(t, h, "/api/cameras/onvif-cam/ptz/stop", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	st, ok, err := h.db.LoadPTZState(t.Context(), "onvif-cam")
	require.NoError(t, err)
	require.True(t, ok, "the position a continuous move ended at must be remembered")
	require.Equal(t, storage.PTZKindONVIFNormalized, st.Kind)
	require.InDelta(t, mock.Position.Pan, st.Pan, 1e-9)
}
