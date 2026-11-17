//go:build linux

// Diagnostics are best effort and never own control cleanup. A lazy native
// duplicate avoids fd1/fd2 SIGPIPE semantics and bounds a stalled pipe/socket;
// unsupported nonpollable sinks are dropped, preserving the operation's exit.
package main

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// Each reporting owner has a total byte/deadline budget, not one per print.
type subevaluationDiagnosticSettings struct {
	MaximumBytes int
	WriteTimeout time.Duration
}

// Managed stdio is a physical pipe/socket; this small allowance adds no time to
// a successful command and is independent from any evaluation's signed clock.
func defaultSubevaluationDiagnosticSettings() subevaluationDiagnosticSettings {
	return subevaluationDiagnosticSettings{MaximumBytes: 4 * 1024, WriteTimeout: time.Second}
}

// Writes/close are serialized. No goroutine owns a pending diagnostic after
// return, and no change to SIGPIPE disposition can leak into native children.
type subevaluationDiagnostic struct {
	stateLock sync.Mutex
	source    *os.File
	owned     *os.File
	settings  subevaluationDiagnosticSettings
	remaining int
	started   bool
	closed    bool
	failed    bool
}

// The source descriptor is untouched until the first diagnostic is attempted,
// so healthy children inherit exactly their original blocking stream flags.
func newSubevaluationDiagnostic(source *os.File, settings subevaluationDiagnosticSettings) *subevaluationDiagnostic {
	return &subevaluationDiagnostic{source: source, settings: settings, remaining: settings.MaximumBytes}
}

// Only the private duplicate is ever closed; the caller retains its stderr.
func (self *subevaluationDiagnostic) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	if self.owned == nil {
		return nil
	}
	err := self.owned.Close()
	self.owned = nil
	return err
}

// The actual write is poller-owned with one absolute native deadline. An EPIPE
// returns normally because this owned file never has descriptor number1 or2.
func (self *subevaluationDiagnostic) Write(content []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(content) == 0 {
		return 0, nil
	}
	if self.closed || self.failed || self.source == nil || self.settings.WriteTimeout <= 0 || self.settings.MaximumBytes <= 0 {
		return 0, errors.New("subevaluation diagnostic unavailable")
	}
	if !self.started {
		self.started = true
		self.owned = duplicateSubevaluationDiagnostic(self.source)
		if self.owned == nil || self.owned.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout)) != nil {
			self.failed = true
			if self.owned != nil {
				_ = self.owned.Close()
				self.owned = nil
			}
			return 0, errors.New("subevaluation diagnostic cannot own a native deadline")
		}
	}
	if self.remaining <= 0 {
		return 0, io.ErrShortWrite
	}
	selected := content
	if len(selected) > self.remaining {
		selected = selected[:self.remaining]
	}
	n, err := self.owned.Write(selected)
	self.remaining -= n
	if err != nil {
		self.failed = true
	}
	if err == nil && n != len(content) {
		err = io.ErrShortWrite
	}
	return n, err
}

// Native nonblocking registration is possible only for inherited pipes/sockets.
// The shared flag change occurs only when printing, never at construction.
func duplicateSubevaluationDiagnostic(source *os.File) *os.File {
	connection, err := source.SyscallConn()
	if err != nil {
		return nil
	}
	fd := -1
	if err := connection.Control(func(original uintptr) {
		var info syscall.Stat_t
		if syscall.Fstat(int(original), &info) != nil || info.Mode&syscall.S_IFMT != syscall.S_IFIFO && info.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
			return
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_GETFL, 0)
		if errno != 0 || int(flags)&syscall.O_ACCMODE == syscall.O_RDONLY {
			return
		}
		duplicate, _, errno := syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_DUPFD_CLOEXEC, 3)
		if errno == 0 {
			fd = int(duplicate)
		}
	}); err != nil || fd < 3 {
		if fd >= 3 {
			_ = syscall.Close(fd)
		}
		return nil
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil
	}
	return os.NewFile(uintptr(fd), "subevaluation-private-diagnostic")
}
