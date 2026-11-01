package camera

import (
	"context"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/config"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/model"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/onvif"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/storage"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/uvc"
)

// PTZ position restore.
//
// A camera's own position memory is not dependable: a UVC device resets its
// camera controls when the capture device is re-opened, and most ONVIF PTZ
// cameras park at their home position once the last PTZ client disconnects.
// The NVR is the only client that knows where the operator left the camera,
// so it stores that position (internal/api/ptz_state.go) and re-applies it
// every time the camera starts again — the NVR boot, a manual restart or a
// health auto-remediation.
//
// The restore is fire-and-forget: it must never delay the camera start, and a
// camera that cannot be moved (no PTZ, device unplugged) must not turn into a
// startup error.

var (
	// ptzRestoreDelay is the pause before the first attempt: the capture
	// process opens the device a moment after the recorder starts, and
	// grabbing the device first would race it.
	ptzRestoreDelay = 3 * time.Second
	// ptzRestoreAttempts / ptzRestoreRetryDelay bound the retry loop — a
	// camera that is still enumerating cannot take the move yet.
	ptzRestoreAttempts   = 3
	ptzRestoreRetryDelay = 6 * time.Second
	// ptzRestoreTimeout bounds the whole restore (delay + attempts).
	ptzRestoreTimeout = 60 * time.Second
)

// ptzRestoreWait is the pause before an attempt: the first one waits for the
// capture process to grab the device, the later ones just space the retries
// out.
func ptzRestoreWait(attempt int) time.Duration {
	if attempt <= 1 {
		return ptzRestoreDelay
	}
	return ptzRestoreRetryDelay
}

// storePTZState records a camera's PTZ position in the position memory
// (the row the restore reads back).
func (cm *CameraManager) storePTZState(ctx context.Context, cameraID, kind string, pan, tilt, zoom float64) {
	if cm == nil || cm.db == nil {
		return
	}
	err := cm.db.SavePTZState(ctx, storage.PTZState{
		CameraID:  cameraID,
		Kind:      kind,
		Pan:       pan,
		Tilt:      tilt,
		Zoom:      zoom,
		UpdatedAt: time.Now(),
	})
	if err != nil {
		logger.Warn("failed to store the camera's PTZ position", "camera_id", cameraID, "error", err)
	}
}

// restorePTZPosition re-applies the camera's last known PTZ position after
// its recorder started. Only the protocols whose position the NVR owns are
// restored: UVC (device units) and ONVIF (normalized vectors). GB28181
// cameras are driven by the upper platform and Xiaomi cameras only support
// relative motor steps — neither has a position to restore.
func (cm *CameraManager) restorePTZPosition(cam config.CameraConfig) {
	if cm == nil || cm.db == nil {
		return
	}
	switch cam.Protocol {
	case string(model.ProtoUVC):
		go cm.restoreUVCPTZPosition(cam)
	case string(model.ProtoONVIF):
		go cm.restoreONVIFPTZPosition(cam)
	}
}

// restoreUVCPTZPosition seeks a UVC camera back to its stored absolute
// pan/tilt/zoom.
func (cm *CameraManager) restoreUVCPTZPosition(cam config.CameraConfig) {
	if cam.UVC == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ptzRestoreTimeout)
	defer cancel()

	st, ok, err := cm.db.LoadPTZState(ctx, cam.ID)
	if err != nil {
		logger.Warn("failed to read the camera's stored PTZ position", "camera_id", cam.ID, "error", err)
		return
	}
	if !ok || st.Kind != storage.PTZKindUVCAbsolute {
		return // the camera was never moved — nothing to restore
	}

	pan, tilt, zoom := int(st.Pan), int(st.Tilt), int(st.Zoom)
	dev := uvc.Device{Name: cam.UVC.Device, Path: cam.UVC.DevicePath}
	var lastErr error
	for attempt := 1; attempt <= ptzRestoreAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(ptzRestoreWait(attempt)):
		}
		// A fresh controller per attempt: the helper process dies with it, so
		// a crashed attempt cannot poison the next one.
		ctrl := uvc.NewUVCController(dev)
		err := ctrl.MoveAbsolute(&pan, &tilt, &zoom)
		ctrl.Stop()
		if err == nil {
			logger.Info("restored the camera's last PTZ position",
				"camera_id", cam.ID, "pan", pan, "tilt", tilt, "zoom", zoom)
			return
		}
		lastErr = err
		logger.Debug("PTZ position restore attempt failed",
			"camera_id", cam.ID, "attempt", attempt, "error", err)
	}
	logger.Warn("failed to restore the camera's last PTZ position",
		"camera_id", cam.ID, "error", lastErr)
}

// restoreONVIFPTZPosition seeks an ONVIF camera back to its stored
// normalized position.
func (cm *CameraManager) restoreONVIFPTZPosition(cam config.CameraConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), ptzRestoreTimeout)
	defer cancel()

	st, ok, err := cm.db.LoadPTZState(ctx, cam.ID)
	if err != nil {
		logger.Warn("failed to read the camera's stored PTZ position", "camera_id", cam.ID, "error", err)
		return
	}
	if !ok || st.Kind != storage.PTZKindONVIFNormalized {
		return // the camera was never moved — nothing to restore
	}

	vec := onvif.PTZVector{Pan: st.Pan, Tilt: st.Tilt, Zoom: st.Zoom}
	var lastErr error
	for attempt := 1; attempt <= ptzRestoreAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(ptzRestoreWait(attempt)):
		}
		ptz, err := cm.GetONVIFPTZController(ctx, cam.ID)
		if err != nil {
			lastErr = err
			continue
		}
		if err := ptz.AbsoluteMove(ctx, vec); err != nil {
			lastErr = err
			continue
		}
		logger.Info("restored the camera's last PTZ position",
			"camera_id", cam.ID, "pan", st.Pan, "tilt", st.Tilt, "zoom", st.Zoom)
		return
	}
	logger.Warn("failed to restore the camera's last PTZ position",
		"camera_id", cam.ID, "error", lastErr)
}
