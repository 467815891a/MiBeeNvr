package camera

import (
	"context"
	"fmt"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/onvif"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/storage"
	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/uvc"
)

// motorController is implemented by Xiaomi recorders (MotorControl direction:
// "up"/"down"/"left"/"right"/"stop", speed 1-100).
type motorController interface {
	MotorControl(direction string, speed int) error
}

// uvcCascadeBurstSteps is the relative pan/tilt step count per forwarded GB
// DeviceControl. Upper platforms send repeated commands while the operator
// holds the pad and "stop" on release, so per-command bursts compose into
// continuous movement with native-feeling responsiveness.
const uvcCascadeBurstSteps = 3

// ForwardPTZ routes a decoded GB/T 28181 PTZ command to a local camera's
// native PTZ control 鈥?the bridge used by the GB28181 cascade when the upper
// platform sends a DeviceControl for one of our aggregated channels:
//
//   - gb28181 cameras: forwarded through the local platform's PTZ controller
//     (gbSend, SIP MESSAGE to the device);
//   - ONVIF cameras: ContinuousMove / Stop with the direction mapped onto a
//     normalized velocity vector;
//   - Xiaomi cameras: MotorControl;
//   - UVC cameras: a short relative-step burst (UVC controls are one-shot;
//     repeated GB commands compose into continuous movement).
//
// speed is the GB command byte (0-255); ONVIF/Xiaomi normalize it to their own
// scales.
func ForwardPTZ(ctx context.Context, cm *CameraManager, gbSend func(channelID, direction string, speed byte) error, cameraID, direction string, speed byte) error {
	if cm == nil {
		return fmt.Errorf("camera manager not available")
	}
	cfg := cm.GetCameraConfig(cameraID)
	if cfg == nil {
		return fmt.Errorf("unknown camera %q", cameraID)
	}

	switch cfg.Protocol {
	case "gb28181":
		if gbSend == nil {
			return fmt.Errorf("gb28181 PTZ controller not available")
		}
		if cfg.GB28181.ChannelID == "" {
			return fmt.Errorf("camera %q has no GB channel binding", cameraID)
		}
		return gbSend(cfg.GB28181.ChannelID, direction, speed)

	case "onvif":
		ptz, err := cm.GetONVIFPTZController(ctx, cameraID)
		if err != nil {
			return err
		}
		if direction == "stop" {
			if err := ptz.Stop(ctx, true, true); err != nil {
				return err
			}
			// The move ended here 鈥?remember where the camera stopped.
			cm.saveForwardedONVIFPTZState(ctx, cameraID, ptz)
			return nil
		}
		v := ptzVectorFor(direction, speed)
		if v == (onvif.PTZVector{}) {
			return fmt.Errorf("direction %q not supported by ONVIF PTZ forward", direction)
		}
		return ptz.ContinuousMove(ctx, v)

	case "xiaomi":
		rec := cm.GetRecorder(cameraID)
		motor, ok := rec.(motorController)
		if !ok {
			return fmt.Errorf("camera %q is not connected (no Xiaomi recorder)", cameraID)
		}
		switch direction {
		case "up", "down", "left", "right":
			s := int(speed)
			if s < 1 {
				s = 1
			}
			if s > 100 {
				s = 100
			}
			return motor.MotorControl(direction, s)
		case "stop":
			return motor.MotorControl("stop", 0)
		default:
			return fmt.Errorf("direction %q not supported by Xiaomi motor", direction)
		}

	case "uvc":
		if cfg.UVC == nil {
			return fmt.Errorf("camera %q has no UVC device binding", cameraID)
		}
		// Control calls run in the sacrificial uvc-ptz helper subprocess 鈥?		// same rationale as the API PTZ path (vendor proxies can crash).
		// Cascade commands are low-frequency, so a short-lived controller
		// (one helper spawn per command) is the simple correct shape.
		dev := uvc.Device{Name: cfg.UVC.Device, Path: cfg.UVC.DevicePath}
		ctrl := uvc.NewUVCController(dev)
		defer ctrl.Stop()
		switch direction {
		case "stop":
			// UVC relative steps complete on their own 鈥?nothing to halt.
			return nil
		case "zoom-in":
			if err := ctrl.ZoomStep(1); err != nil {
				return err
			}
			cm.saveForwardedUVCPTZState(ctx, cameraID, ctrl)
			return nil
		case "zoom-out":
			if err := ctrl.ZoomStep(-1); err != nil {
				return err
			}
			cm.saveForwardedUVCPTZState(ctx, cameraID, ctrl)
			return nil
		}
		pan, tilt := 0, 0
		switch direction {
		case "up":
			tilt = uvcCascadeBurstSteps
		case "down":
			tilt = -uvcCascadeBurstSteps
		case "left":
			pan = -uvcCascadeBurstSteps
		case "right":
			pan = uvcCascadeBurstSteps
		case "up-left":
			pan, tilt = -uvcCascadeBurstSteps, uvcCascadeBurstSteps
		case "up-right":
			pan, tilt = uvcCascadeBurstSteps, uvcCascadeBurstSteps
		case "down-left":
			pan, tilt = -uvcCascadeBurstSteps, -uvcCascadeBurstSteps
		case "down-right":
			pan, tilt = uvcCascadeBurstSteps, -uvcCascadeBurstSteps
		default:
			return fmt.Errorf("direction %q not supported by UVC PTZ forward", direction)
		}
		if err := ctrl.MoveBy(pan, tilt); err != nil {
			return err
		}
		// The cascade moved the camera 鈥?keep the position memory in sync so
		// a restart puts it back where the cascade left it.
		cm.saveForwardedUVCPTZState(ctx, cameraID, ctrl)
		return nil

	default:
		return fmt.Errorf("camera %q (protocol %q) has no PTZ support", cameraID, cfg.Protocol)
	}
}

// saveForwardedUVCPTZState stores the position a cascade command left a UVC
// camera at, so the position memory stays in sync with the hardware.
func (cm *CameraManager) saveForwardedUVCPTZState(ctx context.Context, cameraID string, ctrl *uvc.UVCController) {
	if ctrl == nil {
		return
	}
	pos, err := ctrl.GetPosition()
	if err != nil {
		logger.Debug("read the UVC PTZ position after a cascade move failed",
			"camera_id", cameraID, "error", err)
		return
	}
	cm.storePTZState(ctx, cameraID, storage.PTZKindUVCAbsolute,
		float64(pos.Pan), float64(pos.Tilt), float64(pos.Zoom))
}

// saveForwardedONVIFPTZState stores the position a cascade command left an
// ONVIF camera at.
func (cm *CameraManager) saveForwardedONVIFPTZState(ctx context.Context, cameraID string, ptz onvif.PTZController) {
	if ptz == nil {
		return
	}
	pos, _, err := ptz.GetStatus(ctx)
	if err != nil {
		logger.Debug("read the ONVIF PTZ position after a cascade move failed",
			"camera_id", cameraID, "error", err)
		return
	}
	cm.storePTZState(ctx, cameraID, storage.PTZKindONVIFNormalized, pos.Pan, pos.Tilt, pos.Zoom)
}

// ptzVectorFor maps a GB direction + speed byte onto an ONVIF velocity vector
// (each axis in [-1, 1]). Diagonals combine both axes.
func ptzVectorFor(direction string, speed byte) onvif.PTZVector {
	s := float64(speed) / 255.0
	if s <= 0 {
		s = 0.5
	}
	var v onvif.PTZVector
	switch direction {
	case "up":
		v.Tilt = s
	case "down":
		v.Tilt = -s
	case "left":
		v.Pan = -s
	case "right":
		v.Pan = s
	case "up-left":
		v.Tilt, v.Pan = s, -s
	case "up-right":
		v.Tilt, v.Pan = s, s
	case "down-left":
		v.Tilt, v.Pan = -s, -s
	case "down-right":
		v.Tilt, v.Pan = -s, s
	case "zoom-in":
		v.Zoom = s
	case "zoom-out":
		v.Zoom = -s
	default:
		return onvif.PTZVector{}
	}
	return v
}
