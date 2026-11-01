package camera

import (
	"context"
	"fmt"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/onvif"
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
// native PTZ control — the bridge used by the GB28181 cascade when the upper
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
			return ptz.Stop(ctx, true, true)
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
		// Control calls run in the sacrificial uvc-ptz helper subprocess —
		// same rationale as the API PTZ path (vendor proxies can crash).
		// Cascade commands are low-frequency, so a short-lived controller
		// (one helper spawn per command) is the simple correct shape.
		dev := uvc.Device{Name: cfg.UVC.Device, Path: cfg.UVC.DevicePath}
		ctrl := uvc.NewUVCController(dev)
		defer ctrl.Stop()
		switch direction {
		case "stop":
			// UVC relative steps complete on their own — nothing to halt.
			return nil
		case "zoom-in":
			return ctrl.ZoomStep(1)
		case "zoom-out":
			return ctrl.ZoomStep(-1)
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
		return ctrl.MoveBy(pan, tilt)

	default:
		return fmt.Errorf("camera %q (protocol %q) has no PTZ support", cameraID, cfg.Protocol)
	}
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
