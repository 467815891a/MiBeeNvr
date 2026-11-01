//go:build windows

package uvc

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Pure-syscall DirectShow/COM plumbing (no cgo, no third-party COM wrapper).
// The vtable-invocation pattern mirrors internal/install/install_windows.go:
// COM interface pointers are held as uintptr and methods are invoked by
// vtable index through syscall.SyscallN.

// packageMu serializes all control-plane operations: enumeration, PTZ reads
// and PTZ writes are cheap (millisecond-scale) and drivers behave best when
// they never interleave.
var packageMu sync.Mutex

var (
	ole32  = syscall.NewLazyDLL("ole32.dll")
	oleaut = syscall.NewLazyDLL("oleaut32.dll")

	pCoInitializeEx = ole32.NewProc("CoInitializeEx")
	pCoUninitialize = ole32.NewProc("CoUninitialize")
	pCoCreateInst   = ole32.NewProc("CoCreateInstance")
	pCreateBindCtx  = ole32.NewProc("CreateBindCtx")
	pSysFreeString  = oleaut.NewProc("SysFreeString")
)

type guid = syscall.GUID

const (
	hresultOK = 0
	hrSFalse  = 1 // CreateClassEnumerator returns this for an empty category
	// STA (apartment-threaded) is what DirectShow requires: IAMCameraControl
	// proxies (KsProxy → driver) misbehave under MTA — live crash inside the
	// control Get call was observed on a real device under MTA. The calls in
	// this package are short and same-apartment (objects are created and used
	// on the calling thread), so no message pump is needed.
	coinitApartmentThreaded = 0x2
	clsctxInprocServer      = 1

	vtQI      = 0 // IUnknown::QueryInterface
	vtRelease = 2 // IUnknown::Release
)

// Vtable indices beyond IUnknown (each interface below inherits IUnknown only,
// except IMoniker which adds IPersist + IPersistStream before its own methods).
const (
	// ICreateDevEnum
	vtCreateClassEnumerator = 3
	// IEnumMoniker
	vtEnumNext = 3
	// IPropertyBag
	vtBagRead = 3
	// IAMCameraControl (IUnknown + 3 own methods)
	vtCCGet    = 3
	vtCCSet    = 4
	vtCCGetRng = 5
	// IMoniker: IUnknown(0-2) + IPersist::GetClassID(3) + IPersistStream::
	// IsDirty(4)/Load(5)/Save(6)/GetSizeMax(7), then the IMoniker methods.
	vtMonBindToObject  = 8
	vtMonBindToStorage = 9
)

var (
	clsidSystemDeviceEnum         = guid{Data1: 0x62BE5D10, Data2: 0x60EB, Data3: 0x11D0, Data4: [8]byte{0xBD, 0x3B, 0x00, 0xA0, 0xC9, 0x11, 0xCE, 0x86}}
	clsidVideoInputDeviceCategory = guid{Data1: 0x860BB310, Data2: 0x5D01, Data3: 0x11D0, Data4: [8]byte{0xBD, 0x3B, 0x00, 0xA0, 0xC9, 0x11, 0xCE, 0x86}}
	clsidAudioInputDeviceCategory = guid{Data1: 0x33D9A762, Data2: 0x90C8, Data3: 0x11D0, Data4: [8]byte{0xBD, 0x43, 0x00, 0xA0, 0xC9, 0x11, 0xCE, 0x86}}
	iidICreateDevEnum             = guid{Data1: 0x29840822, Data2: 0x5B84, Data3: 0x11D0, Data4: [8]byte{0xBD, 0x3B, 0x00, 0xA0, 0xC9, 0x11, 0xCE, 0x86}}
	iidIPropertyBag               = guid{Data1: 0x55272A00, Data2: 0x42CB, Data3: 0x11CE, Data4: [8]byte{0x81, 0x35, 0x00, 0xAA, 0x00, 0x4B, 0xB8, 0x51}}
	iidIBaseFilter                = guid{Data1: 0x56A86895, Data2: 0x0AD4, Data3: 0x11CE, Data4: [8]byte{0xB0, 0x3A, 0x00, 0x20, 0xAF, 0x0B, 0xA7, 0x70}}
	iidIAMCameraControl           = guid{Data1: 0xC6E13370, Data2: 0x30AC, Data3: 0x11D0, Data4: [8]byte{0xA1, 0x8C, 0x00, 0xA0, 0xC9, 0x11, 0x89, 0x56}}
	iidIAMVideoProcAmp            = guid{Data1: 0xC6E13360, Data2: 0x30AC, Data3: 0x11D0, Data4: [8]byte{0xA1, 0x8C, 0x00, 0xA0, 0xC9, 0x11, 0x89, 0x56}}
)

// vtCallHR invokes a COM method by vtable index and returns the raw HRESULT
// (callers distinguish S_OK / S_FALSE / property-unsupported themselves).
// Interface pointers are held as unsafe.Pointer end to end — vtable dispatch
// only dereferences the pointer, so no uintptr round-trip is needed anywhere.
func vtCallHR(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	// The COM object's first field is a pointer to its vtable; the vtable is
	// an array of function pointers. View the VTABLE (not the object) as the
	// array — everything stays pointer-typed (no uintptr round-trip).
	vtbl := (*[1 << 16]uintptr)(*(*unsafe.Pointer)(obj))
	fn := vtbl[idx]
	r1, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r1
}

func comRelease(obj unsafe.Pointer) {
	_ = vtCallHR(obj, vtRelease)
}

// hrErr renders a COM failure the way the rest of the repo renders Win32
// errors - hex HRESULT with context.
func hrErr(what string, hr uintptr) error {
	return fmt.Errorf("uvc: %s failed (hr=0x%08x)", what, hr)
}

// withCOM runs fn inside a guaranteed-initialized COM apartment (MTA - the
// calls here are short and pump no messages). All package operations funnel
// through this, and packageMu additionally serializes them so a control read
// never interleaves with a control write at the driver level.
func withCOM[T any](fn func() (T, error)) (T, error) {
	packageMu.Lock()
	defer packageMu.Unlock()

	initR, _, _ := pCoInitializeEx.Call(0, coinitApartmentThreaded)
	needUninit := initR == hresultOK
	// RPC_E_CHANGED_MODE (0x80010106): the thread already has a COM apartment
	// in the other mode - the calls below still work, but we must NOT pair a
	// CoUninitialize with a failed CoInitializeEx.
	defer func() {
		if needUninit {
			_, _, _ = pCoUninitialize.Call()
		}
	}()
	return fn()
}

// variant is the x64 OLE VARIANT layout: 24 bytes — an 8-byte header (vt +
// 3 reserved words) followed by the 16-byte value union (which includes
// DECIMAL, so it does NOT fit in 8). Getting this size wrong makes
// IPropertyBag::Read write 8 bytes past the struct and silently corrupt the
// caller's stack — the crash that surfaced later in unrelated COM calls.
// val holds the union's low word as a raw pointer (BSTR); the high word is
// unread padding that Read still writes.
type variant struct {
	vt   uint16
	res1 uint16
	res2 uint16
	res3 uint16
	val  unsafe.Pointer
	pad  uint64
}

const vtBSTR = 8

// monikerPropString reads one string property (e.g. "FriendlyName",
// "DevicePath") from a device moniker's property bag. Returns "" when the
// property is absent - DevicePath legitimately is, for non-PnP monikers.
func monikerPropString(mon, bc unsafe.Pointer, prop string) string {
	var bag unsafe.Pointer
	hr := vtCallHR(mon, vtMonBindToStorage, uintptr(bc), 0, uintptr(unsafe.Pointer(&iidIPropertyBag)), uintptr(unsafe.Pointer(&bag)))
	if hr != hresultOK || bag == nil {
		return ""
	}
	defer comRelease(bag)

	pProp, err := syscall.UTF16PtrFromString(prop)
	if err != nil {
		return ""
	}
	var v variant
	hr = vtCallHR(bag, vtBagRead, uintptr(unsafe.Pointer(pProp)), uintptr(unsafe.Pointer(&v)), 0)
	if hr != hresultOK || v.vt != vtBSTR || v.val == nil {
		return ""
	}
	defer func() { _, _, _ = pSysFreeString.Call(uintptr(v.val)) }()
	return windows.UTF16PtrToString((*uint16)(v.val))
}

// enumVideoDevices walks CLSID_VideoInputDeviceCategory and returns every
// attached capture device with its friendly name + stable device path.
func enumVideoDevices() ([]Device, error) {
	return enumDeviceCategory(&clsidVideoInputDeviceCategory, "video-input")
}

// enumDeviceCategory walks one device category and returns every attached
// device with its friendly name + stable device path.
func enumDeviceCategory(category *guid, label string) ([]Device, error) {
	var devEnum unsafe.Pointer
	r, _, _ := pCoCreateInst.Call(
		uintptr(unsafe.Pointer(&clsidSystemDeviceEnum)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidICreateDevEnum)), uintptr(unsafe.Pointer(&devEnum)))
	if r != hresultOK || devEnum == nil {
		return nil, hrErr("CoCreateInstance(SystemDeviceEnum)", r)
	}
	defer comRelease(devEnum)

	var enumMon unsafe.Pointer
	hr := vtCallHR(devEnum, vtCreateClassEnumerator,
		uintptr(unsafe.Pointer(category)),
		uintptr(unsafe.Pointer(&enumMon)), 0)
	if hr != hresultOK && hr != hrSFalse {
		return nil, hrErr("CreateClassEnumerator("+label+")", hr)
	}
	if enumMon == nil {
		// S_FALSE: category exists but is empty (no devices attached).
		return []Device{}, nil
	}
	defer comRelease(enumMon)

	var bc unsafe.Pointer
	if rb, _, _ := pCreateBindCtx.Call(0, uintptr(unsafe.Pointer(&bc))); rb == hresultOK && bc != nil {
		defer comRelease(bc)
	}

	devices := []Device{}
	for {
		var mon unsafe.Pointer
		var fetched uint32
		hr = vtCallHR(enumMon, vtEnumNext, 1, uintptr(unsafe.Pointer(&mon)), uintptr(unsafe.Pointer(&fetched)))
		if hr != hresultOK || fetched == 0 || mon == nil {
			break
		}
		name := monikerPropString(mon, bc, "FriendlyName")
		path := monikerPropString(mon, bc, "DevicePath")
		comRelease(mon)
		if name == "" && path == "" {
			continue
		}
		devices = append(devices, Device{Name: name, Path: path})
	}
	return devices, nil
}

// findDeviceMoniker locates the moniker for a configured device identity
// (device path preferred - stable across replug - falling back to the
// friendly name). Returns the moniker (caller releases) and the bind context
// used for property reads (released by the callback wrapper).
func findDeviceMoniker(device Device) (mon, bc unsafe.Pointer, err error) {
	devices, enumErr := enumVideoDevices()
	if enumErr != nil {
		return nil, nil, enumErr
	}

	match := func(d Device) bool {
		if device.Path != "" && d.Path != "" && strings.EqualFold(d.Path, device.Path) {
			return true
		}
		// Path match unavailable (empty path or a stale stored path — e.g. a
		// renamed/re-enumerated device) — fall back to the friendly name so
		// the PTZ path degrades exactly like the capture path
		// (ResolveCaptureTarget) does instead of failing while frames flow.
		return device.Name != "" && d.Name == device.Name
	}
	found := false
	for _, d := range devices {
		if match(d) {
			found = true
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("%w: %q (attached: %d)", ErrDeviceNotFound, device.Name, len(devices))
	}

	// Second pass re-walks the live enumerator - monikers cannot be cached
	// across COM apartments, so the match decision above and the binding
	// below intentionally run over two separate enumerations. Devices do not
	// change between the two passes in practice; if one does, the worst case
	// is a bind miss which surfaces as ErrDeviceNotFound.
	if rb, _, _ := pCreateBindCtx.Call(0, uintptr(unsafe.Pointer(&bc))); rb != hresultOK || bc == nil {
		return nil, nil, hrErr("CreateBindCtx", rb)
	}

	var devEnum unsafe.Pointer
	r, _, _ := pCoCreateInst.Call(
		uintptr(unsafe.Pointer(&clsidSystemDeviceEnum)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidICreateDevEnum)), uintptr(unsafe.Pointer(&devEnum)))
	if r != hresultOK || devEnum == nil {
		comRelease(bc)
		return nil, nil, hrErr("CoCreateInstance(SystemDeviceEnum)", r)
	}
	defer comRelease(devEnum)

	var enumMon unsafe.Pointer
	hr := vtCallHR(devEnum, vtCreateClassEnumerator,
		uintptr(unsafe.Pointer(&clsidVideoInputDeviceCategory)),
		uintptr(unsafe.Pointer(&enumMon)), 0)
	if (hr != hresultOK && hr != hrSFalse) || enumMon == nil {
		comRelease(bc)
		return nil, nil, ErrDeviceNotFound
	}
	defer comRelease(enumMon)

	for {
		var m unsafe.Pointer
		var fetched uint32
		hr = vtCallHR(enumMon, vtEnumNext, 1, uintptr(unsafe.Pointer(&m)), uintptr(unsafe.Pointer(&fetched)))
		if hr != hresultOK || fetched == 0 || m == nil {
			break
		}
		name := monikerPropString(m, bc, "FriendlyName")
		path := monikerPropString(m, bc, "DevicePath")
		if match(Device{Name: name, Path: path}) {
			return m, bc, nil // ownership of m and bc transfers to the caller
		}
		comRelease(m)
	}
	comRelease(bc)
	return nil, nil, fmt.Errorf("%w: %q (attached: %d)", ErrDeviceNotFound, device.Name, len(devices))
}
