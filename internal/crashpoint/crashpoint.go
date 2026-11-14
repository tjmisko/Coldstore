// Package crashpoint declares named places in the archive core where a test can
// kill the process, so the crash matrix can prove every step of a durable
// operation is recoverable. When nothing is armed, Hit is one atomic load.
package crashpoint

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	EnvPoint = "COLDSTORE_CRASHPOINT"
	EnvHit   = "COLDSTORE_CRASHPOINT_HIT"
)

// Point is a registered crash point.
type Point struct{ name string }

type trigger struct {
	remaining int
	fn        func()
}

var (
	mu         sync.Mutex
	registered = map[string]bool{}
	armed      = map[string]*trigger{}
	anyArmed   atomic.Bool
)

// Register declares a crash point at package init. Names are
// "<package>.<operation>.<step>", e.g. "store.put.staged_synced".
// Registering the same name twice panics at init.
func Register(name string) Point {
	mu.Lock()
	defer mu.Unlock()
	if registered[name] {
		panic(fmt.Sprintf("crashpoint: %q registered twice", name))
	}
	registered[name] = true
	return Point{name: name}
}

// Hit fires the armed function if this is its n-th hit. It is a single atomic
// load when nothing is armed.
func (p Point) Hit() {
	if !anyArmed.Load() {
		return
	}
	mu.Lock()
	t, ok := armed[p.name]
	if !ok {
		mu.Unlock()
		return
	}
	t.remaining--
	if t.remaining != 0 {
		mu.Unlock()
		return
	}
	fn := t.fn
	mu.Unlock()
	// Outside the lock: fn may be Kill, or a test callback that panics.
	fn()
}

// Names returns every registered name, sorted.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(registered))
	for name := range registered {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Arm makes the n-th Hit (1-based) of name call fn. Tests only. It panics when
// name is not registered or n < 1: an arming that can never fire would make a
// crash test pass vacuously.
func Arm(name string, n int, fn func()) {
	mu.Lock()
	defer mu.Unlock()
	if !registered[name] {
		panic(fmt.Sprintf("crashpoint: Arm of unregistered point %q", name))
	}
	if n < 1 {
		panic(fmt.Sprintf("crashpoint: Arm of %q with n=%d; hits are 1-based", name, n))
	}
	armed[name] = &trigger{remaining: n, fn: fn}
	anyArmed.Store(true)
}

// Disarm removes every armed point.
func Disarm() {
	mu.Lock()
	defer mu.Unlock()
	armed = map[string]*trigger{}
	anyArmed.Store(false)
}

// ArmFromEnv arms from COLDSTORE_CRASHPOINT=<name> and optional
// COLDSTORE_CRASHPOINT_HIT=<n>; the armed fn is Kill. It is called ONLY from
// test helper processes, never from cmd/coldstore.
func ArmFromEnv() (armedOK bool, err error) {
	name := os.Getenv(EnvPoint)
	if name == "" {
		return false, nil
	}
	n := 1
	if raw := os.Getenv(EnvHit); raw != "" {
		n, err = strconv.Atoi(raw)
		if err != nil || n < 1 {
			return false, fmt.Errorf("crashpoint: %s=%q is not a positive integer", EnvHit, raw)
		}
	}
	mu.Lock()
	known := registered[name]
	mu.Unlock()
	if !known {
		return false, fmt.Errorf("crashpoint: %s names unknown point %q", EnvPoint, name)
	}
	Arm(name, n, Kill)
	return true, nil
}

// Kill sends SIGKILL to the current process: no deferred calls, no flushes.
func Kill() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	// SIGKILL to self is delivered before the syscall returns on Linux; this is
	// only reached if the signal could not be sent, and must still not return.
	os.Exit(137)
}
