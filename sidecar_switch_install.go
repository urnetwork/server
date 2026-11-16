package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
)

// One part of a process that is installed exactly while a sidecar.yml switch
// reads on (§19.11, D-OPS-10; QD-846, question tls-m5-17-certs-host-install):
// certs' routes in api and its chains in taskworker follow sidecar.yml's certs
// switch this way, so the engine is built, and its vault files read, only once
// the owner turns the switch on, and it is taken down again when the switch
// goes off.
//
//   - off, the part is not built: nothing is read from vault, and whatever
//     consults the part finds nothing installed and answers unavailable;
//   - on, the part is built on a context of its own. A build that fails leaves
//     it uninstalled, says why once, and is tried again at each step, so a file
//     that arrives later installs the part with no restart;
//   - off again, the part is removed and its context ended. A later on builds
//     it anew, from the files as they are then.
//
// The switch is read through the process's reader, so it moves within the
// reader's reread and one PollInterval of a config push, and a broken or
// absent file answers what the reader answers: off on a first read, the last
// good read after one.
//
// A failure's text is written to the log as the build gave it, so a build
// names files and parts in its errors and never a value it read.

// Settings of a switch's install.
type SidecarSwitchInstallSettings struct {
	// How often the switch is read again, and a failed build tried again. The
	// reader rereads sidecar.yml every 60 s, so this is what an install adds
	// to that. Non-positive steps once, at the start.
	PollInterval time.Duration
}

func DefaultSidecarSwitchInstallSettings() *SidecarSwitchInstallSettings {
	return &SidecarSwitchInstallSettings{
		PollInterval: 5 * time.Second,
	}
}

// Builds what a switch installs on ctx, which ends when the part is removed,
// and answers what removes it. An error leaves nothing installed, and its text
// goes to the log.
type SidecarSwitchInstallFunction func(ctx context.Context) (uninstall func(), err error)

// See the file header. Safe for concurrent use.
type SidecarSwitchInstall struct {
	// what the log calls the part
	name     string
	on       func() bool
	install  SidecarSwitchInstallFunction
	settings *SidecarSwitchInstallSettings
	// where a step's lines go: glog, or a test's
	log func(format string, arguments ...any)

	// one step at a time; a second caller answers the state as it is
	stepping atomic.Bool

	stateLock sync.Mutex
	installed bool
	// ends the installed part's context
	cancel    context.CancelFunc
	uninstall func()
	// the last failure said, so a build that keeps failing says it once
	failure string
}

// An install of the part name that follows on, sidecar.yml's switch as the
// process reads it. Nothing is built until the first Sync. settings may be nil
// for the defaults.
func NewSidecarSwitchInstall(name string, on func() bool, install SidecarSwitchInstallFunction, settings *SidecarSwitchInstallSettings) *SidecarSwitchInstall {
	if settings == nil {
		settings = DefaultSidecarSwitchInstallSettings()
	}
	return &SidecarSwitchInstall{
		name:     name,
		on:       on,
		install:  install,
		settings: settings,
		log:      glog.Infof,
	}
}

// Sends the install's log lines to log in place of glog, for a test that
// reads what a step said. Call it before the first Sync.
func (self *SidecarSwitchInstall) Testing_SetLog(log func(format string, arguments ...any)) {
	self.log = log
}

// Whether the part is installed now.
func (self *SidecarSwitchInstall) Installed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.installed
}

// One step: builds the part when the switch is on and it is not installed,
// removes it when the switch is off and it is. Answers whether it is installed
// after the step. A step that finds another running answers the state as it
// is.
func (self *SidecarSwitchInstall) Sync(ctx context.Context) bool {
	if !self.stepping.CompareAndSwap(false, true) {
		return self.Installed()
	}
	defer self.stepping.Store(false)
	on := self.on()
	installed := self.Installed()
	switch {
	case on && !installed:
		self.build(ctx)
	case !on && installed:
		self.remove()
		self.log("[%s]sidecar.yml's switch is off: removed\n", self.name)
	case !on:
		// a failure said while the switch was on is said again at the next on
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.failure = ""
		}()
	}
	return self.Installed()
}

// Builds the part and installs it, or says why not when that differs from the
// last failure said.
func (self *SidecarSwitchInstall) build(ctx context.Context) {
	installCtx, cancel := context.WithCancel(ctx)
	uninstall, err := self.install(installCtx)
	if err != nil {
		cancel()
		text := err.Error()
		said := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.failure == text {
				return true
			}
			self.failure = text
			return false
		}()
		if !said {
			self.log("[%s]sidecar.yml's switch is on and it stays uninstalled: %s\n", self.name, text)
		}
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.installed = true
		self.cancel = cancel
		self.uninstall = uninstall
		self.failure = ""
	}()
	self.log("[%s]sidecar.yml's switch is on: installed\n", self.name)
}

// Removes the part when it is installed.
func (self *SidecarSwitchInstall) remove() {
	cancel, uninstall := func() (context.CancelFunc, func()) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		cancel, uninstall := self.cancel, self.uninstall
		self.installed = false
		self.cancel = nil
		self.uninstall = nil
		return cancel, uninstall
	}()
	if uninstall != nil {
		uninstall()
	}
	if cancel != nil {
		cancel()
	}
}

// Removes the part, whatever the switch says. A later Sync builds it again
// while the switch is on.
func (self *SidecarSwitchInstall) Close() {
	self.remove()
}

// Steps at once and then every PollInterval until ctx ends, then removes the
// part. Returns when it has.
func (self *SidecarSwitchInstall) Run(ctx context.Context) {
	defer self.Close()
	self.Sync(ctx)
	if self.settings.PollInterval <= 0 {
		<-ctx.Done()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(self.settings.PollInterval):
		}
		if ctx.Err() != nil {
			return
		}
		self.Sync(ctx)
	}
}

// A file an install is built from is not there or cannot be read. The text
// names the mount and the file, never what the file holds.
var ErrSidecarInstallFile = errors.New("install file")

// A whole file an install is built from, by name, from one of the process's
// mounts: Config, called "config", or Vault, called "vault". The error says
// whether the mount holds no such file or holds one that cannot be read, and
// never carries the cause, which may quote the file.
func ReadSidecarInstallFile(resolver *Resolver, mount string, name string) ([]byte, error) {
	resource, err := resolver.SimpleResource(name)
	if err != nil {
		if errors.Is(err, ErrResourceNotFound) && !errors.Is(err, ErrResourceUnavailable) {
			return nil, fmt.Errorf("%w: %s holds no %s", ErrSidecarInstallFile, mount, name)
		}
		return nil, fmt.Errorf("%w: %s's %s cannot be read", ErrSidecarInstallFile, mount, name)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%w: %s's %s cannot be read", ErrSidecarInstallFile, mount, name)
	}
	return data, nil
}
