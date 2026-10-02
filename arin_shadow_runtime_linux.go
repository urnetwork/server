package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func LoadArinShadowRuntimeConfig() (*ArinShadowRuntimeConfig, error) {
	path := os.Getenv("ARIN_SHADOW_CAPTURE_CONFIG")
	if path == "" {
		// RunWorker's scoped secret_files mount does not imply a whole
		// Vault mount or set a new environment variable for each secret.
		const scoped = "/srv/warp/secrets/arin-shadow-capture.json"
		if _, err := os.Lstat(scoped); err == nil {
			return loadArinShadowRuntimeConfig(scoped)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, ErrArinShadowInput
		}
		var err error
		path, err = Vault.ResourcePath("arin-shadow-capture.json")
		if errors.Is(err, ErrResourceNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, ErrArinShadowInput
		}
	}
	return loadArinShadowRuntimeConfig(path)
}

func loadArinShadowRuntimeConfig(path string) (*ArinShadowRuntimeConfig, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrArinShadowInput
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return nil, ErrArinShadowInput
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
		return nil, ErrArinShadowInput
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrArinShadowInput
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, ErrArinShadowInput
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, ErrArinShadowInput
	}
	var config ArinShadowRuntimeConfig
	if DecodeArinShadowRPC(data, &config) != nil || config.RunId == (Id{}) || !arinShadowHex(config.KeyHex, 32) || config.Capacity < 1 || config.Capacity > ArinShadowCapturePopulationLimit || config.ExpiresAt.IsZero() || config.ExpiresAt.After(NowUtc().Add(4*time.Hour)) {
		return nil, ErrArinShadowInput
	}
	// A completed operator lease must not prevent a later service restart.
	if !config.ExpiresAt.After(NowUtc()) {
		return nil, nil
	}
	for _, item := range []struct {
		path     *string
		resource string
	}{{&config.ActivePath, config.ActiveResource}, {&config.CandidatePath, config.CandidateResource}} {
		if item.resource != "" {
			if *item.path != "" || filepath.IsAbs(item.resource) || filepath.Clean(item.resource) != item.resource || item.resource == ".." || strings.HasPrefix(item.resource, "../") {
				return nil, ErrArinShadowInput
			}
			resolved, err := Config.ResourcePath(item.resource)
			if err != nil {
				return nil, ErrArinShadowInput
			}
			*item.path = resolved
		}
	}
	return &config, nil
}

// Starts one serial, authenticated Unix listener under a private operator
// directory. Callbacks, listener and cleanup are joined on expiration/close.
// Each process has its own endpoint, so old draining generations remain visible.
func StartArinShadowRuntime(ctx context.Context, config *ArinShadowRuntimeConfig, role string, factory func(context.Context) (ArinShadowRPCHandler, func(), error)) (*ArinShadowRuntime, error) {
	if config == nil {
		return nil, nil
	}
	source, ok := currentSourceBuildInfo()
	if !ok {
		return nil, ErrArinShadowInput
	}
	return startArinShadowRuntime(ctx, config, role, factory, source)
}

func startArinShadowRuntime(ctx context.Context, config *ArinShadowRuntimeConfig, role string, factory func(context.Context) (ArinShadowRPCHandler, func(), error), source sourceBuildInfo) (*ArinShadowRuntime, error) {
	if config == nil || ctx == nil || ctx.Err() != nil || factory == nil || !filepath.IsAbs(config.Directory) || filepath.Clean(config.Directory) != config.Directory {
		return nil, ErrArinShadowInput
	}
	info, err := os.Lstat(config.Directory)
	if os.IsNotExist(err) {
		if os.Mkdir(config.Directory, 0700) != nil {
			return nil, ErrArinShadowInput
		}
		info, err = os.Lstat(config.Directory)
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrArinShadowInput
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrArinShadowInput
	}
	keyBytes, err := hex.DecodeString(config.KeyHex)
	if err != nil || len(keyBytes) != 32 {
		return nil, ErrArinShadowInput
	}
	var key [32]byte
	copy(key[:], keyBytes)
	bounded, cancel := context.WithDeadline(ctx, config.ExpiresAt)
	handler, cleanup, err := factory(bounded)
	if err != nil {
		cancel()
		return nil, ErrArinShadowInput
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	dir, err := os.MkdirTemp(config.Directory, role+"-")
	if err != nil {
		cancel()
		cleanup()
		return nil, ErrArinShadowInput
	}
	remove := func() {
		os.Remove(filepath.Join(dir, "identity.json"))
		os.Remove(filepath.Join(dir, "capture.sock"))
		os.Remove(dir)
	}
	identity := ArinShadowRPCIdentity{ProcessNonce: NewId(), StartedAt: NowUtc(), Revision: source.revision, Modified: source.modified, ImageDigest: os.Getenv("WARP_IMAGE_DIGEST"), Role: role}
	identity.Version, _ = Version()
	identity.Environment, _ = Env()
	identity.Host, _ = Host()
	identity.Block, _ = Block()
	service, err := NewArinShadowRPCService(bounded, config.RunId, key, identity, handler)
	if err != nil {
		cancel()
		cleanup()
		remove()
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "capture.sock"), Net: "unix"})
	if err != nil {
		cancel()
		cleanup()
		remove()
		return nil, ErrArinShadowInput
	}
	if os.Chmod(filepath.Join(dir, "capture.sock"), 0600) != nil {
		listener.Close()
		cancel()
		cleanup()
		remove()
		return nil, ErrArinShadowInput
	}
	encoded, _ := json.Marshal(identity)
	file, err := os.OpenFile(filepath.Join(dir, "identity.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = file.Write(encoded)
		if err == nil {
			err = file.Sync()
		}
		file.Close()
	}
	if err != nil {
		listener.Close()
		cancel()
		cleanup()
		remove()
		return nil, ErrArinShadowInput
	}
	runtime := &ArinShadowRuntime{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(runtime.done)
		defer remove()
		defer cleanup()
		defer cancel()
		defer listener.Close()
		stop := context.AfterFunc(bounded, func() { listener.Close() })
		defer stop()
		for bounded.Err() == nil {
			connection, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			serveArinShadowConnection(bounded, service, connection)
		}
	}()
	return runtime, nil
}

func serveArinShadowConnection(ctx context.Context, service *ArinShadowRPCService, connection net.Conn) {
	defer connection.Close()
	bounded, cancel := context.WithTimeout(ctx, ArinShadowRPCCallTimeout)
	defer cancel()
	deadline, _ := bounded.Deadline()
	connection.SetDeadline(deadline)
	stop := context.AfterFunc(bounded, func() { connection.Close() })
	defer stop()
	packet, err := ReadArinShadowRPCFrame(connection, ArinShadowRPCRequestLimit)
	if err != nil {
		return
	}
	reply, err := service.Handle(bounded, packet)
	if err != nil {
		return
	}
	WriteArinShadowRPCFrame(connection, reply, ArinShadowRPCResponseLimit)
}
