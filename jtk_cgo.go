//go:build cgo

package jtk

/*
#cgo CFLAGS: -I.
#cgo LDFLAGS: ${SRCDIR}/libjtk.a -lX11 -lXext -lXcursor -lXi -lXfixes -lXrandr -lXtst -lXss -lfontconfig -lrt -lpthread -lm -ldl -lstdc++

#include <stdlib.h>
#include <stdbool.h>

// --- Definitions from your API ---

typedef enum {
    JTK_TYPE_NIL = 0,
    JTK_TYPE_BOOL = 1,
    JTK_TYPE_NUMBER = 2,
    JTK_TYPE_STRING = 3
} JtkType;

typedef struct {
    char* path;
    JtkType type;
    union {
        bool b_val;
        double n_val;
        char* s_val;
    } value;
} JtkEvent;

void JTK_Run(const char* module_name);
void JTK_State_Update(const char* path, int type, void* val_ptr);
bool JTK_WaitEvent(JtkEvent* out_event);
void JTK_FreeEvent(JtkEvent* event);

// --- C Helper functions for Union access ---
// Accessing complex C unions directly from Go is messy (Go sees them as byte arrays).
// These static inline helpers make it typesafe and clean.

static bool get_event_bool(JtkEvent* e) { return e->value.b_val; }
static double get_event_number(JtkEvent* e) { return e->value.n_val; }
static char* get_event_string(JtkEvent* e) { return e->value.s_val; }

*/
import "C"
import (
	"fmt"
	"runtime"
	"unsafe"
)

func init() {
	go eventLoop()
}

func eventLoop() {
	// Allocate struct on C stack (or Go stack mapped to C)
	var event C.JtkEvent

	for {
		// Blocks until an event arrives.
		// Note: 'C.bool' is a distinct type from Go 'bool'.
		if bool(C.JTK_WaitEvent(&event)) {

			path := C.GoString(event.path)
			goVal := decodeEvent(&event)

			// Free C memory immediately (specifically the internals of JtkEvent)
			C.JTK_FreeEvent(&event)

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
			// Yield just in case wait fails to prevent CPU spin
			runtime.Gosched()
		}
	}
}

// decodeEvent extracts data using the C helper functions
func decodeEvent(e *C.JtkEvent) interface{} {
	// Note: C struct field 'type' matches a Go keyword, so cgo renames it to '_type'.
	switch e._type {
	case C.JTK_TYPE_NIL:
		return nil

	case C.JTK_TYPE_BOOL:
		// Use helper to read union
		return bool(C.get_event_bool(e))

	case C.JTK_TYPE_NUMBER:
		return float64(C.get_event_number(e))

	case C.JTK_TYPE_STRING:
		cStr := C.get_event_string(e)
		return C.GoString(cStr)

	default:
		return nil
	}
}

func Run(moduleName string) {
	// Convert Go string to C string
	cName := C.CString(moduleName)
	defer C.free(unsafe.Pointer(cName))

	C.JTK_Run(cName)
}

func Update(path string, value interface{}) {
	mu.RLock()
	ready := isReady
	mu.RUnlock()
	if !ready {
		fmt.Printf("[JTK Go] Warning: Update '%s' skipped (Lua not ready)\n", path)
		return
	}

	// Prepare data for C
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	switch v := value.(type) {
	case bool:
		cVal := C.bool(v)
		// We pass the pointer to the boolean
		C.JTK_State_Update(cPath, C.JTK_TYPE_BOOL, unsafe.Pointer(&cVal))

	case int:
		// JTK expects JTK_TYPE_NUMBER to be a double
		cVal := C.double(float64(v))
		C.JTK_State_Update(cPath, C.JTK_TYPE_NUMBER, unsafe.Pointer(&cVal))

	case float64:
		cVal := C.double(v)
		C.JTK_State_Update(cPath, C.JTK_TYPE_NUMBER, unsafe.Pointer(&cVal))

	case string:
		// Convert value string to C string
		cValStr := C.CString(v)
		defer C.free(unsafe.Pointer(cValStr))
		C.JTK_State_Update(cPath, C.JTK_TYPE_STRING, unsafe.Pointer(cValStr))

	case nil:
		C.JTK_State_Update(cPath, C.JTK_TYPE_NIL, nil) // unsafe.Pointer(nil) happens automatically? Better be explicit if needed, but nil works in cgo usually.

	default:
		fmt.Printf("[JTK Go] Unsupported type for path '%s'\n", path)
	}
}
