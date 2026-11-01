//go:build !windows

package procctl

import "os/exec"

// HideConsoleWindow is a no-op outside Windows: a console-mode child there
// inherits the parent's terminal instead of opening a window of its own, so
// there is nothing to suppress.
func HideConsoleWindow(*exec.Cmd) {}
