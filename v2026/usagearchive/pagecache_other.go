//go:build !linux

package usagearchive

import "os"

// Elsewhere the read back may come from the page cache.
func dropCachedPages(file *os.File) {}
