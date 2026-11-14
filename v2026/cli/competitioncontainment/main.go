//go:build linux

// Root-only read-only evidence authentication. Only the final compact binding
// is written; no production environment, service, or evaluation is started.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// Every leaf is opened beneath one held directory, refusing all symlinks and
// nonregular files. Signed size limits apply before reading any file bytes.
type containmentFileSource struct{ root *os.File }

// Hold each ancestor descriptor: leaf-only no-follow does not prevent a
// symlink or rename in an intermediate directory from redirecting authority.
func containmentDirectory(path string, protected bool) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid containment directory")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
		if protected {
			var stat syscall.Stat_t
			if err := syscall.Fstat(fd, &stat); err != nil {
				_ = syscall.Close(fd)
				return nil, err
			}
			if stat.Uid != 0 || stat.Mode&0022 != 0 {
				_ = syscall.Close(fd)
				return nil, errors.New("authority ancestor is not root protected")
			}
		}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (self containmentFileSource) open(name string) (*os.File, error) {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00\r\n") {
		return nil, errors.New("invalid artifact path")
	}
	parent, err := syscall.Dup(int(self.root.Fd()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(parent) }()
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := syscall.Openat(parent, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		_ = syscall.Close(parent)
		parent = next
	}
	fd, err := syscall.Openat(parent, parts[len(parts)-1], syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("artifact is not regular")
	}
	return file, nil
}

func (self containmentFileSource) Verify(expected competitionagent.File) error {
	file, err := self.open(expected.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(file, expected.Bytes+1))
	if err != nil || count != expected.Bytes || hex.EncodeToString(digest.Sum(nil)) != expected.Sha256 {
		return errors.New("signed artifact bytes differ")
	}
	return nil
}

func (self containmentFileSource) Read(name string, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("document bound is invalid")
	}
	file, err := self.open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("artifact document exceeds its bound")
	}
	return content, nil
}

// Authority documents are selected by the root operator, not by a submission.
func protectedContainmentDocument(path string, maximum int64) ([]byte, error) {
	if maximum <= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid authority path")
	}
	parent, err := containmentDirectory(filepath.Dir(path), true)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := syscall.Openat(int(parent.Fd()), filepath.Base(path), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("authority file is not root protected")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("authority file exceeds its bound")
	}
	return content, nil
}

func run(args []string) error {
	if os.Geteuid() != 0 {
		return errors.New("root operator required")
	}
	flags := flag.NewFlagSet("competitioncontainment", flag.ContinueOnError)
	configPath := flags.String("config", "", "protected local agent role")
	manifestPath := flags.String("manifest", "", "exact operator start manifest")
	terminalPath := flags.String("terminal-response", "", "signed terminal response")
	artifactsPath := flags.String("artifact-directory", "", "authenticated artifact root")
	outputPath := flags.String("output", "", "new protected binding path")
	patchSha := flags.String("trusted-patch-sha256", "", "approved operator patch digest")
	baselineSha := flags.String("trusted-baseline-sha256", "", "approved control receipt digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *manifestPath == "" || *terminalPath == "" || *artifactsPath == "" || *outputPath == "" {
		return errors.New("all evidence paths are required")
	}
	config, err := competitionagent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	manifestBytes, err := protectedContainmentDocument(*manifestPath, config.Limits.DocumentBytes)
	if err != nil {
		return err
	}
	terminalBytes, err := protectedContainmentDocument(*terminalPath, config.Limits.DocumentBytes)
	if err != nil {
		return err
	}
	boundaryBytes, err := protectedContainmentDocument(config.DockerRole.ResourceBoundary.Path, config.Limits.DocumentBytes)
	if err != nil {
		return err
	}
	boundarySha := sha256.Sum256(boundaryBytes)
	if hex.EncodeToString(boundarySha[:]) != config.DockerRole.ResourceBoundary.Sha256 {
		return errors.New("boundary pin changed")
	}
	var manifest competitionagent.StartManifest
	var response competitionagent.Response
	var boundary competitionagent.RuntimeResourceBoundary
	if json.Unmarshal(manifestBytes, &manifest) != nil || json.Unmarshal(terminalBytes, &response) != nil || json.Unmarshal(boundaryBytes, &boundary) != nil {
		return errors.New("evidence document is invalid")
	}
	keyBytes, err := protectedContainmentDocument(config.SigningKeyPath, competitionagent.DefaultAgentSettings().ConfigBytes)
	if err != nil {
		return err
	}
	defer clear(keyBytes)
	key, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(keyBytes)))
	if err != nil {
		return errors.New("agent key encoding is invalid")
	}
	defer clear(key)
	var private ed25519.PrivateKey
	switch len(key) {
	case ed25519.SeedSize:
		private = ed25519.NewKeyFromSeed(key)
	case ed25519.PrivateKeySize:
		private = append(ed25519.PrivateKey(nil), key...)
	default:
		return errors.New("agent key length is invalid")
	}
	defer clear(private)
	root, err := containmentDirectory(*artifactsPath, false)
	if err != nil {
		return err
	}
	defer root.Close()
	binding, err := privateContainmentBinding(*config, boundary, manifest, response, private.Public().(ed25519.PublicKey), *patchSha, *baselineSha, containmentFileSource{root: root})
	if err != nil {
		return err
	}
	configBytes, err := protectedContainmentDocument(*configPath, config.Limits.DocumentBytes)
	if err != nil {
		return err
	}
	configSha := sha256.Sum256(configBytes)
	binding["agent_config_sha256"] = hex.EncodeToString(configSha[:])
	content, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if !filepath.IsAbs(*outputPath) || filepath.Clean(*outputPath) != *outputPath {
		return errors.New("binding parent is not protected")
	}
	parent, err := containmentDirectory(filepath.Dir(*outputPath), true)
	if err != nil {
		return err
	}
	defer parent.Close()
	fd, err := syscall.Openat(int(parent.Fd()), filepath.Base(*outputPath), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	output := os.NewFile(uintptr(fd), *outputPath)
	_, writeErr := output.Write(content)
	syncErr := output.Sync()
	closeErr := output.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "private containment authentication failed")
		os.Exit(1)
	}
}
