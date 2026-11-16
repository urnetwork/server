// The dedicated patch-retry issuer writes only a new local registration and
// separate bearer. It cannot modify the shared API credential vault.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"golang.org/x/sys/unix"
)

const operatorPatchRetryCredentialRole = "operator_patch_retry"

// Receipts contain only the registration hash and public operational scope,
// never a raw bearer, seed, request, patch or retained evaluator output.
type patchRetryCredentialReceipt struct {
	Action             string `json:"action"`
	RegistrationSha256 string `json:"registration_sha256"`
}

// This root-only action has no vault flag or HTTP/database operation. Both
// output leaves must share the same protected canonical parent directory.
func runPatchRetryCredentials(opts docopt.Opts) {
	if os.Geteuid() != 0 {
		fatalf("credentials: local patch-retry issuance requires root")
	}
	roundId, roundErr := server.ParseId(optString(opts, "--round-id", ""))
	jobId, jobErr := server.ParseId(optString(opts, "--job-id", ""))
	expiresAt, expiryErr := time.Parse(time.RFC3339Nano, optString(opts, "--expires-at", ""))
	if roundErr != nil || jobErr != nil || expiryErr != nil {
		fatalf("credentials: patch-retry scope rejected")
	}
	registrationPath, tokenPath := optString(opts, "--registration", ""), optString(opts, "--token-file", "")
	if !filepath.IsAbs(registrationPath) || filepath.Clean(registrationPath) != registrationPath ||
		!filepath.IsAbs(tokenPath) || filepath.Clean(tokenPath) != tokenPath || filepath.Dir(registrationPath) != filepath.Dir(tokenPath) {
		fatalf("credentials: patch-retry output paths rejected")
	}
	directory, err := openPatchRetryCredentialDirectory(filepath.Dir(registrationPath))
	if err != nil {
		fatalf("credentials: protected patch-retry output directory rejected")
	}
	defer directory.Close()
	receipt, err := issuePatchRetryCredential(directory, filepath.Base(registrationPath), filepath.Base(tokenPath), optString(opts, "--name", ""),
		credentialScope{Environment: optString(opts, "--environment", ""), CompetitionId: optString(opts, "--competition-id", ""),
			SeasonId: optString(opts, "--season-id", ""), RoundId: roundId, JobId: jobId, ExpiresAt: expiresAt.UTC()}, rand.Reader, server.NowUtc(), 0)
	if err != nil {
		fatalf("credentials: local patch-retry issuance refused")
	}
	if err := json.NewEncoder(os.Stdout).Encode(receipt); err != nil {
		fatalf("credentials: local patch-retry receipt unavailable")
	}
}

// Ancestor descriptors prevent symlink/path substitution. Production permits
// only root-owned directories with no other writer, including the final parent.
func openPatchRetryCredentialDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("protected directory rejected")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("protected root unavailable")
	}
	directory := os.NewFile(uintptr(fd), "/")
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		info, err := directory.Stat()
		if err != nil || !patchRetryCredentialDirectoryInfo(info, 0) {
			directory.Close()
			return nil, errors.New("protected ancestor rejected")
		}
		next, err := unix.Openat(int(directory.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		directory.Close()
		if err != nil {
			return nil, errors.New("protected directory unavailable")
		}
		directory = os.NewFile(uintptr(next), component)
	}
	info, err := directory.Stat()
	if err != nil || !patchRetryCredentialDirectoryInfo(info, 0) {
		directory.Close()
		return nil, errors.New("protected directory rejected")
	}
	return directory, nil
}

// Tests supply their actual owner only after opening an isolated directory;
// production always passes root through the protected ancestor walk above.
func patchRetryCredentialDirectoryInfo(info os.FileInfo, owner uint32) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == owner
}

// Invalid scope/expiry cannot consume entropy or create files. Exclusive leaf
// creation prevents overwrite, including a pre-existing symlink or hard link.
func issuePatchRetryCredential(directory *os.File, registrationName, tokenName, name string, scope credentialScope, random io.Reader, now time.Time, owner uint32) (*patchRetryCredentialReceipt, error) {
	if directory == nil || now.IsZero() || !now.Before(scope.ExpiresAt) || validateCredentialCapability(operatorPatchRetryCredentialRole, scope) != nil ||
		registrationName == tokenName || filepath.Base(registrationName) != registrationName || filepath.Base(tokenName) != tokenName ||
		registrationName == "" || registrationName == "." || registrationName == ".." || tokenName == "" || tokenName == "." || tokenName == ".." {
		return nil, errors.New("patch-retry issuance scope rejected")
	}
	info, err := directory.Stat()
	if err != nil || !patchRetryCredentialDirectoryInfo(info, owner) {
		return nil, errors.New("patch-retry output ownership rejected")
	}
	generated, err := generateScopedCredentialToken(name, operatorPatchRetryCredentialRole, scope, random)
	if err != nil {
		return nil, errors.New("patch-retry credential generation failed")
	}
	registration := controller.CompetitionOperatorPatchRetryCredential{Schema: 1, Kind: controller.CompetitionOperatorPatchRetryCredentialKind,
		Capability: model.CompetitionOperatorRequestedPatchRetryCapability, Name: generated.Name, Role: generated.Role, Sha256: generated.Sha256,
		Environment: scope.Environment, CompetitionId: scope.CompetitionId, SeasonId: scope.SeasonId, RoundId: scope.RoundId, JobId: scope.JobId,
		IssuedAt: now.UTC(), ExpiresAt: scope.ExpiresAt.UTC()}
	content, err := json.Marshal(registration)
	if err != nil {
		return nil, errors.New("patch-retry registration encoding failed")
	}
	content = append(content, '\n')
	token := append([]byte(generated.Token), '\n')
	defer clear(token)
	created := []string{}
	committed := false
	defer func() {
		if !committed {
			for _, leaf := range created {
				_ = unix.Unlinkat(int(directory.Fd()), leaf, 0)
			}
		}
	}()
	for _, output := range []struct {
		name    string
		content []byte
	}{{name: registrationName, content: content}, {name: tokenName, content: token}} {
		fd, err := unix.Openat(int(directory.Fd()), output.name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return nil, errors.New("patch-retry output already exists or is unavailable")
		}
		created = append(created, output.name)
		file := os.NewFile(uintptr(fd), output.name)
		_, writeErr := file.Write(output.content)
		syncErr := file.Sync()
		closeErr := file.Close()
		if errors.Join(writeErr, syncErr, closeErr) != nil {
			return nil, errors.New("patch-retry output write failed")
		}
	}
	if err := directory.Sync(); err != nil {
		return nil, errors.New("patch-retry output directory sync failed")
	}
	committed = true
	digest := sha256.Sum256(content)
	return &patchRetryCredentialReceipt{Action: "issue-patch-retry", RegistrationSha256: hex.EncodeToString(digest[:])}, nil
}
