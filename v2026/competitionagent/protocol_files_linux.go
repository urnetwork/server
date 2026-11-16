// Linux file exchange retains descriptor-relative no-follow ownership. The
// signed envelope remains portable, but no weaker platform backend is implied.
package competitionagent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Reads only regular files under a trusted root and verifies the streamed bytes.
// Callers own the root lifecycle and keep it inaccessible to candidates.
func WriteFiles(writer io.Writer, root string, files []File, maximum int64, limits Limits) error {
	if err := ValidateFiles(files, maximum, limits); err != nil {
		return err
	}
	for _, file := range files {
		input, err := openRegular(root, file.Path)
		if err != nil {
			return err
		}
		digest := sha256.New()
		count, copyErr := io.CopyN(io.MultiWriter(writer, digest), input, file.Bytes)
		info, statErr := input.Stat()
		closeErr := input.Close()
		if copyErr != nil || statErr != nil || closeErr != nil || info.Size() != file.Bytes || count != file.Bytes || hex.EncodeToString(digest.Sum(nil)) != file.Sha256 {
			return errors.New("subevaluation source file failed authentication")
		}
	}
	return nil
}

// Exclusive creation in a fresh private directory prevents replacing prior
// evidence. No filesystem permissions, owners or links come from the peer.
func ReadFiles(reader io.Reader, root string, files []File, maximum int64, limits Limits) error {
	if err := ValidateFiles(files, maximum, limits); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm()&0077 != 0 {
		return errors.New("subevaluation destination must be a fresh private directory")
	}
	destination, err := openDirectoryNoFollow(root)
	if err != nil {
		return err
	}
	defer destination.Close()
	openedInfo, err := destination.Stat()
	if err != nil || !os.SameFile(rootInfo, openedInfo) {
		return errors.New("subevaluation destination changed during capture")
	}
	entries, readErr := destination.ReadDir(1)
	if len(entries) != 0 || !errors.Is(readErr, io.EOF) {
		return errors.New("subevaluation destination must be empty before transfer")
	}
	for _, file := range files {
		parent := filepath.Dir(file.Path)
		if parent != "." {
			if err := mkdirRelativeNoFollow(destination, parent, 0700); err != nil {
				return err
			}
		}
		if err := safeAncestors(root, file.Path); err != nil {
			return err
		}
		output, err := openRelativeNoFollow(destination, file.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		digest := sha256.New()
		count, copyErr := io.CopyN(io.MultiWriter(output, digest), reader, file.Bytes)
		syncErr := output.Sync()
		closeErr := output.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
		if count != file.Bytes || hex.EncodeToString(digest.Sum(nil)) != file.Sha256 {
			return errors.Join(ErrIntegrity, errors.New("subevaluation received file failed authentication"))
		}
		directory, err := openRelativeNoFollow(destination, parent, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
			return err
		}
	}
	return nil
}

// Rejects symlink parents even for callers which supplied a pre-existing root.
func safeAncestors(root, name string) error {
	current := root
	parts := strings.Split(filepath.ToSlash(filepath.Dir(name)), "/")
	for _, part := range parts {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe subevaluation parent directory")
		}
	}
	return nil
}

// O_NOFOLLOW closes the leaf race after validating every protected ancestor.
func openRegular(root, name string) (*os.File, error) {
	if err := safeAncestors(root, name); err != nil {
		return nil, err
	}
	directory, err := openDirectoryNoFollow(root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	file, err := openRelativeNoFollow(directory, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("subevaluation input is not a regular file")
	}
	return file, nil
}
