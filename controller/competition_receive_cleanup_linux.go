//go:build linux

// Linux housekeeping uses held private creation descriptors and no-follow
// relative operations. Trusted-root concurrent writers are excluded; POSIX
// unlink is not claimed atomic against a malicious host root. Full preflight
// precedes deletion; later uncertainty stops work and preserves what remains.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type competitionReceiveFileIdentity struct {
	inode, mountId, size     uint64
	major, minor, uid, links uint32
	mode                     uint16
	modified, changed        unix.StatxTimestamp
}

type competitionReceiveNative struct {
	root, child                 *os.File
	rootPath, childName         string
	rootIdentity, childIdentity competitionReceiveFileIdentity
	stat                        func(*os.File) (competitionReceiveFileIdentity, error)
	// Nil in production. Private tests force a preflight/delete ordering.
	beforeDelete func()
	finished     bool
}

type competitionReceiveTreeEntry struct {
	path      string
	identity  competitionReceiveFileIdentity
	directory bool
	remove    bool
}

// Captured before transport can populate the controller's newly created child.
func captureCompetitionReceiveNative(root, directory, runId string) (competitionReceiveNativeCustody, error) {
	name := filepath.Base(directory)
	if !filepath.IsAbs(root) || root == "/" || filepath.Clean(root) != root || filepath.Dir(directory) != root || filepath.Join(root, name) != directory ||
		!sha256Pattern.MatchString(runId) || !strings.HasPrefix(name, ".executor-receive-"+runId+"-") || name == ".executor-receive-"+runId+"-" {
		return nil, errors.New("receive creation path is not the controller's exact private child")
	}
	parent, err := openFailureArtifactPath(root, "", true)
	if err != nil {
		return nil, err
	}
	child, err := competitionReceiveOpenat(parent, name, true)
	if err != nil {
		parent.Close()
		return nil, err
	}
	self := &competitionReceiveNative{root: parent, child: child, rootPath: root, childName: name, stat: statCompetitionReceiveFile}
	self.rootIdentity, err = self.stat(parent)
	if err == nil {
		self.childIdentity, err = self.stat(child)
	}
	if err == nil && (!self.safe(self.rootIdentity, true) || !self.safe(self.childIdentity, true)) {
		err = errors.New("receive creation lost private same-mount custody")
	}
	if err == nil {
		entries, readErr := child.ReadDir(1)
		if len(entries) != 0 || readErr != io.EOF {
			err = errors.New("receive creation custody cannot adopt an already populated tree")
		}
	}
	if err != nil {
		self.Close()
		return nil, err
	}
	return self, nil
}

// Native mount IDs distinguish bind mounts even on the same backing device.
func statCompetitionReceiveFile(file *os.File) (competitionReceiveFileIdentity, error) {
	var value unix.Statx_t
	if file == nil || unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &value) != nil ||
		value.Mask&(unix.STATX_BASIC_STATS|unix.STATX_MNT_ID) != unix.STATX_BASIC_STATS|unix.STATX_MNT_ID || value.Mnt_id == 0 {
		return competitionReceiveFileIdentity{}, errors.New("receive has no native inode/mount observation")
	}
	return competitionReceiveFileIdentity{inode: value.Ino, mountId: value.Mnt_id, major: value.Dev_major, minor: value.Dev_minor, uid: value.Uid,
		links: value.Nlink, mode: value.Mode, size: value.Size, modified: value.Mtime, changed: value.Ctime}, nil
}

func competitionReceiveOpenat(parent *os.File, name string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Only private process-owned regular files/directories on the original mount
// can be examined. Directory link counts legitimately change as children go.
func (self *competitionReceiveNative) safe(value competitionReceiveFileIdentity, directory bool) bool {
	if value.inode == 0 || value.uid != uint32(os.Geteuid()) || value.mode&0077 != 0 || value.mode&07000 != 0 || value.mountId != self.rootIdentity.mountId || value.major != self.rootIdentity.major || value.minor != self.rootIdentity.minor {
		return false
	}
	if directory {
		return value.mode&unix.S_IFMT == unix.S_IFDIR && value.mode&0500 == 0500
	}
	return value.mode&unix.S_IFMT == unix.S_IFREG && value.links == 1
}

func sameCompetitionReceiveDirectory(left, right competitionReceiveFileIdentity) bool {
	return left.inode == right.inode && left.mountId == right.mountId && left.major == right.major && left.minor == right.minor && left.uid == right.uid && left.mode&unix.S_IFMT == right.mode&unix.S_IFMT
}

// Both current pathnames must still name their held original generations.
func (self *competitionReceiveNative) binding(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := openFailureArtifactPath(self.rootPath, "", true)
	if err != nil {
		return err
	}
	root, err := self.stat(current)
	current.Close()
	if err != nil || !self.safe(root, true) || !sameCompetitionReceiveDirectory(root, self.rootIdentity) {
		return errors.New("receive root generation changed")
	}
	current, err = competitionReceiveOpenat(self.root, self.childName, true)
	if err != nil {
		return err
	}
	child, err := self.stat(current)
	current.Close()
	if err != nil || !self.safe(child, true) || !sameCompetitionReceiveDirectory(child, self.childIdentity) {
		return errors.New("receive child generation changed")
	}
	return ctx.Err()
}

// At most depth+constant descriptors are open during the full preflight;
// deletion reopens one bounded path at a time, never the whole inventory.
func (self *competitionReceiveNative) Cleanup(ctx context.Context, settings competitionReceiveCleanupSettings, files map[string]competitionReceiveFile) error {
	if self == nil || self.root == nil || self.child == nil {
		return errors.New("receive creation descriptors are closed")
	}
	if self.finished {
		return nil
	}
	directories, err := competitionReceiveDirectories(files, settings)
	if err != nil {
		return err
	}
	if err := self.binding(ctx); err != nil {
		return err
	}
	entries := []competitionReceiveTreeEntry{}
	identities := map[string]competitionReceiveFileIdentity{"": self.childIdentity}
	preservedDirectories := map[string]bool{}
	for path, file := range files {
		if !file.remove {
			for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
				if parent == "." {
					preservedDirectories[""] = true
					break
				}
				preservedDirectories[parent] = true
			}
		}
	}
	visited, foundFiles := 1, 0
	var scan func(*os.File, string, int) error
	scan = func(directory *os.File, relative string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > settings.MaximumDepth {
			return errors.New("receive tree exceeded its cleanup depth budget")
		}
		children, err := directory.ReadDir(settings.MaximumEntries - visited + 1)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(children) > settings.MaximumEntries-visited {
			return errors.New("receive tree exceeded its cleanup count budget")
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(relative, child.Name())
			if len(path) > settings.MaximumPathBytes {
				return errors.New("receive path exceeded its cleanup byte budget")
			}
			visited++
			if visited > settings.MaximumEntries {
				return errors.New("receive tree exceeded its cleanup count budget")
			}
			file, err := competitionReceiveOpenat(directory, child.Name(), child.IsDir())
			if err != nil {
				return err
			}
			identity, err := self.stat(file)
			isDirectory := identity.mode&unix.S_IFMT == unix.S_IFDIR
			if err == nil && !self.safe(identity, isDirectory) {
				err = errors.New("receive entry has foreign, linked, special or mounted custody")
			}
			if err == nil {
				if isDirectory {
					if !directories[path] {
						err = errors.New("receive acquired an undeclared directory")
					} else {
						identities[path] = identity
						err = scan(file, path, depth+1)
					}
				} else if expected, present := files[path]; !present || expected.bytes < 0 || identity.size != uint64(expected.bytes) {
					err = errors.New("receive file differs from its exact owned inventory")
				} else {
					digest := sha256.New()
					var count int64
					count, err = io.Copy(digest, io.LimitReader(&competitionReceiveContextReader{ctx: ctx, file: file}, expected.bytes+1))
					if err == nil && (count != expected.bytes || hex.EncodeToString(digest.Sum(nil)) != expected.sha256) {
						err = errors.New("receive bytes differ from their independent authenticated hash")
					}
					after, statErr := self.stat(file)
					if err == nil && (statErr != nil || after != identity) {
						err = errors.New("receive file changed around its held hash proof")
					}
					if err == nil {
						foundFiles++
					}
				}
			}
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
			remove := !preservedDirectories[path]
			if !isDirectory {
				remove = files[path].remove
			}
			entries = append(entries, competitionReceiveTreeEntry{path: path, identity: identity, directory: isDirectory, remove: remove})
		}
		return nil
	}
	fresh, err := competitionReceiveOpenat(self.child, ".", true)
	if err != nil {
		return err
	}
	err = scan(fresh, "", 0)
	closeErr := fresh.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if foundFiles != len(files) || len(identities) != len(directories) {
		return errors.New("receive inventory is incomplete")
	}
	if self.beforeDelete != nil {
		self.beforeDelete()
	}
	// No file has been removed before every preflight check above succeeds.
	for _, entry := range entries {
		if !entry.remove {
			continue
		}
		if err := self.binding(ctx); err != nil {
			return err
		}
		parent, err := self.openParent(ctx, filepath.Dir(entry.path), identities)
		if err != nil {
			return err
		}
		current, err := competitionReceiveOpenat(parent, filepath.Base(entry.path), entry.directory)
		if err != nil {
			parent.Close()
			return err
		}
		identity, err := self.stat(current)
		current.Close()
		if err == nil && (!self.safe(identity, entry.directory) || entry.directory && !sameCompetitionReceiveDirectory(identity, entry.identity) || !entry.directory && identity != entry.identity) {
			err = errors.New("receive entry changed after complete preflight")
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			flags := 0
			if entry.directory {
				flags = unix.AT_REMOVEDIR
			}
			err = unix.Unlinkat(int(parent.Fd()), filepath.Base(entry.path), flags)
		}
		parent.Close()
		if err != nil {
			return err
		}
	}
	if err := self.binding(ctx); err != nil {
		return err
	}
	if !preservedDirectories[""] {
		if err := unix.Unlinkat(int(self.root.Fd()), self.childName, unix.AT_REMOVEDIR); err != nil {
			return err
		}
	}
	self.finished = true
	return self.root.Sync()
}

// Regular-file reads observe the same finite cleanup owner between chunks.
type competitionReceiveContextReader struct {
	ctx  context.Context
	file *os.File
}

func (self *competitionReceiveContextReader) Read(content []byte) (int, error) {
	if err := self.ctx.Err(); err != nil {
		return 0, err
	}
	return self.file.Read(content)
}

// Every ancestor is reauthenticated, so a held descriptor renamed outside the
// original child does not grant authority over a newly substituted pathname.
func (self *competitionReceiveNative) openParent(ctx context.Context, relative string, identities map[string]competitionReceiveFileIdentity) (*os.File, error) {
	current, err := competitionReceiveOpenat(self.child, ".", true)
	if err != nil {
		return nil, err
	}
	if relative == "." {
		return current, nil
	}
	path := ""
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if err := ctx.Err(); err != nil {
			current.Close()
			return nil, err
		}
		path = filepath.Join(path, component)
		next, err := competitionReceiveOpenat(current, component, true)
		current.Close()
		if err != nil {
			return nil, err
		}
		identity, err := self.stat(next)
		if err != nil || !self.safe(identity, true) || !sameCompetitionReceiveDirectory(identity, identities[path]) {
			next.Close()
			return nil, errors.New("receive ancestor changed after preflight")
		}
		current = next
	}
	return current, nil
}

// Idempotently release both original descriptors without filesystem mutation.
func (self *competitionReceiveNative) Close() error {
	var rootErr, childErr error
	if self.child != nil {
		childErr = self.child.Close()
		self.child = nil
	}
	if self.root != nil {
		rootErr = self.root.Close()
		self.root = nil
	}
	return errors.Join(rootErr, childErr)
}
