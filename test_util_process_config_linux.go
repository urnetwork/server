//go:build linux

package server

// Configuration reads are descriptor anchored, bounded and checked again for
// write-state changes. This code never edits the caller's configuration tree.

import (
	"bytes"
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

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// Captures file identity and write state; access-time changes caused
// by our own reads are intentionally excluded.
func testProcessSameStat(first, second unix.Stat_t) bool {
	return first.Dev == second.Dev && first.Ino == second.Ino &&
		first.Mode == second.Mode && first.Uid == second.Uid && first.Gid == second.Gid &&
		first.Nlink == second.Nlink && first.Size == second.Size &&
		first.Mtim == second.Mtim && first.Ctim == second.Ctim
}

// Opens a new file description relative to the retained parent; no user path
// or symlink is resolved after that descriptor has been accepted.
func openTestProcessAt(parent *os.File, name string, flags int) (*os.File, error) {
	if parent == nil {
		return nil, errors.New("owned test process directory descriptor is missing")
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// A canonical lowercase digest excludes aliases in private capability fields.
func validTestProcessDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// Rejects settings-driven environment changes before env.go can apply them.
func rejectTestProcessEnvironmentSettings(value []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(value, &document); err != nil {
		return errors.New("owned test process settings are not valid YAML")
	}
	pending := []*yaml.Node{&document}
	seen := map[*yaml.Node]bool{}
	for len(pending) != 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[node] {
			continue
		}
		seen[node] = true
		if node.Kind == yaml.MappingNode {
			for index := 0; index < len(node.Content); index += 2 {
				if node.Content[index].Value == "env_vars" {
					return errors.New("owned test process settings cannot override the environment")
				}
			}
		}
		pending = append(pending, node.Content...)
		if node.Alias != nil {
			pending = append(pending, node.Alias)
		}
	}
	return nil
}

// Computes the canonical complete tree digest without trusting pathname
// metadata. Files and directories must have exact private read-only modes.
func SnapshotTestProcessConfiguration(directory *os.File, limits TestProcessConfigurationLimits) (string, error) {
	return snapshotTestProcessConfiguration(context.Background(), directory, limits)
}

// The owning execution uses its existing deadline even during admission reads.
func snapshotTestProcessConfiguration(ctx context.Context, directory *os.File, limits TestProcessConfigurationLimits) (result string, resultErr error) {
	return readTestProcessConfigurationSnapshot(ctx, directory, limits, nil)
}

// The optional private collector owns copied bytes from this same checked
// traversal; a later pathname read is never used to build a sealed archive.
func readTestProcessConfigurationSnapshot(ctx context.Context, directory *os.File, limits TestProcessConfigurationLimits, collect func(string, bool, []byte)) (result string, resultErr error) {
	if limits.MaxFiles <= 0 || limits.MaxFiles == math.MaxInt || limits.MaxDepth <= 0 || limits.MaxBytes <= 0 || limits.MaxBytes == math.MaxInt64 {
		return "", errors.New("owned test process configuration limits are missing")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := openTestProcessAt(directory, ".", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	digest := sha256.New()
	encoder := json.NewEncoder(digest)
	fileCount := 0
	var totalBytes int64
	var walk func(*os.File, string, int) error
	walk = func(current *os.File, relative string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > limits.MaxDepth {
			return errors.New("owned test process configuration exceeds depth limit")
		}
		var before unix.Stat_t
		if err := unix.Fstat(int(current.Fd()), &before); err != nil {
			return err
		}
		if before.Mode != unix.S_IFDIR|0o500 || before.Uid != uint32(os.Geteuid()) {
			return errors.New("owned test process configuration directory is not private and read-only")
		}
		entries, readErr := current.ReadDir(limits.MaxFiles + 1)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if len(entries) > limits.MaxFiles {
			return errors.New("owned test process configuration exceeds entry limit")
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		if err := encoder.Encode(struct {
			Path string
			Kind string
		}{Path: relative, Kind: "directory"}); err != nil {
			return err
		}
		if collect != nil {
			collect(relative, true, nil)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			fileCount++
			if fileCount > limits.MaxFiles {
				return errors.New("owned test process configuration exceeds entry limit")
			}
			name := entry.Name()
			if name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
				return errors.New("owned test process configuration entry name is invalid")
			}
			childPath := path.Join(relative, name)
			file, err := openTestProcessAt(current, name, unix.O_RDONLY|unix.O_NONBLOCK)
			if err != nil {
				return err
			}
			entryErr := func() (entryErr error) {
				defer func() { entryErr = errors.Join(entryErr, file.Close()) }()
				var first unix.Stat_t
				if err := unix.Fstat(int(file.Fd()), &first); err != nil {
					return err
				}
				if first.Mode&unix.S_IFMT == unix.S_IFDIR {
					return walk(file, childPath, depth+1)
				}
				if first.Mode != unix.S_IFREG|0o400 || first.Uid != uint32(os.Geteuid()) || first.Nlink != 1 {
					return errors.New("owned test process configuration file is not private, regular and read-only")
				}
				if first.Size < 0 || first.Size > limits.MaxBytes-totalBytes {
					return errors.New("owned test process configuration exceeds byte limit")
				}
				value, err := io.ReadAll(io.LimitReader(file, first.Size+1))
				if err != nil {
					return err
				}
				if int64(len(value)) != first.Size {
					return errors.New("owned test process configuration size changed")
				}
				var last unix.Stat_t
				if err := unix.Fstat(int(file.Fd()), &last); err != nil {
					return err
				}
				if !testProcessSameStat(first, last) {
					return errors.New("owned test process configuration changed while reading")
				}
				if name == "settings.yml" {
					if err := rejectTestProcessEnvironmentSettings(value); err != nil {
						return err
					}
				}
				totalBytes += first.Size
				if collect != nil {
					collect(childPath, false, value)
				}
				fileDigest := sha256.Sum256(value)
				return encoder.Encode(struct {
					Path   string
					Kind   string
					Bytes  int64
					SHA256 string
				}{Path: childPath, Kind: "file", Bytes: first.Size, SHA256: hex.EncodeToString(fileDigest[:])})
			}()
			if entryErr != nil {
				return entryErr
			}
		}
		var after unix.Stat_t
		if err := unix.Fstat(int(current.Fd()), &after); err != nil {
			return err
		}
		if !testProcessSameStat(before, after) {
			return errors.New("owned test process configuration directory changed while reading")
		}
		return nil
	}
	if err := walk(root, ".", 0); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Hashes the complete bounded executable from its retained descriptor and
// checks both read/write state and exact expected content.
func verifyTestProcessExecutable(file *os.File, size int64, expected string) error {
	if file == nil || size <= 0 || size == math.MaxInt64 || !validTestProcessDigest(expected) {
		return errors.New("owned test executable identity is missing")
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0o022 != 0 ||
		before.Uid != uint32(os.Geteuid()) || before.Size != size {
		return errors.New("owned test executable metadata differs")
	}
	reader := io.NewSectionReader(file, 0, size+1)
	digest := sha256.New()
	count, err := io.Copy(digest, reader)
	if err != nil {
		return err
	}
	if count != size || hex.EncodeToString(digest.Sum(nil)) != expected {
		return errors.New("owned test executable content differs")
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return err
	}
	if !testProcessSameStat(before, after) {
		return errors.New("owned test executable changed while reading")
	}
	return nil
}

// Canonical private JSON admits exactly one record and no alternate spelling.
func decodeTestProcessJSON(value []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("owned test process JSON has trailing data")
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(value, canonical) {
		return errors.New("owned test process JSON is not canonical")
	}
	return nil
}
