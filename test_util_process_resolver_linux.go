//go:build linux

package server

// Only an admitted worker publishes this immutable logical-file view. Every
// resolver path names a kernel-sealed memfd, including lazy/plain/env/all/version
// reads; there is no fallback to caller-owned filesystem paths.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-semver/semver"
	"golang.org/x/sys/unix"
)

// Maps are immutable after detached construction. The view owns at most one
// descriptor per file plus the retained archive, bounded by MaxFiles+1.
type testProcessConfigurationView struct {
	files            map[string]*os.File
	directories      map[string]bool
	childDirectories map[string][]string
	deadline         time.Time
	limits           TestProcessConfigurationLimits
}

// Child-only publication occurs before env.go init. Ordinary processes retain
// all existing resolver behavior and no mutable global authority cache is used.
var ownedTestProcessConfiguration *testProcessConfigurationView

// Builds and verifies every file before publication; partial failure closes all
// candidate descriptors and leaves the resolver without an owned configuration.
func materializeTestProcessConfiguration(ctx context.Context, archive testProcessConfigurationArchive, limits TestProcessConfigurationLimits, expected string) (view *testProcessConfigurationView, resultErr error) {
	return materializeTestProcessConfigurationWithSealer(ctx, archive, limits, expected, sealTestProcessConfigurationBytes)
}

// The private file-creation seam preserves the real kernel-backed path while
// forcing close/error and post-creation cancellation boundaries in controls.
func materializeTestProcessConfigurationWithSealer(ctx context.Context, archive testProcessConfigurationArchive, limits TestProcessConfigurationLimits, expected string, seal func([]byte) (*os.File, error)) (view *testProcessConfigurationView, resultErr error) {
	if err := validateTestProcessConfigurationArchive(ctx, archive, limits, expected); err != nil {
		return nil, err
	}
	if seal == nil {
		return nil, errors.New("owned configuration file sealer is unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("owned configuration view requires its original deadline")
	}
	view = &testProcessConfigurationView{
		files: map[string]*os.File{}, directories: map[string]bool{},
		childDirectories: map[string][]string{}, deadline: deadline, limits: limits,
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, ctx.Err())
			for _, file := range view.files {
				resultErr = errors.Join(resultErr, file.Close())
			}
			view = nil
		}
	}()
	for _, entry := range archive.Entries {
		if err := ctx.Err(); err != nil {
			return view, err
		}
		if !utf8.ValidString(entry.Path) {
			return view, errors.New("owned configuration logical path is not UTF-8")
		}
		if entry.Directory {
			view.directories[entry.Path] = true
			if entry.Path != "." {
				parent := path.Dir(entry.Path)
				view.childDirectories[parent] = append(view.childDirectories[parent], path.Base(entry.Path))
			}
			continue
		}
		// Creation owns only a copy; it cannot rewrite the admitted archive.
		file, err := seal(bytes.Clone(entry.Value))
		if err != nil {
			if file != nil {
				err = errors.Join(err, file.Close())
			}
			return view, err
		}
		if file == nil {
			return view, errors.New("owned configuration file sealer returned no descriptor")
		}
		seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
		if err != nil || seals&testProcessSeals != testProcessSeals {
			return view, errors.Join(errors.New("owned configuration file is not kernel sealed"), err, file.Close())
		}
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			return view, errors.Join(errors.New("owned configuration file may escape across exec"), err, file.Close())
		}
		// Kernel immutability is necessary, but does not by itself identify
		// the admitted file. Bind the actual descriptor's complete bytes too.
		actual, err := io.ReadAll(io.NewSectionReader(file, 0, int64(len(entry.Value))+1))
		if err != nil || !bytes.Equal(actual, entry.Value) {
			return view, errors.Join(errors.New("owned configuration file differs from admitted bytes"), err, file.Close())
		}
		view.files[entry.Path] = file
	}
	for parent := range view.childDirectories {
		sort.Strings(view.childDirectories[parent])
	}
	if err := ctx.Err(); err != nil {
		return view, err
	}
	return view, nil
}

// The resolver's one read-path gate routes admitted roots to this view only.
// A missing owned resource is an error even if a foreign pathname exists.
func ownedTestProcessResourcePaths(mountType MountType, relative string) ([]string, bool, error) {
	view := ownedTestProcessConfiguration
	if view == nil {
		return nil, false, nil
	}
	paths, err := view.resourcePaths(mountType, relative)
	return paths, true, err
}

// Preserves existing plain/env/all then version-lookup precedence, including
// the legacy repeated plain match produced by its version traversal.
func (self *testProcessConfigurationView) resourcePaths(mountType MountType, relative string) ([]string, error) {
	if !time.Now().Before(self.deadline) {
		return nil, context.DeadlineExceeded
	}
	if relative == "" || relative == "." || path.IsAbs(relative) || path.Clean(relative) != relative ||
		strings.ContainsAny(relative, "\\\x00") || !utf8.ValidString(relative) ||
		relative == ".." || strings.HasPrefix(relative, "../") ||
		int64(len(relative)) > (int64(self.limits.MaxDepth)+1)*256 || strings.Count(relative, "/") > self.limits.MaxDepth {
		return nil, errors.New("owned configuration resource path is invalid")
	}
	var root string
	switch mountType {
	case MOUNT_TYPE_VAULT:
		root = "vault"
	case MOUNT_TYPE_CONFIG:
		root = "config"
	case MOUNT_TYPE_SITE:
		root = "site"
	default:
		return nil, errors.New("owned configuration mount type is unknown")
	}
	homes := []string{root}
	if environment, err := Env(); err == nil && environment != "" &&
		path.Base(environment) == environment && environment != "." && environment != ".." &&
		self.directories[path.Join(root, environment)] {
		homes = append(homes, path.Join(root, environment))
	}
	if self.directories[path.Join(root, "all")] {
		homes = append(homes, path.Join(root, "all"))
	}
	results := []string{}
	remainingSteps := (int64(self.limits.MaxFiles) + 1) * (int64(self.limits.MaxDepth) + 1)
	maxResults := 2*remainingSteps + 3
	appendFile := func(logical string) error {
		if file := self.files[logical]; file != nil {
			if int64(len(results)) >= maxResults {
				return errors.New("owned configuration result exceeds its census-derived bound")
			}
			results = append(results, fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
		}
		return nil
	}
	for _, home := range homes {
		if err := appendFile(path.Join(home, relative)); err != nil {
			return nil, err
		}
	}
	var versions func(string, []string) error
	versions = func(home string, components []string) error {
		if remainingSteps == 0 {
			return errors.New("owned configuration lookup exceeds its census-derived bound")
		}
		remainingSteps--
		if !time.Now().Before(self.deadline) {
			return context.DeadlineExceeded
		}
		if !self.directories[home] || len(components) == 0 {
			return nil
		}
		if len(components) == 1 {
			if err := appendFile(path.Join(home, components[0])); err != nil {
				return err
			}
		} else if err := versions(path.Join(home, components[0]), components[1:]); err != nil {
			return err
		}
		versionNames := map[semver.Version]string{}
		for _, name := range self.childDirectories[home] {
			if version, err := semver.NewVersion(name); err == nil {
				versionNames[*version] = name
			}
		}
		ordered := make([]semver.Version, 0, len(versionNames))
		for version := range versionNames {
			ordered = append(ordered, version)
		}
		semverSortWithBuild(ordered)
		for index := len(ordered) - 1; index >= 0; index-- {
			if err := versions(path.Join(home, versionNames[ordered[index]]), components); err != nil {
				return err
			}
		}
		return nil
	}
	for _, home := range homes {
		if err := versions(home, strings.Split(relative, "/")); err != nil {
			return nil, err
		}
	}
	if len(results) == 0 {
		return nil, errors.New("owned configuration resource is absent from sealed authority")
	}
	return results, nil
}
