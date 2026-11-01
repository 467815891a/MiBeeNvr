package api

import (
	"context"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/onvif"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/storage"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/uvc"
)

// PTZ position memory.
//
// The NVR is the only client that knows where the operator left the camera,
// so it stores the position of every camera it moves. The stored position is
// what the camera manager re-applies when a camera starts again (see
// internal/camera/ptz_restore.go) — without it a camera that forgets its own
// position (UVC controls reset when the device is re-opened, most ONVIF PTZ
// cameras park at home once the client disconnects) would come back pointing
// somewhere else after every NVR restart.
//
// The positions are stored in the protocol's own coordinate space, tagged by
// the kind column: UVC reports raw device units, ONVIF the normalized
// [-1,1] vectors.

// savePTZState stores a camera's PTZ position. Best-effort: a failed write
// only costs the restore, never the PTZ command itself.
func (h *Handler) savePTZState(ctx context.Context, cameraID, kind string, pan, tilt, zoom float64) {
	if h.db == nil {
		return
	}
	err := h.db.SavePTZState(ctx, storage.PTZState{
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

// saveUVCPTZState reads the camera's absolute position from the device and
// stores it.
func (h *Handler) saveUVCPTZState(ctx context.Context, cameraID string, ctrl *uvc.UVCController) {
	if ctrl == nil {
		return
	}
	pos, err := ctrl.GetPosition()
	if err != nil {
		logger.Debug("read the UVC PTZ position for the position memory failed",
			"camera_id", cameraID, "error", err)
		return
	}
	h.savePTZState(ctx, cameraID, storage.PTZKindUVCAbsolute,
		float64(pos.Pan), float64(pos.Tilt), float64(pos.Zoom))
}

// saveONVIFPTZState reads the camera's normalized position and stores it.
func (h *Handler) saveONVIFPTZState(ctx context.Context, cameraID string, ptz onvif.PTZController) {
	if ptz == nil {
		return
	}
	pos, _, err := ptz.GetStatus(ctx)
	if err != nil {
		logger.Debug("read the ONVIF PTZ position for the position memory failed",
			"camera_id", cameraID, "error", err)
		return
	}
	h.savePTZState(ctx, cameraID, storage.PTZKindONVIFNormalized, pos.Pan, pos.Tilt, pos.Zoom)
}
