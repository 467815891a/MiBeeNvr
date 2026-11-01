//go:build windows

package transcoding

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx mirrors the MEMORYSTATUSEX Win32 structure.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	globalMemoryStatusEx  = kernel32.NewProc("GlobalMemoryStatusEx")
)

// windowsTotalMemoryMB returns total physical RAM in MB via GlobalMemoryStatusEx.
// The /proc/meminfo path used on Linux does not exist on Windows.
func windowsTotalMemoryMB() uint64 {
	var status memoryStatusEx
	status.dwLength = uint32(unsafe.Sizeof(status))
	r1, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if r1 == 0 {
		return 0
	}
	return status.ullTotalPhys / 1024 / 1024
}
