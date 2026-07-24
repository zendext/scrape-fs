package diagnostics

import "sync"

type runtimeComponent struct {
	path      string
	available bool
	check     func() error
}

type runtimeSnapshot struct {
	ready  bool
	mount  runtimeComponent
	socket runtimeComponent
}

type Runtime struct {
	mu     sync.RWMutex
	ready  bool
	mount  runtimeComponent
	socket runtimeComponent
}

func NewRuntime(mountPath, socketPath string) *Runtime {
	return &Runtime{
		mount:  runtimeComponent{path: mountPath},
		socket: runtimeComponent{path: socketPath},
	}
}

func (runtime *Runtime) SetMount(check func() error) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.mount.available = true
	runtime.mount.check = check
}

func (runtime *Runtime) SetSocket(check func() error) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.socket.available = true
	runtime.socket.check = check
}

func (runtime *Runtime) SetReady(ready bool) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.ready = ready
}

func (runtime *Runtime) snapshot() runtimeSnapshot {
	if runtime == nil {
		return runtimeSnapshot{}
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtimeSnapshot{
		ready:  runtime.ready,
		mount:  runtime.mount,
		socket: runtime.socket,
	}
}
