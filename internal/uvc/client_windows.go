//go:build windows

package uvc

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Mi-Bee-Studio/MiBeeNvr/internal/procctl"
)

// UVCController drives one UVC camera's PTZ controls through a resident
// PowerShell helper process (see pshelper_windows.go for the script).
//
// WHY A HELPER: vendor camera proxies have been observed crashing the calling
// process on DirectShow control calls (reproduced with an independent C#
// probe on a Logitech MeetUp — the fault is in the kswdmcap.ax → driver
// layer). The working control path on that device is the WinRT/MediaCapture
// property API, which Go cannot call without a WinRT projection — but
// Windows ships PowerShell 5.1 with the projection built in. The helper is
// therefore a resident PowerShell process speaking a line-based JSON
// protocol (spawned via -EncodedCommand): a driver-level fault kills only
// the helper and surfaces as a failed API call, and the helper is respawned
// on demand.
type UVCController struct {
	Device Device

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     *json.Encoder
	stdout    *bufio.Reader
	lastCrash time.Time
}

// uvcPtzRequest / uvcPtzResponse mirror the helper's wire format.
type uvcPtzRequest struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Name  string `json:"name,omitempty"`
	Pan   *int   `json:"pan,omitempty"`
	Tilt  *int   `json:"tilt,omitempty"`
	Zoom  *int   `json:"zoom,omitempty"`
	Delta *int   `json:"delta,omitempty"`
}

type uvcPtzResponse struct {
	OK    bool               `json:"ok"`
	Error string             `json:"error,omitempty"`
	Caps  *PTZCapabilities   `json:"caps,omitempty"`
	Pos   *PTZPosition       `json:"pos,omitempty"`
}

// NewUVCController creates a controller for one device. The helper process
// is spawned lazily on the first operation.
func NewUVCController(device Device) *UVCController {
	return &UVCController{Device: device}
}

// close kills the current helper (if any) so the next call respawns it.
func (c *UVCController) close() {
	if c.cmd != nil {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
		c.cmd = nil
		c.stdin = nil
		c.stdout = nil
	}
}

// start spawns the PowerShell helper and sends the device identity.
func (c *UVCController) start() error {
	// -EncodedCommand takes base64-encoded UTF-16LE — immune to quoting.
	encoded := base64.StdEncoding.EncodeToString(utf16Encoded(psHelperScript))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded)
	// The NVR has no console of its own (GUI subsystem) — without this the
	// helper would open a console window on the taskbar for as long as it
	// lives.
	procctl.HideConsoleWindow(cmd)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("uvc-ptz: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("uvc-ptz: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("uvc-ptz: start powershell: %w", err)
	}
	c.cmd = cmd
	c.stdin = json.NewEncoder(stdinPipe)
	c.stdout = bufio.NewReader(stdoutPipe)

	if err := c.stdin.Encode(uvcPtzRequest{Op: "dev", Path: c.Device.Path, Name: c.Device.Name}); err != nil {
		c.close()
		return fmt.Errorf("uvc-ptz: send dev: %w", err)
	}
	var resp uvcPtzResponse
	if err := json.NewDecoder(c.stdout).Decode(&resp); err != nil {
		c.close()
		return fmt.Errorf("uvc-ptz: dev response: %w", err)
	}
	if !resp.OK {
		c.close()
		return fmt.Errorf("uvc-ptz: dev: %s", resp.Error)
	}
	return nil
}

// utf16Encoded converts a Go string to UTF-16LE bytes (the encoding
// -EncodedCommand expects).
func utf16Encoded(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(u16)*2+2)
	for _, r := range u16 {
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

// call sends one request and decodes one response, restarting the helper and
// retrying once when the previous attempt died (a driver/fault surfaces as
// EOF / broken pipe). Two consecutive crashes of the same op return an error.
func (c *UVCController) call(req uvcPtzRequest) (*uvcPtzResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	quickRecrash := time.Since(c.lastCrash) < 5*time.Second
	if quickRecrash {
		return nil, fmt.Errorf("uvc-ptz: control helper keeps failing on this device — PTZ unavailable for %q", c.Device.Name)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if c.cmd == nil {
			if err := c.start(); err != nil {
				lastErr = err
				c.lastCrash = time.Now()
				continue
			}
		}
		if err := c.stdin.Encode(req); err != nil {
			c.close()
			lastErr = err
			continue
		}
		var resp uvcPtzResponse
		if err := json.NewDecoder(c.stdout).Decode(&resp); err != nil {
			c.close()
			c.lastCrash = time.Now()
			lastErr = err
			continue
		}
		if !resp.OK {
			return &resp, fmt.Errorf("%s", resp.Error)
		}
		return &resp, nil
	}
	return nil, fmt.Errorf("uvc-ptz: helper failed twice on op %q for %q: %w", req.Op, c.Device.Name, lastErr)
}

// GetCapabilities dumps the pan/tilt/zoom ranges and flags via the helper.
func (c *UVCController) GetCapabilities() (PTZCapabilities, error) {
	resp, err := c.call(uvcPtzRequest{Op: "caps"})
	if err != nil {
		return PTZCapabilities{}, err
	}
	if resp.Caps == nil {
		return PTZCapabilities{}, fmt.Errorf("uvc-ptz: caps response missing payload")
	}
	return *resp.Caps, nil
}

// GetPosition returns the current absolute pan/tilt/zoom via the helper.
func (c *UVCController) GetPosition() (PTZPosition, error) {
	resp, err := c.call(uvcPtzRequest{Op: "pos"})
	if err != nil {
		return PTZPosition{}, err
	}
	if resp.Pos == nil {
		return PTZPosition{}, fmt.Errorf("uvc-ptz: pos response missing payload")
	}
	return *resp.Pos, nil
}

// MoveBy performs one relative pan/tilt step via the helper.
func (c *UVCController) MoveBy(panDelta, tiltDelta int) error {
	_, err := c.call(uvcPtzRequest{Op: "rel", Pan: &panDelta, Tilt: &tiltDelta})
	return err
}

// ZoomStep zooms one step toward tele (positive) / wide (negative) via the helper.
func (c *UVCController) ZoomStep(delta int) error {
	_, err := c.call(uvcPtzRequest{Op: "zoom", Delta: &delta})
	return err
}

// MoveAbsolute seeks to an absolute position via the helper (nil skips an axis).
func (c *UVCController) MoveAbsolute(pan, tilt, zoom *int) error {
	_, err := c.call(uvcPtzRequest{Op: "abs", Pan: pan, Tilt: tilt, Zoom: zoom})
	return err
}

// Home resets pan/tilt/zoom to the device defaults via the helper.
func (c *UVCController) Home() error {
	_, err := c.call(uvcPtzRequest{Op: "home"})
	return err
}

// Stop terminates the helper (camera deleted / NVR shutdown). Idempotent.
func (c *UVCController) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close()
}
