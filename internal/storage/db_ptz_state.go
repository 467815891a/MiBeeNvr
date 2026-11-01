package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PTZ position kinds — the coordinate space a stored position lives in.
// The kind travels with the row because the two protocols report positions
// in incompatible spaces: UVC cameras expose raw device units (pan/tilt in
// the driver's own range, zoom in the device's zoom units) while ONVIF
// reports the normalized [-1,1] space of the PTZ vectors.
const (
	// PTZKindUVCAbsolute stores raw UVC device units.
	PTZKindUVCAbsolute = "uvc_absolute"
	// PTZKindONVIFNormalized stores normalized ONVIF PTZ vector values.
	PTZKindONVIFNormalized = "onvif_normalized"
)

// PTZState is the last known PTZ position of a camera — the NVR's own memory
// of where the camera was left.
//
// WHY THE NVR KEEPS IT: a camera's own position memory is not reliable. UVC
// devices reset their camera controls when the capture device is re-opened,
// and most ONVIF PTZ cameras park at their home position when the last PTZ
// client disconnects. The NVR is the only client that knows where the
// operator left the camera, so it stores the position and re-applies it when
// the camera starts again.
type PTZState struct {
	CameraID string
	// Kind is the coordinate space of Pan/Tilt/Zoom (see the PTZKind* consts).
	Kind      string
	Pan       float64
	Tilt      float64
	Zoom      float64
	UpdatedAt time.Time
}

// SavePTZState records a camera's last known PTZ position (upsert — one row
// per camera).
func (d *DB) SavePTZState(ctx context.Context, s PTZState) error {
	if s.CameraID == "" {
		return fmt.Errorf("save ptz state: camera id is required")
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now()
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO camera_ptz_state
		(camera_id, kind, pan, tilt, zoom, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(camera_id) DO UPDATE SET
			kind=excluded.kind, pan=excluded.pan, tilt=excluded.tilt,
			zoom=excluded.zoom, updated_at=excluded.updated_at;`,
		s.CameraID, s.Kind, s.Pan, s.Tilt, s.Zoom, timeToDB(s.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save ptz state: %w", err)
	}
	return nil
}

// LoadPTZState returns a camera's last known PTZ position. ok is false when
// the camera has no stored position (never moved, or the row was deleted).
func (d *DB) LoadPTZState(ctx context.Context, cameraID string) (PTZState, bool, error) {
	var s PTZState
	var updatedAt sql.NullString
	err := d.readConn().QueryRowContext(ctx,
		`SELECT camera_id, kind, pan, tilt, zoom, updated_at FROM camera_ptz_state WHERE camera_id=?;`,
		cameraID).Scan(&s.CameraID, &s.Kind, &s.Pan, &s.Tilt, &s.Zoom, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return PTZState{}, false, nil
		}
		return PTZState{}, false, fmt.Errorf("load ptz state: %w", err)
	}
	s.UpdatedAt = scanTime(updatedAt)
	return s, true, nil
}

// DeletePTZState forgets a camera's stored PTZ position (camera removed).
func (d *DB) DeletePTZState(ctx context.Context, cameraID string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM camera_ptz_state WHERE camera_id=?;`, cameraID); err != nil {
		return fmt.Errorf("delete ptz state: %w", err)
	}
	return nil
}
