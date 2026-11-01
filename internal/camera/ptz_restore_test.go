package camera

import (
	"testing"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/config"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/model"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/onvif"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/storage"
	"github.com/stretchr/testify/require"
)

// The camera manager re-applies the camera's last known PTZ position when
// the camera starts again (a UVC device resets its controls when the device
// is re-opened, most ONVIF PTZ cameras park at home once the client goes
// away — see ptz_restore.go).

func TestRestoreONVIFPTZPosition_AppliesStoredPosition(t *testing.T) {
	cm, _, db, _ := newTestManager(t)

	// No delay in the test — the restore must not wait for the capture
	// process here.
	origDelay, origAttempts := ptzRestoreDelay, ptzRestoreAttempts
	ptzRestoreDelay, ptzRestoreAttempts = 0, 1
	t.Cleanup(func() { ptzRestoreDelay, ptzRestoreAttempts = origDelay, origAttempts })

	mock := &onvif.MockPTZController{Position: onvif.PTZVector{Pan: 0.1}}
	cm.SetTestONVIFPTZController("onvif-cam", mock)

	require.NoError(t, db.SavePTZState(t.Context(), storage.PTZState{
		CameraID: "onvif-cam",
		Kind:     storage.PTZKindONVIFNormalized,
		Pan:      -0.25,
		Tilt:     0.5,
		Zoom:     0.1,
	}))

	cam := config.CameraConfig{ID: "onvif-cam", Protocol: string(model.ProtoONVIF)}
	cm.restoreONVIFPTZPosition(cam)

	require.Equal(t, 1, mock.AbsoluteMoveCalls, "the stored position must be re-applied")
	require.Len(t, mock.MoveHistory, 1)
	require.InDelta(t, -0.25, mock.MoveHistory[0].Pan, 1e-9)
	require.InDelta(t, 0.5, mock.MoveHistory[0].Tilt, 1e-9)
	require.InDelta(t, 0.1, mock.MoveHistory[0].Zoom, 1e-9)
}

func TestRestoreONVIFPTZPosition_NoStoredPosition(t *testing.T) {
	cm, _, _, _ := newTestManager(t)

	origDelay, origAttempts := ptzRestoreDelay, ptzRestoreAttempts
	ptzRestoreDelay, ptzRestoreAttempts = 0, 1
	t.Cleanup(func() { ptzRestoreDelay, ptzRestoreAttempts = origDelay, origAttempts })

	mock := &onvif.MockPTZController{}
	cm.SetTestONVIFPTZController("onvif-cam", mock)

	cam := config.CameraConfig{ID: "onvif-cam", Protocol: string(model.ProtoONVIF)}
	cm.restoreONVIFPTZPosition(cam)

	require.Equal(t, 0, mock.AbsoluteMoveCalls, "a camera that was never moved must not be moved")
}

// A camera whose stored position belongs to another protocol must not be
// moved with it (the coordinate spaces are not interchangeable).
func TestRestoreONVIFPTZPosition_IgnoresForeignKind(t *testing.T) {
	cm, _, db, _ := newTestManager(t)

	origDelay, origAttempts := ptzRestoreDelay, ptzRestoreAttempts
	ptzRestoreDelay, ptzRestoreAttempts = 0, 1
	t.Cleanup(func() { ptzRestoreDelay, ptzRestoreAttempts = origDelay, origAttempts })

	mock := &onvif.MockPTZController{}
	cm.SetTestONVIFPTZController("onvif-cam", mock)

	require.NoError(t, db.SavePTZState(t.Context(), storage.PTZState{
		CameraID: "onvif-cam",
		Kind:     storage.PTZKindUVCAbsolute,
		Pan:      -22,
		Tilt:     -8,
	}))

	cam := config.CameraConfig{ID: "onvif-cam", Protocol: string(model.ProtoONVIF)}
	cm.restoreONVIFPTZPosition(cam)

	require.Equal(t, 0, mock.AbsoluteMoveCalls)
}

// Only the protocols whose position the NVR owns are restored.
func TestRestorePTZPosition_IgnoresOtherProtocols(t *testing.T) {
	cm, _, db, _ := newTestManager(t)

	require.NoError(t, db.SavePTZState(t.Context(), storage.PTZState{
		CameraID: "rtsp-cam",
		Kind:     storage.PTZKindONVIFNormalized,
		Pan:      0.5,
	}))

	// An RTSP camera has no PTZ surface at all — the restore must be a
	// no-op (and must not touch the stored row).
	cm.restorePTZPosition(config.CameraConfig{ID: "rtsp-cam", Protocol: string(model.ProtoRTSP)})
	time.Sleep(50 * time.Millisecond)

	st, ok, err := db.LoadPTZState(t.Context(), "rtsp-cam")
	require.NoError(t, err)
	require.True(t, ok)
	require.InDelta(t, 0.5, st.Pan, 1e-9)
}
