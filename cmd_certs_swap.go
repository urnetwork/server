// `sidecarctl certs swap <env> <version>` moves a new certificate into a running lb with no new
// image, for an lb whose config reads tls/current (W5, §19.5; D-OPS-12's "W5 reloads them"). An lb
// image bakes its nginx config when it is built, so a path under tls/<version> changes only with a
// new image, while a path under tls/current/<name> names no version: pointing that link at another
// version and reloading nginx is the whole rotation. warpctl keeps the other half, the lb config:
// `warpctl lb create-config` reads tls/current/<name> only where the builder's vault holds it.
//
// Production does not use this. The owner decided on 2026-11-06 (QD-022) that every lb stays on
// versioned paths for now, so no vault entry or lb image reads tls/current. Every lb deployed today
// reads the version's names from versioned paths, which this refuses before it changes anything.
//
// It runs on one lb host, after the version it names has reached the host, against the flat vault
// warpctl mounts into an lb there: warp.json's vaultHome, else $WARP_VAULT_HOME, else
// $WARP_HOME/vault, as warpctl's RequireVaultHome finds it. So it needs the WARP_HOME the host's
// units run warpctl with, and refuses without one. Links are relative, because the vault mounts at
// another path in the lb container, and there is one per name, because a pair has to change in one
// step and each name's newest version can differ. nginx reads a link only at start and reload, so a
// moved link changes nothing a client sees until the reload that follows it. Docker Desktop's file
// sharing has failed an nginx reload right after a link moved, where ext4 did not, so a swap
// against a macOS bind mount says nothing about one on a host.
//
// Neither half of that is taken on trust. nginx reloads happily with a config that never reads
// tls/current, or from a vault that is not this one, and answers 0 to a reload it then ignores. So
// each lb's paths are read before a link moves, each lb must see the moved links, and `Reloaded`
// waits until the lb has replaced every worker it ran before the signal. nginx alone has the last
// word on whether it loads a pair, and only once the links have moved, so a swap that fails puts
// them back: a refused version left in the links would stop the next lb start.
//
// All it knows of warp is warp's layout, which the constants below name: the labels warpctl run
// stamps on a container and the vault mount it gives an lb (warpctl/run.go), where warpctl finds
// that vault (warpctl/docker.go), the config lb/main.go starts nginx from for a block, and the
// lines `warpctl lb create-config` writes there. warpctl's
// TestNginxConfigWritesTlsLinesTheSwapReads holds its pair lines to lbTlsPathRegex's shape, and
// TestNginxConfigWritesWorkerCountsTheSwapReads its worker count to what lbWorkerProcessesRegex
// and lbWorkerProcessesLineRegex read.
//
// tls/current must stay out of what the vault copy ships to a host, which follows links in the
// destination: it would overwrite the version a host's link points at, and a version's key is also
// a jwt key (warp/README.md, "TLS certificates").

package main

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// warp's lb layout, as the comment at the top of this file says where each comes from.
const (
	// where the vault mounts in an lb container, which every tls path in its config starts with
	lbVaultMountPath = "/srv/warp/vault"
	// the config an lb starts nginx from for a block, with the block's name in place of %s. A
	// host-networked lb's master runs <block>_host.conf, which differs only in its listen lines.
	lbBlockConfigPathFormat = "/srv/warp/nginx.conf/%s.conf"
	// the labels warpctl run puts on every container, and the service an lb runs as
	warpEnvLabel     = "warp.env"
	warpServiceLabel = "warp.service"
	warpBlockLabel   = "warp.block"
	lbServiceName    = "lb"
	// the directory under tls/ whose entries name no version. warpctl's tlsCurrentDirName is the
	// same, and it does not parse as a version, which keeps it out of every reader that walks tls/
	// for versions, server's jwt keyring included.
	tlsCurrentDirName = "current"
)

// What nginx titles a worker in `docker top`, and what it retitles one that is finishing its
// connections. Both kinds run after a reload. A drain leaves only the second, and nginx's master
// ignores every reload from then on. An nginx that changed these would show no workers, which
// refuses every swap and passes none.
const (
	nginxWorkerTitle         = "nginx: worker process"
	nginxShutdownWorkerTitle = "nginx: worker process is shutting down"
)

// How many workers nginx runs for a config that sets no count, its built-in default.
const nginxDefaultWorkerCount = 1

// A tls path in an lb config. A line that is commented out does not match.
var lbTlsPathRegex = regexp.MustCompile(`(?m)^\s*ssl_certificate(?:_key)?\s+([^\s;]+)\s*;`)

// How many workers an lb config runs, as warpctl writes it for a block sized for its load: a
// number, opening its line. auto, or anything else, does not match.
var lbWorkerProcessesRegex = regexp.MustCompile(`(?m)^\s*worker_processes\s+([0-9]+)\s*;`)

// A line that sets the worker count in any shape, the directive anywhere before a comment. warpctl
// writes none for a block without capacity sizing, whose nginx then runs its default.
var lbWorkerProcessesLineRegex = regexp.MustCompile(`(?m)^[^#\n]*\bworker_processes\b`)

// A version as `warpctl certs issue` names every one it writes, yyyy.m.d: three numbers and nothing
// else. warp and server take any semver as a version, so refusing the rest fails closed, and it
// refuses current, an empty name and anything that is a path.
var tlsVersionRegex = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "certs swap",
		usage:   "<env> <version> [--no-reload]",
		summary: "Point this lb host's tls/current at a version and reload its lbs. --no-reload moves the links alone.",
		run:     runCertsSwap,
	})
}

// How one swap runs. Tests shorten the waits and replace the seams.
type certsSwapSettings struct {
	// Moves the links and touches no lb and no docker daemon, for a host with no lb a reload would
	// reach: a new host before its first lb start, or a host whose lb is down, where this is the
	// rollback. Nothing but the pair checks judges the version then, and a running lb keeps its old
	// pair until its next reload or start. Nor does any lb confirm the vault: that rests on finding
	// it as warpctl does, from the environment the host's units give warpctl (runCertsSwap).
	noReload bool
	// Bounds each docker call, so that a wedged daemon fails the swap instead of hanging the job
	// that ran it. A reload parses the whole lb config first. Non-positive expires every call at
	// once, so the swap refuses at its census with nothing moved.
	dockerTimeout time.Duration
	// How long a docker call its timeout killed may hold its output open before the swap stops
	// waiting for it, as warpctl's retirement commands allow. Non-positive waits until it closes.
	dockerWaitDelay time.Duration
	// How many times to look at an lb's workers before failing its reload: before the signal, for
	// the lb to run exactly its own count of workers, and again after it, for the reload to retire
	// them all. With the interval this bounds how long a master may take to parse its config and
	// start its workers, which a minute covers many times over. Non-positive never looks, so every
	// reload fails before its signal.
	reloadCheckCount int
	// The wait between those looks, long enough to keep `docker top` calls few. Non-positive looks
	// again at once.
	reloadCheckInterval time.Duration
	// The smallest RSA key a pair may hold, in bits. 2048 is what OpenSSL asks at security level 2,
	// which Debian's nginx runs at, so a smaller key would pass every other check here and stop
	// the reload, or with noReload the next lb start. Non-positive accepts any size.
	minimumRsaKeyBits int
	// The time each certificate's validity is judged at.
	now func() time.Time
	// Moves one temporary link over its name. This is the one step that changes what a name
	// serves.
	rename func(oldPath string, newPath string) error
	// Runs one docker command, its arguments after `docker`, and returns what it printed.
	docker func(ctx context.Context, args []string) ([]byte, error)
}

// The settings of a swap run on an lb host. A caller changes its own copy.
func defaultCertsSwapSettings() *certsSwapSettings {
	settings := &certsSwapSettings{
		noReload:            false,
		dockerTimeout:       60 * time.Second,
		dockerWaitDelay:     time.Second,
		reloadCheckCount:    120,
		reloadCheckInterval: 500 * time.Millisecond,
		minimumRsaKeyBits:   2048,
		now:                 time.Now,
		rename:              os.Rename,
	}
	settings.docker = func(ctx context.Context, args []string) ([]byte, error) {
		return runDockerCli(ctx, settings.dockerWaitDelay, args)
	}
	return settings
}

// One lb on this host that can take a reload.
type lbContainer struct {
	containerId string
	block       string
	// the names of this version the lb reads through tls/current
	reachedNames []string
	// the workers it runs exactly when no reload is under way: the count its config sets, or
	// nginx's default where it sets none
	workerCount int
}

// `sidecarctl certs swap <env> <version> [--no-reload]`, in the vault warpctl would mount into an
// lb it ran with this environment.
func runCertsSwap(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("certs swap", flag.ContinueOnError)
	noReload := flags.Bool("no-reload", false, "move the links and touch no lb")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 2 {
		return sidecarctlUsageErrorf("want <env> and <version>, got %q", positionals)
	}

	// warpctl's RequireVaultHome (warpctl/docker.go), which every lb's vault mount comes from. With
	// --no-reload nothing after this checks that the links move where an lb reads them, so where
	// warpctl could find another vault this refuses.
	vaultHome, err := findWarpVaultHome()
	if err != nil {
		return err
	}
	settings := defaultCertsSwapSettings()
	settings.noReload = *noReload
	return swapTlsCurrent(vaultHome, positionals[0], positionals[1], settings, out)
}

// Points tls/current/<name> at tls/<version>/<name> for every name the version holds, then reloads
// nginx in this host's lb containers of env, printing each step to out. Names the version does not
// hold stay where they are, and swapping to an older version is a rollback.
//
// Every check comes before the first link moves, so a refusal changes nothing. That covers the
// lbs too: one reading a name of this version from anywhere but tls/current would reload and go on
// serving what it served, as would one reading none of them. A draining lb is passed over, since it
// takes no reload and no new connections, and with no lb left to reload the swap refuses, unless
// settings.noReload asks for the links alone.
//
// A move or reload that fails, or an lb that does not see the moved links, puts every link back
// and reloads any lb already reloaded, so a failed swap leaves the host as it was and can simply be
// run again. An lb whose reload failed goes on serving its old pair from memory.
func swapTlsCurrent(vaultHome string, env string, version string, settings *certsSwapSettings, out io.Writer) error {
	if !tlsVersionRegex.MatchString(version) {
		return fmt.Errorf("%q is not a tls version, which is three numbers such as 2026.11.5", version)
	}
	versionHome := filepath.Join(vaultHome, "tls", version)
	currentHome := filepath.Join(vaultHome, "tls", tlsCurrentDirName)

	// a pair must be one a visitor can trust and nginx can load: the key matches the certificate,
	// which is valid now and covers the name, and an RSA key is as large as nginx's tls library
	// asks. star.<domain> is the wildcard's name, which VerifyHostname matches against the
	// wildcard. nginx may still refuse a pair for a reason of its own, so it has the last word at
	// the reload.
	verifyPair := func(name string) error {
		pair, err := tls.LoadX509KeyPair(
			filepath.Join(versionHome, name, fmt.Sprintf("%s.pem", name)),
			filepath.Join(versionHome, name, fmt.Sprintf("%s.key", name)),
		)
		if err != nil {
			return err
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
		if rsaKey, ok := leaf.PublicKey.(*rsa.PublicKey); ok && rsaKey.N.BitLen() < settings.minimumRsaKeyBits {
			return fmt.Errorf("the key is RSA of %d bits, and nginx's tls library refuses fewer than %d", rsaKey.N.BitLen(), settings.minimumRsaKeyBits)
		}
		now := settings.now()
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return fmt.Errorf("the certificate is valid from %s to %s", leaf.NotBefore, leaf.NotAfter)
		}
		return leaf.VerifyHostname(name)
	}
	// one bad name refuses them all: the version was meant to go live as a whole. A stray file is
	// no name, but a link is refused, since what it leads to was never meant to be a name of this
	// version. ReadDir returns names in order.
	entries, err := os.ReadDir(versionHome)
	if err != nil {
		return err
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing %s: it is a link, and a version holds its names as directories", filepath.Join(versionHome, entry.Name()))
		}
		if !entry.IsDir() {
			continue
		}
		if err := verifyPair(entry.Name()); err != nil {
			return fmt.Errorf("refusing %s: %w", filepath.Join(versionHome, entry.Name()), err)
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return fmt.Errorf("%s holds no certificates", versionHome)
	}

	// a link moves in one step. A real directory cannot, and a tls/current that is itself a link
	// would take these links into a version's directory. Each target is kept for putting the link
	// back; a name not in here had no link.
	previousNameTargets := map[string]string{}
	checkMovable := func() error {
		info, err := os.Lstat(currentHome)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory of links", currentHome)
		}
		for _, name := range names {
			linkPath := filepath.Join(currentHome, name)
			info, err := os.Lstat(linkPath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("%s is not a link, so it cannot move in one step", linkPath)
			}
			target, err := os.Readlink(linkPath)
			if err != nil {
				return err
			}
			previousNameTargets[name] = target
		}
		return nil
	}
	if err := checkMovable(); err != nil {
		return err
	}

	// a draining container is listed too, and nginx answers 0 to a reload it then ignores. Workers
	// tell the two apart. With none of either kind nothing here can say which state the lb is in,
	// so that refuses like any unknown.
	findLbContainers := func() ([]*lbContainer, error) {
		output, err := dockerOutput(
			settings,
			"ps",
			"--no-trunc",
			"--filter", fmt.Sprintf("label=%s=%s", warpEnvLabel, env),
			"--filter", fmt.Sprintf("label=%s=%s", warpServiceLabel, lbServiceName),
			"--format", fmt.Sprintf(`{{.ID}} {{.Label "%s"}}`, warpBlockLabel),
		)
		if err != nil {
			return nil, fmt.Errorf("find %s lb containers: %w", env, err)
		}
		lbContainers := []*lbContainer{}
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) != 2 {
				return nil, fmt.Errorf("cannot read lb container %q", line)
			}
			containerId, block := fields[0], fields[1]
			workerPids, shutdownWorkerCount, err := readLbWorkers(settings, containerId)
			if err != nil {
				return nil, fmt.Errorf("read the workers of %s: %w", block, err)
			}
			if len(workerPids) == 0 {
				if shutdownWorkerCount == 0 {
					return nil, fmt.Errorf("%s has no nginx workers, so it cannot take a reload yet", block)
				}
				fmt.Fprintf(out, "Passing over %s: it is draining\n", block)
				continue
			}
			lbContainers = append(lbContainers, &lbContainer{
				containerId: containerId,
				block:       block,
			})
		}
		return lbContainers, nil
	}

	// an image's paths are fixed when it is built, per name, so a name without a tls/current entry
	// in the builder's vault stays versioned in the image, and a swap can reach a name only through
	// tls/current. A name this version does not hold has nothing new to serve, so it stops nothing,
	// but it has not rotated either, so the swap says so. The worker count is what tells the lb's
	// own reload from a late worker of an earlier one, so an lb whose count cannot be read is
	// refused like one whose paths cannot.
	checkLbReads := func(lb *lbContainer) error {
		// not the whole config, so that only tls paths and the worker count reach a log
		output, err := dockerOutput(settings, "exec", lb.containerId, "grep", "-e", "ssl_certificate", "-e", "worker_processes", fmt.Sprintf(lbBlockConfigPathFormat, lb.block))
		if err != nil {
			return fmt.Errorf("read the tls paths of %s: %w", lb.block, err)
		}
		// warpctl writes the count once for a block sized for its load and not at all for one
		// without capacity sizing, and nginx runs one worker for a config that sets none. A count
		// set some other way, as auto or twice, cannot be read. One set where this grep does not
		// look, as in an include, which the lb image leaves empty, would differ from what nginx
		// runs, and the wait before the signal refuses a count the lb does not settle at.
		switch len(lbWorkerProcessesLineRegex.FindAll(output, -1)) {
		case 0:
			lb.workerCount = nginxDefaultWorkerCount
		case 1:
			// a count past an int is refused here, where Atoi would give the largest int with its
			// error, and the wait before the signal would look for that many workers
			if match := lbWorkerProcessesRegex.FindSubmatch(output); match != nil {
				if workerCount, err := strconv.Atoi(string(match[1])); err == nil {
					lb.workerCount = workerCount
				}
			}
		}
		if lb.workerCount <= 0 {
			return fmt.Errorf("%s sets its count of nginx workers in a way this cannot read, so its reload cannot be told from an earlier one's", lb.block)
		}
		currentNames := map[string]bool{}
		otherNamePaths := map[string]string{}
		for _, match := range lbTlsPathRegex.FindAllSubmatch(output, -1) {
			tlsPath := string(match[1])
			name := strings.TrimSuffix(path.Base(tlsPath), path.Ext(tlsPath))
			if path.Dir(tlsPath) == path.Join(lbVaultMountPath, "tls", tlsCurrentDirName, name) {
				currentNames[name] = true
			} else {
				otherNamePaths[name] = tlsPath
			}
		}
		for _, name := range names {
			if otherPath, ok := otherNamePaths[name]; ok {
				return fmt.Errorf("%s reads %s from %s, not from tls/%s, so a reload would not change what it serves", lb.block, name, otherPath, tlsCurrentDirName)
			}
			if currentNames[name] {
				lb.reachedNames = append(lb.reachedNames, name)
			}
		}
		if len(lb.reachedNames) == 0 {
			return fmt.Errorf("%s reads none of %s's names from tls/%s, so a reload would change nothing", lb.block, version, tlsCurrentDirName)
		}
		for _, name := range slices.Sorted(maps.Keys(otherNamePaths)) {
			fmt.Fprintf(out, "%s keeps reading %s from %s, which no swap moves\n", lb.block, name, otherNamePaths[name])
		}
		for _, name := range slices.Sorted(maps.Keys(currentNames)) {
			if !slices.Contains(names, name) {
				fmt.Fprintf(out, "%s keeps its %s link: %s does not hold that name\n", lb.block, name, version)
			}
		}
		return nil
	}

	lbContainers := []*lbContainer{}
	if !settings.noReload {
		var err error
		if lbContainers, err = findLbContainers(); err != nil {
			return err
		}
		if len(lbContainers) == 0 {
			return fmt.Errorf("no %s lb on this host can take a reload. To move the links alone, as on a new host or one whose lb is down, use --no-reload", env)
		}
		for _, lb := range lbContainers {
			if err := checkLbReads(lb); err != nil {
				return err
			}
		}
	}

	// the temporary link is hidden so that one left by a swap that died here is not read as a
	// name, and sits beside the name so that the rename is atomic
	pointLink := func(name string, target string) error {
		tempPath := filepath.Join(currentHome, fmt.Sprintf(".%s.swap", name))
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Symlink(target, tempPath); err != nil {
			return err
		}
		if err := settings.rename(tempPath, filepath.Join(currentHome, name)); err != nil {
			return errors.Join(err, os.Remove(tempPath))
		}
		return nil
	}

	// `-c` names the config the image baked for the block. nginx parses it only to find the pid
	// file and to prove it loads, refusing here and leaving the master untouched if a pair does
	// not. The master then rereads the config it started with, which for a host-networked lb is
	// `_host.conf`, after nginx here has exited 0, so only the workers show that it did. A master
	// forks a reload's workers one by one and retires the old ones only after the last, so a worker
	// that was not there before the signal can be a late one of an earlier reload, which read the
	// links as they stood then. So the signal waits until the lb runs exactly its own count of
	// workers, with no reload under way, and the reload is done only when every worker from before
	// the signal has stopped serving while another serves: the end of a whole reload begun after
	// the signal. A drain stops them all too, with none serving after. A master slower than every
	// look reads whichever links stand when it gets there.
	reloadLb := func(lb *lbContainer) error {
		settledWorkerPids := []string{}
		for checkIndex := 0; checkIndex < settings.reloadCheckCount; checkIndex += 1 {
			if 0 < checkIndex {
				time.Sleep(settings.reloadCheckInterval)
			}
			workerPids, _, err := readLbWorkers(settings, lb.containerId)
			if err != nil {
				return fmt.Errorf("read its workers before the signal: %w", err)
			}
			if len(workerPids) == lb.workerCount {
				settledWorkerPids = workerPids
				break
			}
		}
		if len(settledWorkerPids) == 0 {
			return fmt.Errorf("nginx never ran exactly its %d workers before the signal: a reload is under way, or it is draining", lb.workerCount)
		}
		_, err := dockerOutput(
			settings,
			"exec",
			lb.containerId,
			"nginx",
			"-c", fmt.Sprintf(lbBlockConfigPathFormat, lb.block),
			"-s", "reload",
		)
		if err != nil {
			return err
		}
		for checkIndex := 0; checkIndex < settings.reloadCheckCount; checkIndex += 1 {
			if 0 < checkIndex {
				time.Sleep(settings.reloadCheckInterval)
			}
			workerPids, _, err := readLbWorkers(settings, lb.containerId)
			if err != nil {
				return fmt.Errorf("read its workers after the signal: %w", err)
			}
			retired := !slices.ContainsFunc(workerPids, func(workerPid string) bool {
				return slices.Contains(settledWorkerPids, workerPid)
			})
			if 0 < len(workerPids) && retired {
				return nil
			}
		}
		return errors.New("nginx took the signal and did not replace its workers: it is draining, or its log says why")
	}

	// a vault home that is not the one an lb mounts moves links nothing there reads, and the lb
	// would reload what it had, with new workers to show for it
	checkLbSeesLinks := func(lb *lbContainer) error {
		args := []string{"exec", lb.containerId, "readlink"}
		wantTargets := []string{}
		for _, name := range lb.reachedNames {
			args = append(args, path.Join(lbVaultMountPath, "tls", tlsCurrentDirName, name))
			wantTargets = append(wantTargets, path.Join("..", version, name))
		}
		output, err := dockerOutput(settings, args...)
		if err != nil {
			return fmt.Errorf("read the links %s sees: %w", lb.block, err)
		}
		if targets := strings.Fields(string(output)); !slices.Equal(targets, wantTargets) {
			return fmt.Errorf("%s sees its links at %v, not %v, so it does not mount the vault at %s", lb.block, targets, wantTargets, vaultHome)
		}
		return nil
	}

	// a version left in the links after nginx refused it would stop the next lb start, and an lb
	// already reloaded would be alone on the new pair
	undoSwap := func(swapErr error, movedNames []string, reloadedLbContainers []*lbContainer) error {
		undoErrs := []error{swapErr}
		for _, name := range movedNames {
			var err error
			if target, ok := previousNameTargets[name]; ok {
				err = pointLink(name, target)
			} else {
				err = os.Remove(filepath.Join(currentHome, name))
			}
			if err != nil {
				undoErrs = append(undoErrs, fmt.Errorf("put %s back: %w", name, err))
				continue
			}
			fmt.Fprintf(out, "Put %s back\n", filepath.Join(currentHome, name))
		}
		for _, lb := range reloadedLbContainers {
			if err := reloadLb(lb); err != nil {
				undoErrs = append(undoErrs, fmt.Errorf("reload %s back: %w", lb.block, err))
				continue
			}
			fmt.Fprintf(out, "Reloaded %s back\n", lb.block)
		}
		return errors.Join(undoErrs...)
	}

	// tls/current leads to private keys, so it is its owner's alone
	if err := os.MkdirAll(currentHome, 0700); err != nil {
		return err
	}
	movedNames := []string{}
	for _, name := range names {
		if err := pointLink(name, path.Join("..", version, name)); err != nil {
			return undoSwap(fmt.Errorf("swap %s: %w", name, err), movedNames, nil)
		}
		movedNames = append(movedNames, name)
		fmt.Fprintf(out, "%s -> %s\n", filepath.Join(currentHome, name), version)
	}
	if settings.noReload {
		fmt.Fprintf(out, "Reloaded nothing: a running lb keeps its old pair until its next reload or start\n")
		return nil
	}

	for _, lb := range lbContainers {
		if err := checkLbSeesLinks(lb); err != nil {
			return undoSwap(err, movedNames, nil)
		}
	}
	for lbIndex, lb := range lbContainers {
		if err := reloadLb(lb); err != nil {
			return undoSwap(fmt.Errorf("reload %s: %w", lb.block, err), movedNames, lbContainers[:lbIndex])
		}
		fmt.Fprintf(out, "Reloaded %s\n", lb.block)
	}
	return nil
}

// Reads an lb container's nginx workers through `docker top`, which asks nothing of the image.
// Returns the pids of those serving, and how many are shutting down. A worker's title is all that
// tells the two apart from outside nginx.
func readLbWorkers(settings *certsSwapSettings, containerId string) ([]string, int, error) {
	output, err := dockerOutput(settings, "top", containerId, "-o", "pid,args")
	if err != nil {
		return nil, 0, err
	}
	workerPids := []string{}
	shutdownWorkerCount := 0
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.Join(fields[1:], " ") {
		case nginxWorkerTitle:
			workerPids = append(workerPids, fields[0])
		case nginxShutdownWorkerTitle:
			shutdownWorkerCount += 1
		}
	}
	return workerPids, shutdownWorkerCount, nil
}

// Runs one docker command under the swap's timeout. A command that finished after the timeout is
// a failure too, whatever it answered.
func dockerOutput(settings *certsSwapSettings, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), settings.dockerTimeout)
	defer cancel()
	output, err := settings.docker(ctx, args)
	if ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), err)
	}
	return output, err
}

// Runs the docker cli as warpctl does, through sudo on linux, in a process group of its own so
// that ctx's end kills sudo's child too, and waits at most waitDelay for its output to close after
// that. A failure carries what the command printed to stderr, which the standard library bounds.
func runDockerCli(ctx context.Context, waitDelay time.Duration, args []string) ([]byte, error) {
	name := "docker"
	commandArgs := args
	if runtime.GOOS == "linux" {
		name = "sudo"
		commandArgs = append([]string{"docker"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, commandArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = waitDelay
	output, err := cmd.Output()
	if err == nil {
		return output, nil
	}
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) && 0 < len(strings.TrimSpace(string(exitErr.Stderr))) {
		return output, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return output, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
}
