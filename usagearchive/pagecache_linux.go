package usagearchive

import (
	"os"

	"golang.org/x/sys/unix"
)

// Drops the file's clean pages from the page cache, so the read that follows
// comes from storage rather than from what was just written.
func dropCachedPages(file *os.File) {
	_ = unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED)
}
