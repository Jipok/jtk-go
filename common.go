package jtk

import (
	"fmt"
	"sync"
)

// Callback function type for listeners
type Callback func(interface{})

// =================================================================================
// Shared Globals (Available to both implementations)
// =================================================================================

var (
	listeners = make(map[string][]Callback)
	mu        sync.RWMutex
	isReady   bool
	readyChan = make(chan struct{})
)

// =================================================================================
// Shared API
// =================================================================================

// IsReady checks if the Lua state is fully initialized via a thread-safe lock.
func IsReady() bool {
	mu.RLock()
	defer mu.RUnlock()
	return isReady
}

// WaitUntilReady blocks the calling goroutine until the JTK state is initialized.
func WaitUntilReady() {
	<-readyChan
}

// Listen registers a callback for a specific state path.
func Listen(path string, callback Callback) {
	mu.Lock()
	defer mu.Unlock()
	listeners[path] = append(listeners[path], callback)
}

// dispatchEvent is used by both implementations to notify listeners
func dispatchEvent(path string, value interface{}) {
	mu.RLock()
	defer mu.RUnlock()

	if funcs, found := listeners[path]; found {
		for _, fn := range funcs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("[JTK Error] Panic in listener for '%s': %v\n", path, r)
					}
				}()
				fn(value)
			}()
		}
	}
}
