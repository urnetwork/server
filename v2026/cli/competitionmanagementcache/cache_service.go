//go:build linux

// One persistent, bounded parent keeps the ordinary hierarchy unchanged.
// Its finite pass children inherit the same service cgroup and journal budget.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// A join is a positive process-wait acknowledgment, never a timeout estimate.
type cacheServicePassOutcome struct {
	Result  *cacheResult
	Started bool
	Joined  bool
	Err     error
}

// Tests replace only the pass owner; native execution has no alternate target.
type cacheServicePassRunner interface {
	Run(context.Context) cacheServicePassOutcome
}

// Each bounded liveness record contains finite public counters, not raw errors.
type cacheServiceEvent struct {
	Schema       int          `json:"schema"`
	Status       string       `json:"status"`
	ChildStarted bool         `json:"child_started"`
	ChildJoined  bool         `json:"child_joined"`
	Pass         *cacheResult `json:"pass,omitempty"`
}

// A failed/uncertain pass holds this owner alive, preserving its cgroup. Only
// an operator stop ends that hold; no failure authorizes another automatic pass.
func superviseCacheService(ctx context.Context, settings cacheSettings, runner cacheServicePassRunner, output io.Writer, wait func(context.Context, time.Duration) error) error {
	if ctx == nil || settings.validate() != nil || runner == nil || output == nil || wait == nil {
		return errors.New("stable cache service has no finite owner")
	}
	encoder := json.NewEncoder(output)
	for ctx.Err() == nil {
		outcome := runner.Run(ctx)
		event := cacheServiceEvent{Schema: 1, Status: "pass_joined", ChildStarted: outcome.Started, ChildJoined: outcome.Joined, Pass: outcome.Result}
		failed := outcome.Err != nil || !outcome.Started || !outcome.Joined || outcome.Result == nil
		if failed {
			event.Status = "pass_failed_held"
			if !outcome.Joined {
				event.Status = "pass_unjoined_held"
			}
		}
		if err := encoder.Encode(event); err != nil {
			return errors.New("stable cache service cannot publish its finite status")
		}
		if failed {
			<-ctx.Done()
			if !outcome.Joined {
				return errors.New("stable cache service retained unjoined pass custody")
			}
			return nil
		}
		if err := wait(ctx, settings.Interval); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("stable cache service interval owner failed")
		}
	}
	return nil
}

// Scheduling starts only after positive join, so slow work cannot accumulate.
func waitCacheService(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// The process factory is a test seam, never a settings field or CLI argument.
type nativeCacheServicePass struct {
	settings   cacheSettings
	newCommand func() *exec.Cmd
}

// /proc/self/exe retains this exact root-owned installed image even if an
// operator later replaces its pathname. No shell, systemd unit or client runs.
func newNativeCacheServicePass(settings cacheSettings, settingsPath string) (*nativeCacheServicePass, error) {
	if settings.validate() != nil {
		return nil, errors.New("stable cache service settings are invalid")
	}
	directory, err := openProtectedCacheDirectory(filepath.Dir(cacheInstalledExecutable))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fd, err := syscall.Openat(int(directory.Fd()), filepath.Base(cacheInstalledExecutable), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	installed := os.NewFile(uintptr(fd), "installed-cache-owner")
	defer installed.Close()
	info, err := installed.Stat()
	current, currentErr := os.Stat("/proc/self/exe")
	if err != nil || currentErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 || !os.SameFile(info, current) {
		return nil, errors.New("stable cache owner differs from its protected installed executable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Nlink != 1 {
		return nil, errors.New("stable cache executable is not exclusive root authority")
	}
	arguments := []string{"--once"}
	if settingsPath != "" {
		arguments = append(arguments, "--settings="+settingsPath)
	}
	return &nativeCacheServicePass{settings: settings, newCommand: func() *exec.Cmd {
		command := exec.Command("/proc/self/exe", arguments...)
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		return command
	}}, nil
}

// Only one fixed --once child is issued. Its grace and hard-kill/join phases
// share one stop budget; no unjoined outcome permits another automatic pass.
func (self *nativeCacheServicePass) Run(parent context.Context) cacheServicePassOutcome {
	outcome := cacheServicePassOutcome{Joined: true}
	if parent == nil || self == nil || self.settings.validate() != nil || self.newCommand == nil {
		outcome.Err = errors.New("cache pass has no native bounded process owner")
		return outcome
	}
	if err := parent.Err(); err != nil {
		outcome.Err = err
		return outcome
	}
	command := self.newCommand()
	if command == nil {
		outcome.Err = errors.New("cache pass has no fixed command")
		return outcome
	}
	output := &cacheServiceOutput{maximum: self.settings.DocumentBytes}
	command.Stdin, command.Stdout, command.Stderr = nil, output, io.Discard
	command.WaitDelay = self.settings.StopTimeout
	if err := command.Start(); err != nil {
		outcome.Err = err
		return outcome
	}
	outcome.Started, outcome.Joined = true, false
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finish := func(waitErr, cause error) cacheServicePassOutcome {
		outcome.Joined = true
		outcome.Result, outcome.Err = decodeCacheServicePass(output)
		outcome.Err = errors.Join(outcome.Err, waitErr, cause)
		return outcome
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.PassTimeout)
	defer cancel()
	select {
	case err := <-done:
		return finish(err, nil)
	case <-ctx.Done():
	}
	_ = command.Process.Signal(syscall.SIGTERM)
	grace := self.settings.StopTimeout / 2
	graceTimer := time.NewTimer(grace)
	defer graceTimer.Stop()
	select {
	case err := <-done:
		return finish(err, ctx.Err())
	case <-graceTimer.C:
	}
	_ = command.Process.Kill()
	joinTimer := time.NewTimer(self.settings.StopTimeout - grace)
	defer joinTimer.Stop()
	select {
	case err := <-done:
		return finish(err, ctx.Err())
	case <-joinTimer.C:
		outcome.Err = errors.New("cache pass termination has no positive join acknowledgment")
		return outcome
	}
}

// Flooded child output cannot grow a root owner or become a private error log.
type cacheServiceOutput struct {
	lock     sync.Mutex
	maximum  int64
	content  []byte
	exceeded bool
}

// Discarding excess output still lets the process join; it never validates it.
func (self *cacheServiceOutput) Write(content []byte) (int, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	remaining := max(int64(0), self.maximum-int64(len(self.content)))
	if int64(len(content)) > remaining {
		self.exceeded = true
	}
	self.content = append(self.content, content[:min(int64(len(content)), remaining)]...)
	return len(content), nil
}

// The parent's published vocabulary is finite even on malformed child output.
func decodeCacheServicePass(output *cacheServiceOutput) (*cacheResult, error) {
	output.lock.Lock()
	defer output.lock.Unlock()
	if output.exceeded {
		return nil, errors.New("cache pass output exceeded its fixed document bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(output.content))
	decoder.DisallowUnknownFields()
	result := &cacheResult{}
	if err := decoder.Decode(result); err != nil {
		return nil, errors.New("cache pass output is not its finite result")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("cache pass output has more than one result")
	}
	switch result.Status {
	case "invalid_owner", "journal_unavailable", "initial_observation_unavailable", "initial_evidence_invalid", "target_observed", "above_trigger",
		"episode_budget_exhausted", "inactive_clean_cache_unavailable", "pre_issue_debit_unavailable", "post_observation_unavailable", "target_pending",
		"journal_authority_unavailable", "fixed_kernel_authority_unavailable":
		return result, nil
	default:
		return nil, errors.New("cache pass output has an unknown status")
	}
}
