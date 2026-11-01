package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/model"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/storage"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/uvc"
)

// UVC (local USB camera) device discovery + PTZ endpoints.
//
// UVC cameras ride the SAME /api/cameras/{id}/ptz/* endpoints as ONVIF/GB28181
// cameras (the dispatch in handlers_onvif.go routes "uvc" protocol cameras
// into these handlers). The wire semantics differ from ONVIF: UVC camera
// controls are direct hardware values (DirectShow IAMCameraControl), so
//   - continuous move is EMULATED with a ticker of relative pan/tilt steps
//     until stop (UVC has no "move until told otherwise" verb),
//   - zoom is stepped toward tele/wide while held (relative when the device
//     supports it, clamped absolute steps otherwise),
//   - presets are stored LOCALLY (camera_ptz_presets.position, JSON) — UVC
//     devices have no preset memory — and replayed as absolute moves.
//
// All control operations go through uvc.UVCController, which runs them in the
// sacrificial `mibee-nvr uvc-ptz` helper process: vendor driver proxies have
// been observed crashing the calling process on control calls (reproduced
// independently with a .NET probe), and the helper keeps that fault away from
// the NVR itself. Device enumeration stays in-process (a stable code path).
//
// Pan/tilt direction mapping matches the ONVIF convention the frontend pad
// uses (positive pan = right, positive tilt = up), which is also the UVC
// relative-control convention.

const (
	// uvcTickInterval is the emulated-continuous-move step cadence. 100ms
	// keeps the motor response snappy; each tick is one helper round-trip.
	uvcTickInterval = 100 * time.Millisecond
	// uvcPanTiltStep is the relative pan/tilt delta per tick at full speed
	// (|axis| = 1). UVC camera controls are absolute ranges (e.g. pan
	// -25..25), so the step must be large enough to be VISIBLE: at 0.5 UI
	// speed this yields ±2.5 units per 100ms tick (≈ half the pan range in
	// ~4s of holding), at full speed ±5.
	uvcPanTiltStep = 5
	// uvcZoomStepUnit is the zoom delta per tick (device zoom ranges are
	// usually 0..N in 1-steps).
	uvcZoomStepUnit = 5
)

// uvcPTZTicker is one camera's running emulated-continuous-move loop.
type uvcPTZTicker struct {
	cancel func()
	done   chan struct{}
}

// uvcDeviceForCamera resolves a camera's UVC device identity from its config.
func (h *Handler) uvcDeviceForCamera(cameraID string) (uvc.Device, error) {
	if h.camMgr == nil {
		return uvc.Device{}, errors.New("camera manager not available")
	}
	cfg := h.camMgr.GetCameraConfig(cameraID)
	if cfg == nil || cfg.Protocol != string(model.ProtoUVC) || cfg.UVC == nil {
		return uvc.Device{}, errors.New("camera is not a UVC camera")
	}
	return uvc.Device{Name: cfg.UVC.Device, Path: cfg.UVC.DevicePath}, nil
}

// uvcPTZController returns the per-camera helper-backed controller, rebuilding
// it when the camera's device binding changed (device edits restart the
// recorder, but the cached controller may outlive the request).
func (h *Handler) uvcPTZController(cameraID string) (*uvc.UVCController, error) {
	device, err := h.uvcDeviceForCamera(cameraID)
	if err != nil {
		return nil, err
	}
	h.uvcPTZMu.Lock()
	defer h.uvcPTZMu.Unlock()
	if h.uvcPTZClients == nil {
		h.uvcPTZClients = make(map[string]*uvc.UVCController)
	}
	c := h.uvcPTZClients[cameraID]
	if c == nil || c.Device != device {
		if c != nil {
			c.Stop()
		}
		c = uvc.NewUVCController(device)
		h.uvcPTZClients[cameraID] = c
	}
	return c, nil
}

// handleUVCDevices lists locally attached capture devices for the add-camera
// form (GET /api/uvc/devices). In-process: enumeration is a stable COM path
// (only the property handlers crash — those live in the helper).
func (h *Handler) handleUVCDevices(w http.ResponseWriter, _ *http.Request) {
	devices, err := uvc.ListDevices()
	if err != nil {
		if errors.Is(err, uvc.ErrUnsupportedPlatform) {
			WriteError(w, http.StatusNotImplemented, "UVC device discovery is only supported on Windows")
			return
		}
		logger.Error("list UVC devices failed", "error", err)
		WriteError(w, http.StatusInternalServerError, "failed to list UVC devices")
		return
	}
	if devices == nil {
		devices = []uvc.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

// handleUVCAudioDevices lists locally attached audio capture (microphone)
// devices (GET /api/uvc/audio-devices) — the input picker for UVC cameras
// that record sound.
func (h *Handler) handleUVCAudioDevices(w http.ResponseWriter, _ *http.Request) {
	devices, err := uvc.ListAudioDevices()
	if err != nil {
		if errors.Is(err, uvc.ErrUnsupportedPlatform) {
			WriteError(w, http.StatusNotImplemented, "UVC audio device discovery is only supported on Windows")
			return
		}
		logger.Error("list UVC audio devices failed", "error", err)
		WriteError(w, http.StatusInternalServerError, "failed to list audio devices")
		return
	}
	if devices == nil {
		devices = []uvc.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

// handleUVCDeviceModes lists a device's supported capture modes
// (GET /api/uvc/device-modes?device=<friendly name>) — the add-form offers
// these as preset resolution/framerate combos instead of free-text inputs.
func (h *Handler) handleUVCDeviceModes(w http.ResponseWriter, r *http.Request) {
	device := strings.TrimSpace(r.URL.Query().Get("device"))
	if device == "" {
		WriteError(w, http.StatusBadRequest, "device query parameter is required")
		return
	}
	ffmpegPath := ""
	if h.config != nil {
		ffmpegPath = h.config.Transcoding.FFmpegPath
	}
	modes, err := uvc.ListDeviceModes(ffmpegPath, device)
	if err != nil {
		if errors.Is(err, uvc.ErrUnsupportedPlatform) {
			WriteError(w, http.StatusNotImplemented, "UVC mode discovery is only supported on Windows")
			return
		}
		logger.Error("list UVC device modes failed", "device", device, "error", err)
		WriteError(w, http.StatusInternalServerError, "failed to list device modes")
		return
	}
	if modes == nil {
		modes = []uvc.Mode{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"modes": modes})
}

// handleUVCPTZMove executes one unified PTZ vector on a UVC camera. Called
// from handlePTZMove's "uvc" dispatch arm with the already-decoded body.
func (h *Handler) handleUVCPTZMove(w http.ResponseWriter, r *http.Request, cameraID, mode string, pan, tilt, zoom float64) {
	ctrl, err := h.uvcPTZController(cameraID)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch mode {
	case "continuous":
		// Emulate press-and-hold: relative steps every tick until stop.
		h.startUVCPTZTicker(cameraID, ctrl, pan, tilt, zoom)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case "absolute":
		h.stopUVCPTZTicker(cameraID)
		if err := uvcNormalizedAbsoluteMove(ctrl, pan, tilt, zoom); err != nil {
			h.uvcPTZError(w, cameraID, "absolute move", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case "relative":
		h.stopUVCPTZTicker(cameraID)
		// One relative nudge in the vector's direction (normalized ±1 = one step).
		if err := ctrl.MoveBy(int(pan*uvcPanTiltStep), int(tilt*uvcPanTiltStep)); err != nil &&
			!errors.Is(err, uvc.ErrAxisUnsupported) {
			h.uvcPTZError(w, cameraID, "relative move", err)
			return
		}
		if zoom != 0 {
			if err := ctrl.ZoomStep(signI(zoom) * uvcZoomStepUnit); err != nil &&
				!errors.Is(err, uvc.ErrAxisUnsupported) {
				h.uvcPTZError(w, cameraID, "zoom", err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		WriteError(w, http.StatusBadRequest, "mode must be continuous, absolute, or relative")
	}
}

// handleUVCPTZStop ends the emulated continuous move (relative steps complete
// on their own, so cancelling the ticker is the whole stop).
func (h *Handler) handleUVCPTZStop(w http.ResponseWriter, _ *http.Request, cameraID string) {
	h.stopUVCPTZTicker(cameraID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// handleUVCPTZStatus reports the current control values (raw device units)
// and whether an emulated move is in flight. Axes the device does not expose
// report supported=false.
func (h *Handler) handleUVCPTZStatus(w http.ResponseWriter, _ *http.Request, cameraID string) {
	ctrl, err := h.uvcPTZController(cameraID)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	caps, err := ctrl.GetCapabilities()
	if err != nil {
		if errors.Is(err, uvc.ErrDeviceNotFound) {
			WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		logger.Error("get UVC PTZ status failed", "camera_id", cameraID, "error", err)
		WriteError(w, http.StatusInternalServerError, "get PTZ status failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pan":      caps.Pan.Value,
		"tilt":     caps.Tilt.Value,
		"zoom":     caps.Zoom.Value,
		"moving":   h.uvcPTZTickerActive(cameraID),
		"panAxis":  caps.Pan,
		"tiltAxis": caps.Tilt,
		"zoomAxis": caps.Zoom,
	})
}

// --- UVC presets (local camera_ptz_presets rows with stored positions) ---

// uvcPresetResponse mirrors the ONVIF PTZPreset shape consumed by the
// frontend (token + name) plus the stored absolute position.
type uvcPresetResponse struct {
	Token    string           `json:"token"`
	Name     string           `json:"name"`
	Position *uvc.PTZPosition `json:"position,omitempty"`
}

func (h *Handler) handleUVCPTZGetPresets(w http.ResponseWriter, r *http.Request, cameraID string) {
	presets := []uvcPresetResponse{}
	if h.db != nil {
		rows, err := h.db.ListPTZPresets(r.Context(), cameraID)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "failed to list presets")
			return
		}
		for _, p := range rows {
			entry := uvcPresetResponse{Token: p.Token, Name: p.Name}
			if p.Position != "" {
				var pos uvc.PTZPosition
				if json.Unmarshal([]byte(p.Position), &pos) == nil {
					entry.Position = &pos
				}
			}
			presets = append(presets, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"presets": presets})
}

func (h *Handler) handleUVCPTZCreatePreset(w http.ResponseWriter, r *http.Request, cameraID string) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	ctrl, err := h.uvcPTZController(cameraID)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.db == nil {
		WriteError(w, http.StatusInternalServerError, "database not available")
		return
	}
	token, ok := h.db.NextPTZPresetToken(r.Context(), cameraID)
	if !ok {
		WriteError(w, http.StatusConflict, "all 255 preset slots are in use")
		return
	}
	// The position IS the preset for UVC: read the current absolute
	// pan/tilt/zoom and store it. A device without working controls stores
	// zeros — goto then seeks to range minimums, which is honest for devices
	// that report positions only.
	pos, err := ctrl.GetPosition()
	if err != nil {
		logger.Warn("read UVC position for preset failed; storing zero position", "camera_id", cameraID, "error", err)
	}
	posJSON, err := json.Marshal(pos)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to encode preset position")
		return
	}
	if err := h.db.UpsertPTZPreset(r.Context(), storage.PTZPreset{
		CameraID:  cameraID,
		Token:     token,
		Name:      req.Name,
		Position:  string(posJSON),
		CreatedAt: time.Now(),
	}); err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to save preset")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (h *Handler) handleUVCPTZGoToPreset(w http.ResponseWriter, r *http.Request, cameraID, token string) {
	ctrl, err := h.uvcPTZController(cameraID)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.db == nil {
		WriteError(w, http.StatusInternalServerError, "database not available")
		return
	}
	if _, err := strconv.Atoi(token); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid preset token")
		return
	}
	rows, err := h.db.ListPTZPresets(r.Context(), cameraID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to load presets")
		return
	}
	var position string
	for _, p := range rows {
		if p.Token == token {
			position = p.Position
			break
		}
	}
	if position == "" {
		WriteError(w, http.StatusConflict, "preset has no stored position")
		return
	}
	var pos uvc.PTZPosition
	if err := json.Unmarshal([]byte(position), &pos); err != nil {
		WriteError(w, http.StatusInternalServerError, "stored preset position is corrupt")
		return
	}
	if err := ctrl.MoveAbsolute(&pos.Pan, &pos.Tilt, &pos.Zoom); err != nil {
		h.uvcPTZError(w, cameraID, "go to preset", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleUVCPTZDeletePreset(w http.ResponseWriter, r *http.Request, cameraID, token string) {
	if h.db == nil {
		WriteError(w, http.StatusInternalServerError, "database not available")
		return
	}
	if err := h.db.DeletePTZPreset(r.Context(), cameraID, token); err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to delete preset")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// uvcPTZError maps uvc package errors to HTTP statuses.
func (h *Handler) uvcPTZError(w http.ResponseWriter, cameraID, what string, err error) {
	switch {
	case errors.Is(err, uvc.ErrDeviceNotFound):
		WriteError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, uvc.ErrAxisUnsupported):
		WriteError(w, http.StatusNotImplemented, "PTZ control not supported by this camera ("+what+")")
	case errors.Is(err, uvc.ErrUnsupportedPlatform):
		WriteError(w, http.StatusNotImplemented, err.Error())
	default:
		logger.Error("UVC PTZ command failed", "camera_id", cameraID, "op", what, "error", err)
		WriteError(w, http.StatusInternalServerError, "PTZ command failed")
	}
}

// --- emulated continuous-move ticker registry ---

// startUVCPTZTicker launches (or restarts) the per-camera relative-step
// ticker implementing press-and-hold movement. pan/tilt carry the direction
// sign and speed magnitude in [-1,1]; zoom's sign picks the direction.
func (h *Handler) startUVCPTZTicker(cameraID string, ctrl *uvc.UVCController, pan, tilt, zoom float64) {
	h.stopUVCPTZTicker(cameraID)
	if pan == 0 && tilt == 0 && zoom == 0 {
		return
	}

	done := make(chan struct{})
	stop := make(chan struct{})
	t := &uvcPTZTicker{cancel: func() { close(stop) }, done: done}

	h.uvcPTZMu.Lock()
	if h.uvcPTZTickers == nil {
		h.uvcPTZTickers = make(map[string]*uvcPTZTicker)
	}
	h.uvcPTZTickers[cameraID] = t
	h.uvcPTZMu.Unlock()

	panStep := int(pan * uvcPanTiltStep)
	tiltStep := int(tilt * uvcPanTiltStep)
	zoomDelta := 0
	if zoom > 0 {
		zoomDelta = uvcZoomStepUnit
	} else if zoom < 0 {
		zoomDelta = -uvcZoomStepUnit
	}

	go func() {
		defer close(done)
		tk := time.NewTicker(uvcTickInterval)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if panStep != 0 || tiltStep != 0 {
					if err := ctrl.MoveBy(panStep, tiltStep); err != nil {
						slog.Debug("uvc ptz tick failed", "camera_id", cameraID, "error", err)
					}
				}
				if zoomDelta != 0 {
					if err := ctrl.ZoomStep(zoomDelta); err != nil {
						slog.Debug("uvc zoom tick failed", "camera_id", cameraID, "error", err)
					}
				}
			}
		}
	}()
}

// stopUVCPTZTicker cancels the camera's ticker and joins its goroutine.
func (h *Handler) stopUVCPTZTicker(cameraID string) {
	h.uvcPTZMu.Lock()
	t := h.uvcPTZTickers[cameraID]
	delete(h.uvcPTZTickers, cameraID)
	h.uvcPTZMu.Unlock()
	if t != nil {
		t.cancel()
		<-t.done
	}
}

// uvcPTZTickerActive reports whether an emulated move is in flight.
func (h *Handler) uvcPTZTickerActive(cameraID string) bool {
	h.uvcPTZMu.Lock()
	defer h.uvcPTZMu.Unlock()
	return h.uvcPTZTickers[cameraID] != nil
}

// stopAllUVCPTZTickers joins every running ticker and releases the per-camera
// helper processes (Handler.Close shutdown).
func (h *Handler) stopAllUVCPTZTickers() {
	h.uvcPTZMu.Lock()
	ids := make([]string, 0, len(h.uvcPTZTickers))
	for id := range h.uvcPTZTickers {
		ids = append(ids, id)
	}
	clients := h.uvcPTZClients
	h.uvcPTZClients = nil
	h.uvcPTZMu.Unlock()
	for _, id := range ids {
		h.stopUVCPTZTicker(id)
	}
	for _, c := range clients {
		c.Stop()
	}
}

// uvcNormalizedAbsoluteMove maps normalized [-1,1] axis values onto each
// axis's device range and seeks. Clamped for out-of-range inputs.
func uvcNormalizedAbsoluteMove(ctrl *uvc.UVCController, pan, tilt, zoom float64) error {
	caps, err := ctrl.GetCapabilities()
	if err != nil {
		return err
	}
	mapAxis := func(a uvc.PTZAxis, v float64) *int {
		if !a.Supported || !a.Absolute {
			return nil
		}
		if v < -1 {
			v = -1
		}
		if v > 1 {
			v = 1
		}
		pos := a.Min + int((float64(a.Max-a.Min))*(v+1)/2)
		return &pos
	}
	return ctrl.MoveAbsolute(mapAxis(caps.Pan, pan), mapAxis(caps.Tilt, tilt), mapAxis(caps.Zoom, zoom))
}

func signI(v float64) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}
