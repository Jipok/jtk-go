//go:build !cgo

package jtk

import (
	"fmt"
	"math"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

// =================================================================================
// Constants & Types
// =================================================================================

type JtkType int32

const (
	TypeNil    JtkType = 0
	TypeBool   JtkType = 1
	TypeNumber JtkType = 2
	TypeString JtkType = 3
)

// jtkEventC mirrors the memory layout of the C struct JtkEvent.
// C Layout (64-bit):
//
//	char* path;      // 8 bytes
//	int type;        // 4 bytes
//	[padding];       // 4 bytes (to align the next 8-byte union)
//	union { ... };   // 8 bytes (max size of double or char*)
type jtkEventC struct {
	Path  *byte   // Corresponds to char*
	Type  JtkType // Corresponds to enum int
	_     [4]byte // Manual padding for 64-bit alignment
	Value uint64  // Placeholder for the union content (8 bytes)
}

// Global function pointers
var (
	libHandle    uintptr
	cRun         func(string)
	cStateUpdate func(string, JtkType, unsafe.Pointer)
	cWaitEvent   func(*jtkEventC) bool
	cFreeEvent   func(*jtkEventC)
)

func init() {
	var libPath string

	switch runtime.GOOS {
	case "windows":
		libPath = "jtk.dll"
	case "darwin":
		libPath = "./libjtk.dylib"
	case "linux":
		libPath = "./libjtk.so"
	default:
		panic(fmt.Errorf("unsupported OS: %s", runtime.GOOS))
	}

	lib, err := purego.Dlopen(libPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		panic(fmt.Errorf("failed to load %s: %v", libPath, err))
	}
	libHandle = lib

	purego.RegisterLibFunc(&cRun, libHandle, "JTK_Run")
	purego.RegisterLibFunc(&cStateUpdate, libHandle, "JTK_State_Update")
	purego.RegisterLibFunc(&cWaitEvent, libHandle, "JTK_WaitEvent")
	purego.RegisterLibFunc(&cFreeEvent, libHandle, "JTK_FreeEvent")

	go eventLoop()
}

// bytePtrToString converts a C-style null-terminated string to a Go string
func bytePtrToString(ptr *byte) string {
	if ptr == nil {
		return ""
	}
	// purego doesn't export a generic string helper, so we use unsafe
	// essentially finding the null terminator.
	// A strictly safer way is usually iterating until 0.
	var length int
	for {
		if *(*byte)(unsafe.Add(unsafe.Pointer(ptr), length)) == 0 {
			break
		}
		length++
	}
	return string(unsafe.Slice(ptr, length))
}

// eventLoop runs in a background goroutine, waiting for C events
func eventLoop() {
	var event jtkEventC

	for {
		// This blocks until C signals an event
		ok := cWaitEvent(&event)
		if ok {
			path := bytePtrToString(event.Path)
			goVal := decodeUnion(&event)

			// Free C memory immediately after copying data to Go
			cFreeEvent(&event)

			// System event check
			if path == "Lua State Created" {
				mu.Lock()
				isReady = true
				mu.Unlock()
				select {
				case <-readyChan:
				default:
					close(readyChan)
				}
			}

			dispatchEvent(path, goVal)
		} else {
			// Small yield if wait failed to prevent spin-lock behavior (if implementation changes)
			runtime.Gosched()
		}
	}
}

// decodeUnion reads the memory of the union based on the event Type
func decodeUnion(e *jtkEventC) interface{} {
	// The Value field is a uint64 representing 8 bytes of memory.
	// We need to interpret these bytes differently based on e.Type.

	switch e.Type {
	case TypeNil:
		return nil

	case TypeBool:
		// bool is usually 1 byte. We cast the address of Value to *bool
		valPtr := (*bool)(unsafe.Pointer(&e.Value))
		return *valPtr

	case TypeNumber:
		// double is 8 bytes. Cast address to *float64
		// Or use math functions if we treat Value as bits
		return math.Float64frombits(e.Value)

	case TypeString:
		// char* is a pointer (uintptr).
		// We treat the stored uint64 as a uintptr (address of the string chars)
		strPtr := (*byte)(unsafe.Pointer(uintptr(e.Value)))
		return bytePtrToString(strPtr)

	default:
		return nil
	}
}

// Run starts the UI. This blocks the main thread.
func Run(moduleName string) {
	// TODO reset state and channel?
	cRun(moduleName)
}

// Update sends data from Go to the Lua state.
func Update(path string, value interface{}) {
	// If not ready, we might want to log or buffer. For now, we skip.
	mu.RLock()
	ready := isReady
	mu.RUnlock()
	if !ready {
		fmt.Printf("[JTK Go] Warning: Update '%s' skipped (Lua not ready)\n", path)
		return
	}

	// Prepare data for C
	// JTK_State_Update(const char* path, int type, void* val_ptr)

	switch v := value.(type) {
	case bool:
		// Pass address of bool
		// Copy to variable to ensure addressable memory
		val := v
		cStateUpdate(path, TypeBool, unsafe.Pointer(&val))

	case int:
		// Convert to float64 (double) as JTK API demands numbers be doubles
		val := float64(v)
		cStateUpdate(path, TypeNumber, unsafe.Pointer(&val))

	case float64:
		val := v
		cStateUpdate(path, TypeNumber, unsafe.Pointer(&val))

	case string:
		// For string, purego passes string as char* automatically for arguments,
		// but JTK_State_Update's 3rd arg is void*.
		// If we pass a string in Go to a void* argument in purego, it might not convert automatically.
		// Use ByteString/CString conversion explicitly or unsafe.Pointer to byte slice.

		b := []byte(v + "\x00") // Ensure null termination
		cStateUpdate(path, TypeString, unsafe.Pointer(&b[0]))

	case nil:
		cStateUpdate(path, TypeNil, nil)

	default:
		fmt.Printf("[JTK Go] Unsupported type for path '%s'\n", path)
	}
}
