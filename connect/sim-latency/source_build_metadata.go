// Module metadata crosses the container boundary as bounded regular-file data,
// never a Docker archive extracted blindly by a privileged host client.
package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Only flat cache entries are valid at a module's @v directory. No archive
// directory, symlink, hardlink, device or path escape can gain a host write.
func decodeSourceBuildMetadata(content []byte, settings sourceBuildSandboxSettings) (map[string][]byte, error) {
	if settings.MetadataBytes <= 0 || settings.MetadataFiles <= 0 || int64(len(content)) > settings.MetadataBytes {
		return nil, errors.New("source module metadata exceeds its bounded archive contract")
	}
	reader := tar.NewReader(bytes.NewReader(content))
	entries := map[string][]byte{}
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("source module metadata archive is malformed")
		}
		if (header.Name == "." || header.Name == "./") && header.Typeflag == tar.TypeDir {
			continue
		}
		name := strings.TrimPrefix(header.Name, "./")
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "\\\x00\r\n") ||
			header.Typeflag != tar.TypeReg || header.Linkname != "" || header.Size < 0 || header.Size > settings.MetadataBytes-total || len(entries) >= settings.MetadataFiles {
			return nil, errors.New("source module metadata contains an unsafe entry or exceeds its budget")
		}
		if _, exists := entries[name]; exists {
			return nil, errors.New("source module metadata contains duplicate entries")
		}
		data, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil || int64(len(data)) != header.Size {
			return nil, errors.New("source module metadata entry is truncated")
		}
		entries[name], total = data, total+header.Size
	}
	if len(entries) == 0 {
		return nil, errors.New("source module metadata archive has no regular entries")
	}
	return entries, nil
}

// Independent phase seeds are exclusively allocated and then sealed readable
// for the container UID. Check-phase writes never flow into compilation.
func writeSourceBuildMetadata(directory string, entries map[string][]byte) error {
	if len(entries) == 0 {
		return errors.New("source module metadata seed is empty")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return err
	}
	for name, data := range entries {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "\\\x00\r\n") {
			return errors.New("source module metadata filename is invalid")
		}
		path := filepath.Join(directory, name)
		if err := writePrivateReviewFile(path, data); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o444); err != nil {
			return err
		}
	}
	return os.Chmod(directory, 0o555)
}
