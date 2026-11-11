//go:build linux

// A root-only companion runs independently of agent admission but inside the
// existing private aggregate. It never invokes an agent or changes its proofs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// The installed companion and unit names are root deployment identities.
const cacheInstalledExecutable = "/usr/local/libexec/competitionmanagementcache"

// Parsing cannot select another reclaim path, weaken a guard or reset a journal.
func main() {
	settings := defaultCacheSettings()
	flags := flag.NewFlagSet("competitionmanagementcache", flag.ContinueOnError)
	once := flags.Bool("once", false, "Perform one bounded native pass")
	renderService := flags.Bool("render-service", false, "Render the independently started bounded service")
	renderTimer := flags.Bool("render-timer", false, "Render the coalescing oneshot timer")
	settingsPath := flags.String("settings", "", "Optional protected root-owned numeric policy")
	if err := flags.Parse(os.Args[1:]); err != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	operations := 0
	for _, enabled := range []bool{*once, *renderService, *renderTimer} {
		if enabled {
			operations++
		}
	}
	if operations != 1 {
		fmt.Fprintln(os.Stderr, "exactly one companion operation is required")
		os.Exit(2)
	}
	if *settingsPath != "" {
		if err := loadCacheSettings(*settingsPath, &settings); err != nil {
			fmt.Fprintln(os.Stderr, "protected cache settings are unavailable")
			os.Exit(1)
		}
	}
	if err := settings.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *renderService || *renderTimer {
		content, err := renderCacheUnit(settings, *settingsPath, *renderService)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(content)
		return
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "native cache maintenance requires root custody")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	result, err := runCachePass(ctx, settings)
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if err != nil {
		// Kernel paths, process identities and private error details stay local.
		fmt.Fprintln(os.Stderr, "bounded cache maintenance did not complete; inspect the finite result")
		os.Exit(1)
	}
}

// Optional settings retain strict root custody and cannot add target authority.
func loadCacheSettings(path string, settings *cacheSettings) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("cache settings require a canonical absolute protected path")
	}
	directory, err := openProtectedCacheDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	fd, err := syscall.Openat(int(directory.Fd()), filepath.Base(path), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "cache-settings")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return errors.New("cache settings are not protected root authority")
	}
	content, err := io.ReadAll(io.LimitReader(file, settings.DocumentBytes+1))
	if err != nil || int64(len(content)) > settings.DocumentBytes {
		return errors.New("cache settings exceed their document bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(settings); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("cache settings have trailing or oversized content")
	}
	return settings.validate()
}

// Startup never calls managed-agent admission, so pressure cannot create a cycle.
func runCachePass(ctx context.Context, settings cacheSettings) (cacheResult, error) {
	journal, err := newNativeCacheJournal(settings)
	if err != nil {
		return cacheResult{Status: "journal_authority_unavailable"}, err
	}
	defer journal.Close()
	kernel, err := newNativeCacheKernel(settings)
	if err != nil {
		return cacheResult{Status: "fixed_kernel_authority_unavailable"}, err
	}
	defer kernel.Close()
	return maintainCache(ctx, settings, kernel, journal)
}

// Unit rendering shares the owner defaults. The timer never overlaps an active
// service; its runtime/stop bounds contain a slow kernel reclaim syscall.
func renderCacheUnit(settings cacheSettings, settingsPath string, service bool) (string, error) {
	if err := settings.validate(); err != nil {
		return "", err
	}
	argument := ""
	if settingsPath != "" {
		if !regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`).MatchString(settingsPath) || filepath.Clean(settingsPath) != settingsPath {
			return "", errors.New("cache settings path cannot expand manager authority")
		}
		argument = " --settings=" + settingsPath
	}
	seconds := func(value time.Duration) string { return strconv.FormatFloat(value.Seconds(), 'f', 6, 64) + "s" }
	if !service {
		return "[Unit]\nDescription=Bounded private engine cache maintenance timer\n\n[Timer]\nOnBootSec=" + seconds(settings.Interval) +
			"\nOnUnitInactiveSec=" + seconds(settings.Interval) + "\nUnit=urnetwork-management-cache.service\n\n[Install]\nWantedBy=timers.target\n", nil
	}
	return "[Unit]\nDescription=Bounded private engine cache maintenance\nAfter=urnetwork-private-engine.service\n\n[Service]\nType=oneshot\nUser=root\nGroup=root\n" +
		"Slice=urnetwork-management-private.slice\nExecStart=" + cacheInstalledExecutable + " --once" + argument +
		"\nRestart=no\nRuntimeMaxSec=" + seconds(settings.PassTimeout) + "\nTimeoutStartSec=" + seconds(settings.PassTimeout) + "\nTimeoutStopSec=" + seconds(settings.StopTimeout) +
		"\nKillMode=control-group\nMemoryMax=" + strconv.FormatInt(settings.ServiceMemoryBytes, 10) + "\nMemoryHigh=infinity\nMemorySwapMax=0\nTasksMax=" + strconv.Itoa(settings.ServiceTasksMaximum) +
		"\nLimitCORE=0\nUMask=0077\nNoNewPrivileges=yes\nCapabilityBoundingSet=\nAmbientCapabilities=\nPrivateNetwork=yes\nPrivateTmp=yes\n" +
		"ProtectHome=yes\nProtectSystem=strict\nProtectControlGroups=no\nReadOnlyPaths=/sys/fs/cgroup\nReadWritePaths=" + cachePrivatePath + "/" + cacheEngineName +
		"/memory.reclaim\nReadWritePaths=" + cacheJournalPath + "\nRestrictSUIDSGID=yes\nLockPersonality=yes\n", nil
}
