package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSshLeaseLifetimeParsingPreservesDefaultAndBounds(t *testing.T) {
	for _, testCase := range []struct {
		args []string
		want time.Duration
	}{
		{[]string{"ssh-lease", "/synthetic/request"}, 240 * time.Second},
		{[]string{"ssh-lease", "/synthetic/request", "--lifetime-seconds", "1"}, time.Second},
		{[]string{"ssh-lease", "/synthetic/request", "--lifetime-seconds", "1880"}, 1880 * time.Second},
		{[]string{"ssh-lease", "/synthetic/request", "--lifetime-seconds", "1900"}, 1900 * time.Second},
	} {
		path, lifetime, err := parseSshLeaseInvocation(testCase.args)
		if err != nil || path != "/synthetic/request" || lifetime != testCase.want {
			t.Fatal("lease invocation changed its bounded lifetime")
		}
	}
	for _, value := range []string{"", "0", "-1", "1901", "99999999999999999999999999", "+240", "0240", "1.5", "240s", " 240", "240 "} {
		args := []string{"ssh-lease", "/synthetic/request", "--lifetime-seconds", value}
		if _, _, err := parseSshLeaseInvocation(args); err == nil {
			t.Fatal("invalid lease lifetime accepted")
		}
	}
	for _, args := range [][]string{
		nil, {}, {"ssh-lease"}, {"wrong", "/synthetic/request"}, {"ssh-lease", ""},
		{"ssh-lease", "/synthetic/request", "1900"},
		{"ssh-lease", "/synthetic/request", "--unknown", "1900"},
		{"ssh-lease", "/synthetic/request", "--lifetime-seconds", "1900", "extra"},
	} {
		called := false
		err := runSshLeaseCommand(context.Background(), args, strings.NewReader(""), io.Discard,
			func(context.Context, string, io.Reader, io.Writer) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("malformed invocation reached the lease owner")
		}
	}
}

func TestSshLeaseLongOperationIncludesAdmissionWorkAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		owner, stop := context.WithTimeout(context.Background(), 1900*time.Second)
		defer stop()
		joined := false
		var observed context.Context
		err := runSshLeaseCommand(owner, []string{"ssh-lease", "/synthetic/request", "--lifetime-seconds", "1900"}, strings.NewReader("release\n"), io.Discard,
			func(ctx context.Context, path string, input io.Reader, output io.Writer) error {
				observed = ctx
				if path != "/synthetic/request" {
					t.Fatal("request binding changed")
				}
				// Same finite envelope: queue30 + setup40 + CLI1800 + cleanup15.
				for _, duration := range []time.Duration{30 * time.Second, 40 * time.Second, 1800 * time.Second, 15 * time.Second} {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(duration):
					}
				}
				message, readErr := io.ReadAll(input)
				joined = readErr == nil && string(message) == "release\n"
				return readErr
			})
		if err != nil || !joined {
			t.Fatal("helper deadline interrupted an operation inside its existing owner envelope")
		}
		if elapsed := time.Since(started); elapsed != 1885*time.Second {
			t.Fatal("test did not include queue, setup, full work and cleanup")
		}
		if observed == nil || !errors.Is(observed.Err(), context.Canceled) || owner.Err() != nil {
			t.Fatal("normal joined return did not cancel only the helper context")
		}
	})
}

func TestSshLeaseDefaultStillExpiresAt240Seconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		released := false
		err := runSshLeaseCommand(context.Background(), []string{"ssh-lease", "/synthetic/request"}, strings.NewReader(""), io.Discard,
			func(ctx context.Context, _ string, _ io.Reader, _ io.Writer) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(1800 * time.Second):
					released = true
					return nil
				}
			})
		if !errors.Is(err, context.DeadlineExceeded) || released || time.Since(started) != 240*time.Second {
			t.Fatal("legacy helper no longer preserves its 240-second bound")
		}
	})
}

func TestSshLeaseLifetimeNeverExtendsParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		owner, stop := context.WithTimeout(context.Background(), 80*time.Second)
		defer stop()
		err := runSshLeaseCommand(owner, []string{"ssh-lease", "/synthetic/request", "--lifetime-seconds", "1900"}, strings.NewReader(""), io.Discard,
			func(ctx context.Context, _ string, _ io.Reader, _ io.Writer) error {
				<-ctx.Done()
				return ctx.Err()
			})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 80*time.Second {
			t.Fatal("explicit lifetime extended its parent deadline")
		}
	})
}

func TestSshLeaseOwnerCancellationPropagatesWithoutRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, stop := context.WithCancel(context.Background())
		defer stop()
		go func() { time.Sleep(60 * time.Second); stop() }()
		err := runSshLeaseCommand(owner, []string{"ssh-lease", "/synthetic/request", "--lifetime-seconds", "1900"}, strings.NewReader(""), io.Discard,
			func(ctx context.Context, _ string, _ io.Reader, _ io.Writer) error {
				<-ctx.Done()
				return ctx.Err()
			})
		if !errors.Is(err, context.Canceled) {
			t.Fatal("owner cancellation did not reach the unchanged lease protocol")
		}
	})
}
