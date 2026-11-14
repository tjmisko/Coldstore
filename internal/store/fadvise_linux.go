//go:build linux

package store

import (
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/tjmisko/coldstore/internal/fsx"
)

// dropPageCache asks the kernel to forget the cached pages of f so the next
// read comes from the device and not from memory. It is best effort: a file
// that is not backed by a descriptor, or an fadvise error, is ignored.
var dropPageCache = func(f fsx.File) {
	conn, ok := f.(syscall.Conn)
	if !ok {
		return
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = unix.Fadvise(int(fd), 0, 0, unix.FADV_DONTNEED)
	})
}
