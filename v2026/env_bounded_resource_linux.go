//go:build linux

// Bounded public-policy readers preserve resolver selection and overrides while
// admitting the actual opened descriptor before allocating or reading bytes.
package server

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"syscall"

	"golang.org/x/sys/unix"
)

type boundedResourceHooks struct {
	afterOpen func(*os.File)
	afterRead func()
}

// BytesBoundedE reads one selected public resource under its caller's limit and
// lifecycle. Projected config symlinks remain supported; their opened target
// must be regular. A raced FIFO is opened nonblocking and refused before reads.
func (self *SimpleResource) BytesBoundedE(ctx context.Context, maximum int) ([]byte, error) {
	return self.bytesBounded(ctx, maximum, boundedResourceHooks{})
}

// Hooks observe owned boundaries in deterministic fixtures and grant no bypass.
func (self *SimpleResource) bytesBounded(ctx context.Context, maximum int, hooks boundedResourceHooks) (result []byte, resultErr error) {
	if ctx == nil || self == nil || maximum <= 0 || maximum > 16*1024*1024 {
		return nil, errors.New("bounded resource requires owner and a 1–16777216 byte limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			result = nil
		}
	}()
	if self.override != nil {
		if len(self.override) > maximum {
			return nil, errors.New("resource override exceeds the selected byte limit")
		}
		return slices.Clone(self.override), ctx.Err()
	}
	// Preserve resolver symlink semantics, then trust only the actual opened
	// descriptor. NONBLOCK prevents a raced FIFO from parking this owner.
	fd, err := unix.Open(self.path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), self.path)
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > int64(maximum) {
		return nil, errors.New("resource is not a regular file within the selected byte limit")
	}
	if hooks.afterOpen != nil {
		hooks.afterOpen(file)
	}
	result = make([]byte, 0, int(before.Size()))
	var buffer [4096]byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := file.Read(buffer[:])
		if hooks.afterRead != nil {
			hooks.afterRead()
		}
		if n > maximum-len(result) {
			return nil, errors.New("resource grew beyond the selected byte limit")
		}
		result = append(result, buffer[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	final, ok := after.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev != final.Dev || stat.Ino != final.Ino || stat.Mode != final.Mode || stat.Mtim != final.Mtim || stat.Ctim != final.Ctim || after.Size() != before.Size() || int64(len(result)) != before.Size() {
		return nil, errors.New("selected resource changed while reading")
	}
	return result, ctx.Err()
}
