// Descriptor metadata/read behavior is synthetic. No host executable is read,
// modified, replaced, or invoked by these exact-boundary controls.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// A Linux FileInfo snapshot is constructed from visibly synthetic authority.
type nativeQualificationExecutableTestInfo struct {
	size  int64
	mode  os.FileMode
	mtime time.Time
	stat  syscall.Stat_t
}

// FileInfo methods expose immutable snapshots to the real hashing algorithm.
func (self nativeQualificationExecutableTestInfo) Name() string       { return "synthetic-executable" }
func (self nativeQualificationExecutableTestInfo) Size() int64        { return self.size }
func (self nativeQualificationExecutableTestInfo) Mode() os.FileMode  { return self.mode }
func (self nativeQualificationExecutableTestInfo) ModTime() time.Time { return self.mtime }
func (self nativeQualificationExecutableTestInfo) IsDir() bool        { return false }
func (self nativeQualificationExecutableTestInfo) Sys() any           { value := self.stat; return &value }

// The private seam controls exact before/after fstat and read/cancel ordering.
type nativeQualificationExecutableTestFile struct {
	reader      *bytes.Reader
	before      nativeQualificationExecutableTestInfo
	after       nativeQualificationExecutableTestInfo
	statCalls   int
	statErrorAt int
	statError   error
	readCalls   int
	readErr     error
	afterRead   func()
}

// Stat generations are deterministic, not a racy filesystem replacement.
func (self *nativeQualificationExecutableTestFile) Stat() (os.FileInfo, error) {
	self.statCalls++
	if self.statCalls == self.statErrorAt {
		return nil, self.statError
	}
	if self.statCalls == 1 {
		return self.before, nil
	}
	return self.after, nil
}

// A hook acts only after acknowledged bytes, permitting exact cancellation.
func (self *nativeQualificationExecutableTestFile) Read(data []byte) (int, error) {
	self.readCalls++
	if self.readErr != nil {
		return 0, self.readErr
	}
	n, err := self.reader.Read(data)
	if self.afterRead != nil {
		self.afterRead()
	}
	return n, err
}

// Actual bytes are synthetic and independently pinned in this test only.
func nativeQualificationExecutableTestValues(data []byte) (*nativeQualificationExecutableTestFile, Pin) {
	info := nativeQualificationExecutableTestInfo{size: int64(len(data)), mode: 0o500, mtime: time.Unix(0, 0), stat: syscall.Stat_t{Dev: 1, Ino: 2, Nlink: 1, Uid: 0, Gid: 0, Mode: 0o100500}}
	return &nativeQualificationExecutableTestFile{reader: bytes.NewReader(data), before: info, after: info}, Pin{Path: "/opt/synthetic-qualification/probe", Sha256: digestDockerBytes(data)}
}

// An exact limit succeeds; one-byte overflow refuses before any body read.
func TestNativeQualificationExecutableHashesExactBoundAndRefusesOverflow(t *testing.T) {
	data := []byte("synthetic executable bytes")
	file, pin := nativeQualificationExecutableTestValues(data)
	current := func() (os.FileInfo, error) { return file.after, nil }
	count, err := hashNativeQualificationExecutable(context.Background(), file, current, pin, int64(len(data)), true)
	if err != nil || count != int64(len(data)) {
		t.Fatal("exact executable bound failed", count, err)
	}
	file, pin = nativeQualificationExecutableTestValues(data)
	if _, err := hashNativeQualificationExecutable(context.Background(), file, func() (os.FileInfo, error) { return file.after, nil }, pin, int64(len(data)-1), true); err == nil || file.readCalls != 0 {
		t.Fatal("oversize executable was read or accepted", err)
	}
}

// The path's final metadata must match the final descriptor as well as its
// inode: mode/uid/gid/size/ctime/nlink changes after fstat are all observable.
func TestNativeQualificationExecutableRefusesDescriptorAndSameInodePathMutation(t *testing.T) {
	for _, mutate := range []func(*nativeQualificationExecutableTestInfo){
		func(info *nativeQualificationExecutableTestInfo) { info.stat.Ino++ },
		func(info *nativeQualificationExecutableTestInfo) { info.size++ },
		func(info *nativeQualificationExecutableTestInfo) { info.mode = 0o700; info.stat.Mode = 0o100700 },
		func(info *nativeQualificationExecutableTestInfo) { info.mtime = info.mtime.Add(time.Second) },
		func(info *nativeQualificationExecutableTestInfo) { info.stat.Ctim.Sec++ },
		func(info *nativeQualificationExecutableTestInfo) { info.stat.Nlink = 2 },
		func(info *nativeQualificationExecutableTestInfo) { info.stat.Uid = 12345 },
		func(info *nativeQualificationExecutableTestInfo) { info.stat.Gid = 12346 },
	} {
		for _, pathOnly := range []bool{false, true} {
			file, pin := nativeQualificationExecutableTestValues([]byte("synthetic executable"))
			current := file.after
			if pathOnly {
				mutate(&current)
			} else {
				mutate(&file.after)
				current = file.after
			}
			if _, err := hashNativeQualificationExecutable(context.Background(), file, func() (os.FileInfo, error) { return current, nil }, pin, 1024, true); err == nil {
				t.Fatal("changed descriptor/current-path metadata qualified", pathOnly)
			}
		}
	}
}

// Cancellation and original read/stat errors retain their identity; neither a
// short EOF nor a late partial read produces a successful executable pin.
func TestNativeQualificationExecutablePreservesCancellationShortReadAndErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	file, pin := nativeQualificationExecutableTestValues([]byte("synthetic executable"))
	file.afterRead = cancel
	if _, err := hashNativeQualificationExecutable(ctx, file, func() (os.FileInfo, error) { return file.after, nil }, pin, 1024, true); !errors.Is(err, context.Canceled) {
		t.Fatal("read cancellation attribution lost", err)
	}
	sentinel := errors.New("synthetic descriptor read error")
	file, pin = nativeQualificationExecutableTestValues([]byte("synthetic executable"))
	file.readErr = sentinel
	if _, err := hashNativeQualificationExecutable(context.Background(), file, func() (os.FileInfo, error) { return file.after, nil }, pin, 1024, true); !errors.Is(err, sentinel) {
		t.Fatal("original read error lost", err)
	}
	file, pin = nativeQualificationExecutableTestValues([]byte("synthetic executable"))
	file.reader = bytes.NewReader([]byte("short"))
	if _, err := hashNativeQualificationExecutable(context.Background(), file, func() (os.FileInfo, error) { return file.after, nil }, pin, 1024, true); err == nil {
		t.Fatal("short descriptor body passed")
	}
	file, pin = nativeQualificationExecutableTestValues([]byte("synthetic executable"))
	if _, err := hashNativeQualificationExecutable(context.Background(), file, func() (os.FileInfo, error) { return nil, io.ErrUnexpectedEOF }, pin, 1024, true); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("original path-stat error lost", err)
	}
	for _, at := range []int{1, 2} {
		file, pin = nativeQualificationExecutableTestValues([]byte("synthetic executable"))
		file.statErrorAt = at
		file.statError = sentinel
		if _, err := hashNativeQualificationExecutable(context.Background(), file, func() (os.FileInfo, error) { return file.after, nil }, pin, 1024, true); !errors.Is(err, sentinel) || at == 1 && file.readCalls != 0 {
			t.Fatal("original first/final fstat error lost", at, err)
		}
	}
}

// Unsafe initial authority and a pre-canceled owner perform zero body reads.
func TestNativeQualificationExecutableRefusesUnsafeAuthorityBeforeRead(t *testing.T) {
	for _, mutate := range []func(*nativeQualificationExecutableTestInfo){
		func(info *nativeQualificationExecutableTestInfo) { info.mode = 0o400; info.stat.Mode = 0o100400 },
		func(info *nativeQualificationExecutableTestInfo) { info.mode = 0o520; info.stat.Mode = 0o100520 },
		func(info *nativeQualificationExecutableTestInfo) { info.stat.Nlink = 2 },
		func(info *nativeQualificationExecutableTestInfo) { info.stat.Uid = 12345 },
	} {
		file, pin := nativeQualificationExecutableTestValues([]byte("synthetic executable"))
		mutate(&file.before)
		if _, err := hashNativeQualificationExecutable(context.Background(), file, func() (os.FileInfo, error) { return file.after, nil }, pin, 1024, true); err == nil || file.readCalls != 0 {
			t.Fatal("unsafe initial authority was consumed", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file, pin := nativeQualificationExecutableTestValues([]byte("synthetic executable"))
	if _, err := hashNativeQualificationExecutable(ctx, file, func() (os.FileInfo, error) { return file.after, nil }, pin, 1024, true); !errors.Is(err, context.Canceled) || file.statCalls != 0 || file.readCalls != 0 {
		t.Fatal("pre-canceled hash touched descriptor", err)
	}
}
