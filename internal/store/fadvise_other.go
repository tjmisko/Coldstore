//go:build !linux

package store

import "github.com/tjmisko/coldstore/internal/fsx"

// dropPageCache is a no-op where posix_fadvise is unavailable.
var dropPageCache = func(f fsx.File) {}
