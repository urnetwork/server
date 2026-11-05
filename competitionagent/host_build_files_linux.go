// Tracked compilation crosses the host boundary through one joined regular
// binary. Check/build caches are separate finite copies of trusted base data;
// neither arbitrary output trees nor candidate paths select root authority.
package competitionagent

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Public readback describes the exact inode copied after the kernel join.
// Build metadata is evidence to compare, never authority to execute the file.
type HostBuildBinary struct {
	Sha256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	GoVersion   string `json:"go_version"`
	GoPackage   string `json:"go_package"`
	VcsRevision string `json:"vcs_revision"`
	VcsModified string `json:"vcs_modified"`
}

// Independent check/compile copies begin with the same authenticated base
// metadata. Subsequent check writes cannot enter the compilation cache.
type HostBuildCache struct {
	Stage  string `json:"stage"`
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Files  int    `json:"files"`
}

// The complete path vocabulary is derived from the committed run root.
func hostBuildRelativePath(operation string) string {
	switch operation {
	case "prepare-build-check-cache":
		return filepath.Join(".evidence-runtime", "tracked-build", "check-cache")
	case "prepare-build-compile-cache":
		return filepath.Join(".evidence-runtime", "tracked-build", "compile-cache")
	case "prepare-build-output", "reclaim-build-output":
		return filepath.Join(".evidence-runtime", "tracked-build", "output")
	}
	return ""
}

// The ordinary handoff admission has already joined all Docker API requests
// and found no remaining containers. A separate kernel join is still required
// before consuming bytes that an orphaned compiler child could otherwise edit.
func (self *hostFileBroker) executeBuildFiles(ctx context.Context, request HostFilesRequest, relative string, result HostFilesResult) HostFilesResult {
	if self.buildJoined == nil || self.buildJoined(ctx) != nil {
		result.ErrorCode = "build_process_join_unverified"
		return result
	}
	directory, err := self.openDirectory(relative)
	if err != nil {
		result.ErrorCode = "build_directory_unavailable"
		return result
	}
	defer directory.Close()
	switch request.Operation {
	case "prepare-build-output":
		names, readErr := directory.Readdirnames(1)
		if len(names) != 0 || readErr != nil && !errors.Is(readErr, io.EOF) {
			err = errors.New("tracked build output is not fresh")
		} else if err = self.chown(directory, self.plan.HostFiles.ContainerRootUid+hostFilesContainerUid, self.plan.HostFiles.ContainerRootGid+hostFilesContainerGid); err == nil {
			err = directory.Chmod(0700)
		}
	case "prepare-build-check-cache", "prepare-build-compile-cache":
		stage := "check"
		if request.Operation == "prepare-build-compile-cache" {
			stage = "compile"
		}
		result.BuildCache, err = self.prepareBuildCache(ctx, directory, stage)
	case "reclaim-build-output":
		result.BuildBinary, err = self.reclaimBuildBinary(ctx, directory)
	default:
		err = errors.New("unrecognized tracked build handoff")
	}
	if err != nil {
		result.BuildCache, result.BuildBinary = nil, nil
		result.ErrorCode = "build_file_handoff_failed"
		return result
	}
	result.ExitCode, result.ErrorCode = 0, ""
	return result
}

// The existing module-download leaf is flat. No symlink, directory, device,
// shared hardlink or over-cap seed is converted into mapped writable input.
func (self *hostFileBroker) prepareBuildCache(ctx context.Context, directory *os.File, stage string) (*HostBuildCache, error) {
	names := []string{}
	for {
		entries, err := directory.ReadDir(self.settings.DirectoryBatch)
		for _, entry := range entries {
			if len(names) >= self.settings.MaximumBuildCacheEntries || !fs.ValidPath(entry.Name()) || entry.Name() == "." || strings.ContainsAny(entry.Name(), "/\\\x00\r\n") {
				return nil, errors.New("tracked cache exceeds its finite flat inventory")
			}
			names = append(names, entry.Name())
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	hash := sha256.New()
	var size int64
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if self.beforeOpen != nil {
			self.beforeOpen(directory, name)
		}
		file, err := openRelativeNoFollow(directory, name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		stat, ok := infoSysStat(info)
		if err != nil || !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 0 || info.Size() > self.settings.MaximumBuildCacheBytes-size {
			file.Close()
			return nil, errors.New("tracked cache contains an unsafe or oversized inode")
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00", name, info.Size())
		count, copyErr := io.Copy(hash, io.LimitReader(file, info.Size()+1))
		after, statErr := file.Stat()
		if copyErr != nil || statErr != nil || count != info.Size() || !sameHostBuildFile(info, after) {
			file.Close()
			return nil, errors.New("tracked cache changed during its descriptor read")
		}
		size += count
		if err := self.chown(file, self.plan.HostFiles.ContainerRootUid+hostFilesContainerUid, self.plan.HostFiles.ContainerRootGid+hostFilesContainerGid); err != nil {
			file.Close()
			return nil, err
		}
		err = file.Chmod(0600)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
	}
	if err := self.chown(directory, self.plan.HostFiles.ContainerRootUid+hostFilesContainerUid, self.plan.HostFiles.ContainerRootGid+hostFilesContainerGid); err != nil {
		return nil, err
	}
	if err := directory.Chmod(0700); err != nil {
		return nil, err
	}
	return &HostBuildCache{Stage: stage, Sha256: hex.EncodeToString(hash.Sum(nil)), Bytes: size, Files: len(names)}, nil
}

// The assembly directory never enters a candidate container. A new exclusive
// regular copy, synced before acknowledgment, breaks every candidate inode
// reference; all build metadata is parsed from the same held source file.
func (self *hostFileBroker) reclaimBuildBinary(ctx context.Context, directory *os.File) (*HostBuildBinary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names, readErr := directory.Readdirnames(2)
	if len(names) != 1 || names[0] != "sim-latency" || readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.New("tracked build output must contain exactly one binary")
	}
	if self.beforeOpen != nil {
		self.beforeOpen(directory, names[0])
	}
	file, err := openRelativeNoFollow(directory, names[0], os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	stat, ok := infoSysStat(info)
	if err != nil || !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() <= 0 || info.Size() > self.settings.MaximumBuildBinaryBytes {
		return nil, errors.New("tracked binary is not one bounded regular inode")
	}
	object, err := elf.NewFile(file)
	if err != nil {
		return nil, errors.New("tracked output is not a native Linux ELF binary")
	}
	defer object.Close()
	if object.Class != elf.ELFCLASS64 || object.Data != elf.ELFDATA2LSB || object.Machine != elf.EM_X86_64 ||
		object.Type != elf.ET_EXEC && object.Type != elf.ET_DYN || object.OSABI != elf.ELFOSABI_NONE && object.OSABI != elf.ELFOSABI_LINUX {
		return nil, errors.New("tracked binary changed the fixed Linux amd64 platform")
	}
	metadata, err := buildinfo.Read(file)
	if err != nil || len(metadata.GoVersion) > 64 || len(metadata.Path) > 256 {
		return nil, errors.New("tracked output has no bounded Go build metadata")
	}
	receipt := &HostBuildBinary{GoVersion: metadata.GoVersion, GoPackage: metadata.Path, Bytes: info.Size()}
	for _, setting := range metadata.Settings {
		switch setting.Key {
		case "vcs.revision":
			if receipt.VcsRevision != "" || len(setting.Value) != 40 || !isDockerProxyHex(setting.Value) {
				return nil, errors.New("tracked binary has an invalid source revision")
			}
			receipt.VcsRevision = setting.Value
		case "vcs.modified":
			if receipt.VcsModified != "" || setting.Value != "true" && setting.Value != "false" {
				return nil, errors.New("tracked binary has an invalid source modification marker")
			}
			receipt.VcsModified = setting.Value
		}
	}
	assembly, err := self.openDirectory(filepath.Join(".evidence-runtime", "tracked-build", "assembly"))
	if err != nil {
		return nil, err
	}
	defer assembly.Close()
	output, err := openRelativeNoFollow(assembly, "sim-latency", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return nil, err
	}
	defer output.Close()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(file, self.settings.MaximumBuildBinaryBytes+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || count != info.Size() || !sameHostBuildFile(info, after) || ctx.Err() != nil {
		return nil, errors.New("tracked binary changed or exceeded its completed owner")
	}
	if err := self.chown(output, self.plan.WorkerUid, self.plan.HostFiles.WorkerGid); err != nil {
		return nil, err
	}
	if err := output.Chmod(0400); err != nil {
		return nil, err
	}
	if err := output.Sync(); err != nil {
		return nil, err
	}
	if err := assembly.Sync(); err != nil {
		return nil, err
	}
	receipt.Sha256 = hex.EncodeToString(hash.Sum(nil))
	return receipt, nil
}

// A failed stat never reaches a nil interface assertion or dereference.
func infoSysStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

// Ctime catches a same-size rewrite even when a writer restores mtime.
func sameHostBuildFile(before, after os.FileInfo) bool {
	first, firstOk := infoSysStat(before)
	last, lastOk := infoSysStat(after)
	return firstOk && lastOk && os.SameFile(before, after) && before.Size() == after.Size() && first.Mtim == last.Mtim && first.Ctim == last.Ctim
}
