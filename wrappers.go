package jtk

import (
	"reflect"
	"strings"
	"sync"
	"unsafe"
)

var (
	allWatchers []*Watcher // A list to hold all created watchers
	watchersMu  sync.Mutex // A mutex to protect the list
)

// =================================================================================
// HIGH LEVEL API: Var Wrapper (Thread-Safe, Type-Safe)
// =================================================================================

// Var is a thread-safe wrapper generic type.
type Var[T any] struct {
	Path  string
	value T
	mu    sync.RWMutex
}

// Get returns the value from the local cache immediately.
func (v *Var[T]) Get() T {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.value
}

// Set sends the value to Lua and updates the local cache.
func (v *Var[T]) Set(val T) {
	v.mu.Lock()
	v.value = val
	v.mu.Unlock()
	Update(v.Path, val)
}

// On registers a typed callback.
func (v *Var[T]) On(callback func(val T)) {
	// Register the low-level listener on the path string
	Listen(v.Path, func(raw interface{}) {
		val := convertTo[T](raw)

		v.mu.Lock()
		v.value = val
		v.mu.Unlock()

		// Allow internal cache update above, but suppress user callback if Lua is still dumping its initial state
		if IsReady() {
			callback(val)
		}
	})
}

// BindWrappers populates struct fields of type *Var[T].
// prefix: e.g., "app.settings". If empty, field name is used.
func BindWrappers(ptr interface{}, prefix string) {
	val := reflect.ValueOf(ptr).Elem()
	typ := val.Type()

	if prefix != "" && !strings.HasSuffix(prefix, ".") {
		prefix += "."
	}

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		// We assume that *Var[T] is used if it's a pointer and the type name suggests it.
		// Note: Checking string representation is a bit brittle but simple for generics here.
		typeStr := field.Type().String()
		if field.Kind() == reflect.Ptr && strings.Contains(typeStr, ".Var[") {

			// Determine Lua path
			path := fieldType.Tag.Get("jtk")
			if path == "" {
				path = strings.ToLower(fieldType.Name)
			}
			fullPath := prefix + path

			// Initialize the *Var[T] struct
			if field.IsNil() {
				newVar := reflect.New(field.Type().Elem())
				field.Set(newVar)
			}

			// 1. Set the 'Path' field (exported)
			field.Elem().FieldByName("Path").SetString(fullPath)

			// 2. Set up internal listener to keep the cache updated even without user .On calls.
			// We need to access the private fields 'value' and 'mu' via reflection + unsafe
			// because they are unexported.

			const (
				fieldNameValue = "value"
				fieldNameMu    = "mu"
			)

			// Capture values for closure
			vStruct := field.Elem() // This is the Struct Var[T], not pointer

			// Register global listener
			Listen(fullPath, func(raw interface{}) {
				// Access private 'value' using unsafe
				rfVal := vStruct.FieldByName(fieldNameValue)
				rfValPtr := reflect.NewAt(rfVal.Type(), unsafe.Pointer(rfVal.UnsafeAddr())).Elem()

				// Convert to target type T
				converted := convertReflect(raw, rfVal.Type())

				// Access private 'mu' (RWMutex)
				rfMu := vStruct.FieldByName(fieldNameMu)
				muPtr := reflect.NewAt(rfMu.Type(), unsafe.Pointer(rfMu.UnsafeAddr())).Interface().(*sync.RWMutex)

				muPtr.Lock()
				rfValPtr.Set(converted)
				muPtr.Unlock()
			})
		}
	}
}

// =================================================================================
// HIGH LEVEL API: Structure Watcher (Dirty Checking)
// =================================================================================

// Watcher manages binding between a Go struct and Lua state using dirty checking.
type Watcher struct {
	Target interface{} // Pointer to the struct
	Prefix string
	shadow map[string]interface{} // Shadow copy to track changes
	mu     sync.Mutex
}

// BindStruct creates a Watcher for a plain struct.
func BindStruct(prefix string, targetPtr interface{}) *Watcher {
	w := &Watcher{
		Target: targetPtr,
		Prefix: prefix,
		shadow: make(map[string]interface{}),
	}
	if w.Prefix != "" && !strings.HasSuffix(w.Prefix, ".") {
		w.Prefix += "."
	}
	w.init()
	// Register the watcher for global sync
	watchersMu.Lock()
	allWatchers = append(allWatchers, w)
	watchersMu.Unlock()
	return w
}

func (w *Watcher) init() {
	val := reflect.ValueOf(w.Target).Elem()
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		structField := typ.Field(i)

		// Skip unexported fields
		if !field.CanSet() {
			continue
		}

		tag := structField.Tag.Get("jtk")
		name := tag
		if name == "" {
			name = strings.ToLower(structField.Name)
		}
		fullPath := w.Prefix + name

		// Initialize shadow copy
		w.shadow[name] = field.Interface()

		// Listen for Lua updates to update Go struct
		Listen(fullPath, func(raw interface{}) {
			w.mu.Lock()
			defer w.mu.Unlock()

			// Convert raw Lua type (often float64) to Go field type
			newVal := convertReflect(raw, field.Type())

			field.Set(newVal)
			w.shadow[name] = newVal.Interface()
		})
	}
}

// Sync checks for changes in the Go struct and pushes them to Lua.
func (w *Watcher) Sync() {
	w.mu.Lock()
	defer w.mu.Unlock()

	val := reflect.ValueOf(w.Target).Elem()
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		if !field.CanSet() {
			continue
		}

		structField := typ.Field(i)
		tag := structField.Tag.Get("jtk")
		name := tag
		if name == "" {
			name = strings.ToLower(structField.Name)
		}
		fullPath := w.Prefix + name

		curr := field.Interface()
		prev, exists := w.shadow[name]

		// If value changed locally, push to Lua and update shadow
		if !exists || curr != prev {
			Update(fullPath, curr)
			w.shadow[name] = curr
		}
	}
}

// On registers a callback. After the callback finishes, Sync() is called automatically.
// This allows modifying the struct inside the callback and having it auto-synced.
func (w *Watcher) On(path string, fn func(val interface{})) {
	Listen(path, func(val interface{}) {
		// Suppress user logic and auto-sync during initialization
		if !IsReady() {
			return
		}

		// 1. Run user logic
		fn(val)

		// 2. Auto-sync ALL watchers, not just the current one
		SyncAll()
	})
}

func SyncAll() {
	watchersMu.Lock()
	defer watchersMu.Unlock()
	for _, w := range allWatchers {
		w.Sync()
	}
}

// =================================================================================
// Helpers
// =================================================================================

// convertTo casts raw interface{} (from Lua) to type T.
func convertTo[T any](raw interface{}) T {
	var zero T

	if raw == nil {
		return zero
	}

	// Fast path: types match
	if casted, ok := raw.(T); ok {
		return casted
	}

	rVal := reflect.ValueOf(raw)
	targetType := reflect.TypeOf(zero)

	// Handle float64 -> int conversion (common with Lua)
	if targetType.Kind() == reflect.Int && rVal.Kind() == reflect.Float64 {
		return any(int(rVal.Float())).(T)
	}

	// Add more conversions here if needed (e.g. float -> string)

	return zero
}

// convertReflect converts raw interface{} to a reflect.Value of targetType.
func convertReflect(raw interface{}, targetType reflect.Type) reflect.Value {
	if raw == nil {
		return reflect.Zero(targetType)
	}

	rVal := reflect.ValueOf(raw)

	if rVal.Type() == targetType {
		return rVal
	}

	// Handle float64 -> int
	if targetType.Kind() == reflect.Int && rVal.Kind() == reflect.Float64 {
		return reflect.ValueOf(int(rVal.Float()))
	}

	return rVal
}
