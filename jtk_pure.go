//go:build !cgo

package jtk

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
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
type jtkEventC struct {
	Path *byte   // Corresponds to char*
	Type JtkType // Corresponds to enum int
	// Go automatically inserts 4 bytes of padding here on 64-bit systems
	Value uint64 // 8-byte placeholder acting as the C union
}

type cEmbeddedAsset struct {
	Path *byte
	Data *byte
	Size uintptr
}

// Global function pointers
var (
	libHandle    uintptr
	cRun         func(moduleName string, argc int32, argv unsafe.Pointer)
	cSetAssets   func(assets unsafe.Pointer)
	cStateUpdate func(path string, jtkType JtkType, valPtr unsafe.Pointer)
	cWaitEvent   func(*jtkEventC) bool
	cFreeEvent   func(*jtkEventC)

	// Хранилище, чтобы сборщик мусора (GC) не очистил ассеты пока "C" ими пользуется (до завершения программы)
	pinnedAssetsSlice []cEmbeddedAsset
	pinnedAssetData   [][]byte
)

func init() {
	var libPath string

	switch runtime.GOOS {
	case "windows":
		libPath = "jtk.dll" // Or purego.Dlopen logic for Windows
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
	purego.RegisterLibFunc(&cSetAssets, libHandle, "JTK_SetAssets")
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
	var length int
	for {
		if *(*byte)(unsafe.Add(unsafe.Pointer(ptr), length)) == 0 {
			break
		}
		length++
	}
	// string(unsafe.Slice(ptr, ...)) performs a COPY of the C memory.
	// This is critical since `JTK_FreeEvent` drops the C memory immediately afterward.
	return string(unsafe.Slice(ptr, length))
}

func eventLoop() {
	var event jtkEventC

	for {
		if cWaitEvent(&event) {
			path := bytePtrToString(event.Path)
			goVal := decodeUnion(&event)

			// Safely free the struct internals exactly as allocated
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
			runtime.Gosched()
		}
	}
}

// decodeUnion handles extracting underlying C Union data. Direct pointer casting
// is entirely agnostic to architectures/endianness compared to bit mathematical casting.
func decodeUnion(e *jtkEventC) interface{} {
	// Grab the address of the placeholder uint64 and cast according to type
	unionPtr := unsafe.Pointer(&e.Value)

	switch e.Type {
	case TypeNil:
		return nil

	case TypeBool:
		return *(*bool)(unionPtr)

	case TypeNumber:
		return *(*float64)(unionPtr)

	case TypeString:
		strPtr := *(**byte)(unionPtr)
		return bytePtrToString(strPtr)

	default:
		return nil
	}
}

// Run starts the UI. This blocks the main thread.
func Run(moduleName string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Mimic the cgo string matrix mapping for char** argv
	argc := int32(len(os.Args))
	argv := make([]*byte, len(os.Args))
	for i, arg := range os.Args {
		b := append([]byte(arg), 0)
		argv[i] = &b[0]
	}

	var argvPtr unsafe.Pointer
	if argc > 0 {
		argvPtr = unsafe.Pointer(&argv[0])
	}

	cRun(moduleName, argc, argvPtr)
}

// Update sends data from Go to the Lua state.
func Update(path string, value interface{}) {
	mu.RLock()
	ready := isReady
	mu.RUnlock()
	if !ready {
		fmt.Printf("[JTK Go] Warning: Update '%s' skipped (Lua not ready)\n", path)
		return
	}

	switch v := value.(type) {
	case bool:
		cStateUpdate(path, TypeBool, unsafe.Pointer(&v))

	case int:
		val := float64(v)
		cStateUpdate(path, TypeNumber, unsafe.Pointer(&val))

	case float64:
		cStateUpdate(path, TypeNumber, unsafe.Pointer(&v))

	case string:
		// Slightly safer GC memory allocation using native append
		b := append([]byte(v), 0)
		cStateUpdate(path, TypeString, unsafe.Pointer(&b[0]))

	case nil:
		cStateUpdate(path, TypeNil, nil)

	default:
		fmt.Printf("[JTK Go] Unsupported type for path '%s'\n", path)
	}
}

// SetAssets извлекает файлы из embed.FS и передает их в JTK (purego).
func SetAssets(efs embed.FS) {
	fs.WalkDir(efs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := efs.ReadFile(path)
		if err != nil {
			return nil
		}

		// Строки для C должны заканчиваться нулевым байтом '\0'
		cPath := append([]byte(path), 0)

		// pinning (удержание ссылок в глобальной переменной)
		pinnedAssetData = append(pinnedAssetData, cPath, data)

		pinnedAssetsSlice = append(pinnedAssetsSlice, cEmbeddedAsset{
			Path: &cPath[0],
			Data: &data[0],
			Size: uintptr(len(data)),
		})

		return nil
	})

	// Нулевой (пустой) элемент в конец массива для безопасности C API
	pinnedAssetsSlice = append(pinnedAssetsSlice, cEmbeddedAsset{})

	// Передаём в C
	cSetAssets(unsafe.Pointer(&pinnedAssetsSlice[0]))
}
