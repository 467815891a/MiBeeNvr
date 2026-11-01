//go:build !windows

package uvc

import "fmt"

// UVCController drives one UVC camera's PTZ controls through the
// `mibee-nvr uvc-ptz` helper subprocess (Windows only today — see
// client_windows.go for the rationale). Non-Windows: every operation returns
// ErrUnsupportedPlatform.
type UVCController struct {
	Device Device
}

func NewUVCController(device Device) *UVCController { return &UVCController{Device: device} }

func (c *UVCController) GetCapabilities() (PTZCapabilities, error) {
	return PTZCapabilities{}, ErrUnsupportedPlatform
}

func (c *UVCController) GetPosition() (PTZPosition, error) {
	return PTZPosition{}, ErrUnsupportedPlatform
}

func (c *UVCController) MoveBy(panDelta, tiltDelta int) error {
	return fmt.Errorf("%w: %q", ErrUnsupportedPlatform, c.Device.Name)
}

func (c *UVCController) ZoomStep(delta int) error {
	return fmt.Errorf("%w: %q", ErrUnsupportedPlatform, c.Device.Name)
}

func (c *UVCController) MoveAbsolute(pan, tilt, zoom *int) error {
	return fmt.Errorf("%w: %q", ErrUnsupportedPlatform, c.Device.Name)
}

func (c *UVCController) Home() error {
	return fmt.Errorf("%w: %q", ErrUnsupportedPlatform, c.Device.Name)
}

func (c *UVCController) Stop() {}
