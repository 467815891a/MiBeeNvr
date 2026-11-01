package api

// Camera burn-in watermark coverage: the per-camera `watermark_enabled` flag
// must round-trip through the update API — into the in-memory config, into
// the YAML on disk, and back out through the GET response the camera edit
// form reads.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCameraUpdateWatermark(t *testing.T) {
	t.Parallel()
	h, cm, cfgPath := newCameraAPIEnv(t)

	require.Equal(t, http.StatusOK,
		camDo(t, h, http.MethodPut, "/api/cameras/ingest-cam", `{"watermark_enabled":true}`).Code)

	cam := cm.GetCameraConfig("ingest-cam")
	require.NotNil(t, cam)
	require.True(t, cam.WatermarkEnabled, "watermark_enabled must reach the in-memory config")

	disk, err := config.Load(cfgPath)
	require.NoError(t, err)
	persisted := false
	for i := range disk.Cameras {
		if disk.Cameras[i].ID == "ingest-cam" {
			persisted = disk.Cameras[i].WatermarkEnabled
		}
	}
	require.True(t, persisted, "watermark_enabled must survive the YAML persist")

	// The GET response carries it too — the edit form reads the flag back to
	// prefill the checkbox.
	w := camDo(t, h, http.MethodGet, "/api/cameras/ingest-cam", "")
	require.Equal(t, http.StatusOK, w.Code)
	var row struct {
		WatermarkEnabled bool `json:"watermark_enabled"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &row))
	require.True(t, row.WatermarkEnabled)

	// Turning it off again must clear it (the flag is a plain boolean, not a
	// tri-state).
	require.Equal(t, http.StatusOK,
		camDo(t, h, http.MethodPut, "/api/cameras/ingest-cam", `{"watermark_enabled":false}`).Code)
	cam = cm.GetCameraConfig("ingest-cam")
	require.NotNil(t, cam)
	require.False(t, cam.WatermarkEnabled, "watermark_enabled must be clearable")
}
