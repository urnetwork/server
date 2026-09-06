//go:build linux

package server

// Configuration content is captured once into a kernel-sealed bounded archive.
// Workers admit that exact archive and expose only per-file sealed descriptors
// to their central resolver, never the mutable source directory.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Entry order is the canonical sorted depth-first traversal used by snapshots.
// Logical directories have no bytes; the root is exactly one "." directory.
type testProcessConfigurationEntry struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	Value     []byte `json:"value"`
}

// One archive is bounded independently of JSON's base64/name expansion.
type testProcessConfigurationArchive struct {
	Schema  int                             `json:"schema"`
	Entries []testProcessConfigurationEntry `json:"entries"`
}

// Bounds the archive, every logical path, and at most MaxFiles+1 retained
// descriptors. Arithmetic rejects overflow before any allocation or file read.
func testProcessConfigurationArchiveLimit(limits TestProcessConfigurationLimits) (int64, error) {
	if limits.MaxFiles <= 0 || limits.MaxFiles == math.MaxInt || limits.MaxDepth <= 0 ||
		limits.MaxBytes <= 0 || limits.MaxBytes > math.MaxInt64/2 {
		return 0, errors.New("owned configuration archive limits are invalid")
	}
	if int64(limits.MaxDepth) > (math.MaxInt64-256)/256 {
		return 0, errors.New("owned configuration path bound overflows")
	}
	pathBytes := (int64(limits.MaxDepth) + 1) * 256
	if pathBytes > (math.MaxInt64-128)/6 {
		return 0, errors.New("owned configuration encoded path bound overflows")
	}
	perEntry := 6*pathBytes + 128
	entries := int64(limits.MaxFiles) + 1
	if entries > (math.MaxInt64-1024-2*limits.MaxBytes)/perEntry {
		return 0, errors.New("owned configuration archive byte bound overflows")
	}
	bound := 1024 + 2*limits.MaxBytes + entries*perEntry
	if bound == math.MaxInt64 {
		return 0, errors.New("owned configuration archive lookahead bound overflows")
	}
	return bound, nil
}

// Materializes immutable kernel bytes without an on-disk name or writable mode.
func sealTestProcessConfigurationBytes(value []byte) (file *os.File, resultErr error) {
	fd, err := unix.MemfdCreate("urnetwork-test-configuration", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	file = os.NewFile(uintptr(fd), "owned-test-configuration")
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, file.Close())
			file = nil
		}
	}()
	if count, err := file.Write(value); err != nil {
		return file, err
	} else if count != len(value) {
		return file, io.ErrShortWrite
	}
	if err := file.Chmod(0o400); err != nil {
		return file, err
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, testProcessSeals); err != nil {
		return file, err
	}
	return file, nil
}

// Keeps bytes from the same successful checked traversal as their digest.
func sealTestProcessConfiguration(ctx context.Context, configuration TestProcessConfiguration) (*os.File, error) {
	limit, err := testProcessConfigurationArchiveLimit(configuration.Limits)
	if err != nil {
		return nil, err
	}
	archive := testProcessConfigurationArchive{Schema: 1}
	actual, err := readTestProcessConfigurationSnapshot(ctx, configuration.Directory, configuration.Limits,
		func(relative string, directory bool, value []byte) {
			archive.Entries = append(archive.Entries, testProcessConfigurationEntry{
				Path: relative, Directory: directory, Value: append([]byte(nil), value...),
			})
		})
	if err != nil || actual != configuration.SHA256 {
		return nil, errors.Join(err, errors.New("owned configuration changed before archive sealing"))
	}
	if err := validateTestProcessConfigurationArchive(ctx, archive, configuration.Limits, configuration.SHA256); err != nil {
		return nil, err
	}
	value, err := json.Marshal(archive)
	if err != nil || int64(len(value)) > limit {
		return nil, errors.New("owned configuration archive exceeds its encoded bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sealTestProcessConfigurationBytes(value)
}

// Full semantic validation precedes descriptor publication. Duplicate names,
// traversal, conflicting types, absent parents and alternate ordering fail closed.
func validateTestProcessConfigurationArchive(ctx context.Context, archive testProcessConfigurationArchive, limits TestProcessConfigurationLimits, expected string) error {
	if _, err := testProcessConfigurationArchiveLimit(limits); err != nil {
		return err
	}
	if archive.Schema != 1 || !validTestProcessDigest(expected) || len(archive.Entries) == 0 ||
		len(archive.Entries)-1 > limits.MaxFiles {
		return errors.New("owned configuration archive schema or census differs")
	}
	entries := map[string]testProcessConfigurationEntry{}
	children := map[string][]string{}
	var totalBytes int64
	for _, entry := range archive.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Path == "" || !utf8.ValidString(entry.Path) || path.IsAbs(entry.Path) || path.Clean(entry.Path) != entry.Path ||
			strings.ContainsAny(entry.Path, "\\\x00") || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") ||
			int64(len(entry.Path)) > (int64(limits.MaxDepth)+1)*256 {
			return errors.New("owned configuration archive logical path is invalid")
		}
		if _, exists := entries[entry.Path]; exists {
			return errors.New("owned configuration archive repeats a logical path")
		}
		depth := len(strings.Split(entry.Path, "/"))
		if entry.Path == "." {
			depth = 0
			if !entry.Directory {
				return errors.New("owned configuration archive root is not a directory")
			}
		}
		if entry.Directory && (entry.Value != nil || depth > limits.MaxDepth) ||
			!entry.Directory && (depth-1 > limits.MaxDepth || int64(len(entry.Value)) > limits.MaxBytes-totalBytes) {
			return errors.New("owned configuration archive type, depth or byte bound differs")
		}
		if !entry.Directory {
			totalBytes += int64(len(entry.Value))
			if path.Base(entry.Path) == "settings.yml" {
				if err := rejectTestProcessEnvironmentSettings(entry.Value); err != nil {
					return err
				}
			}
		}
		entries[entry.Path] = entry
		if entry.Path != "." {
			parent := path.Dir(entry.Path)
			children[parent] = append(children[parent], entry.Path)
		}
	}
	if root, ok := entries["."]; !ok || !root.Directory {
		return errors.New("owned configuration archive root is absent")
	}
	for name := range entries {
		if name == "." {
			continue
		}
		if parent, ok := entries[path.Dir(name)]; !ok || !parent.Directory {
			return errors.New("owned configuration archive logical parent is absent or not a directory")
		}
	}
	for name := range children {
		sort.Strings(children[name])
	}
	digest := sha256.New()
	encoder := json.NewEncoder(digest)
	next := 0
	var visit func(string) error
	visit = func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if next >= len(archive.Entries) || archive.Entries[next].Path != name {
			return errors.New("owned configuration archive order is not canonical")
		}
		next++
		entry := entries[name]
		if entry.Directory {
			if err := encoder.Encode(struct {
				Path string
				Kind string
			}{Path: name, Kind: "directory"}); err != nil {
				return err
			}
			for _, child := range children[name] {
				if err := visit(child); err != nil {
					return err
				}
			}
		} else {
			hash := sha256.Sum256(entry.Value)
			if err := encoder.Encode(struct {
				Path   string
				Kind   string
				Bytes  int64
				SHA256 string
			}{
				Path: name, Kind: "file", Bytes: int64(len(entry.Value)), SHA256: hex.EncodeToString(hash[:]),
			}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit("."); err != nil {
		return err
	}
	if next != len(archive.Entries) || hex.EncodeToString(digest.Sum(nil)) != expected {
		return errors.New("owned configuration archive complete content identity differs")
	}
	return nil
}

// Actual kernel sealing is checked in every admitting process, not inferred
// from mode bits or from a matching after-the-fact pathname snapshot.
func readTestProcessConfigurationArchive(ctx context.Context, file *os.File, limits TestProcessConfigurationLimits, expected string) (testProcessConfigurationArchive, error) {
	var archive testProcessConfigurationArchive
	if file == nil {
		return archive, errors.New("owned configuration archive descriptor is missing")
	}
	bound, err := testProcessConfigurationArchiveLimit(limits)
	if err != nil {
		return archive, err
	}
	if err := ctx.Err(); err != nil {
		return archive, err
	}
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&testProcessSeals != testProcessSeals {
		return archive, errors.New("owned configuration archive is not kernel sealed")
	}
	value, err := io.ReadAll(io.NewSectionReader(file, 0, bound+1))
	if err != nil || int64(len(value)) > bound {
		return archive, errors.New("owned configuration archive is unavailable or exceeds its bound")
	}
	if err := decodeTestProcessJSON(value, &archive); err != nil {
		return archive, err
	}
	if err := validateTestProcessConfigurationArchive(ctx, archive, limits, expected); err != nil {
		return archive, err
	}
	return archive, nil
}
