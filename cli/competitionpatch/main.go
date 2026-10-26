// competitionpatch revalidates the exact patch bytes used to build a
// competition submission image. The immutable trusted builder release supplies
// its pinned binary independently of candidate files and old base images.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"

	"github.com/urnetwork/server/controller"
)

const maxPolicyBytes = 1024 * 1024

var (
	gitShaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type options struct {
	baseSha                      string
	expectedPatchSha256          string
	expectedPolicySha256         string
	patchPath                    string
	policyPath                   string
	repositoryPatchDirectory     string
	sourceRoot                   string
	sourceIdentityPath           string
	expectedSourceIdentitySha256 string
}

type imageIdentity struct {
	Schema            int                                `json:"schema"`
	BaseSha           string                             `json:"base_sha"`
	PatchSha256       string                             `json:"patch_sha256"`
	PolicySha256      string                             `json:"policy_sha256"`
	Paths             []string                           `json:"paths"`
	RepositoryPatches map[string]repositoryPatchIdentity `json:"repository_patches,omitempty"`
	CandidateCommits  map[string]string                  `json:"candidate_commits,omitempty"`
}

// Digests bind routed bytes to the original screened patch and policy receipt.
type repositoryPatchIdentity struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(competitionPatchExitCode(err))
	}
}

// Canonical patch refusals remain exit one. Trusted source/application failure
// is infrastructure, never unsupported evidence of candidate wrongdoing.
func competitionPatchExitCode(err error) int {
	var applicationError *repositoryApplicationError
	if errors.As(err, &applicationError) {
		return 2
	}
	return 1
}

func run(args []string, stdout io.Writer) error {
	return runContext(context.Background(), args, stdout)
}

// Signal/deadline ownership continues through the complete source application.
func runContext(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("competitionpatch", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts options
	flags.StringVar(&opts.baseSha, "base-sha", "", "trusted base Git SHA")
	flags.StringVar(&opts.expectedPatchSha256, "expected-patch-sha256", "", "expected canonical patch SHA-256")
	flags.StringVar(&opts.expectedPolicySha256, "expected-policy-sha256", "", "expected policy-file SHA-256")
	flags.StringVar(&opts.patchPath, "patch", "", "canonical patch path")
	flags.StringVar(&opts.policyPath, "policy", "", "patch policy JSON path")
	flags.StringVar(&opts.repositoryPatchDirectory, "repository-patch-directory", "", "exclusively create private routed patch directory")
	flags.StringVar(&opts.sourceRoot, "apply-source-root", "", "apply validated patches only to authenticated private source checkouts")
	flags.StringVar(&opts.sourceIdentityPath, "source-identity", "", "trusted original source identity JSON")
	flags.StringVar(&opts.expectedSourceIdentitySha256, "expected-source-identity-sha256", "", "exact original source identity digest")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("usage: competitionpatch --base-sha SHA --patch FILE --expected-patch-sha256 SHA256 --policy FILE --expected-policy-sha256 SHA256")
	}
	if !gitShaPattern.MatchString(opts.baseSha) {
		return errors.New("base SHA must be exactly 40 lowercase hexadecimal characters")
	}
	if !sha256Pattern.MatchString(opts.expectedPatchSha256) || !sha256Pattern.MatchString(opts.expectedPolicySha256) {
		return errors.New("expected patch and policy SHA-256 values must be exactly 64 lowercase hexadecimal characters")
	}

	policyBytes, err := readRegularFile(opts.policyPath, maxPolicyBytes)
	if err != nil {
		return fmt.Errorf("read patch policy: %w", err)
	}
	policyDigest := sha256.Sum256(policyBytes)
	if actual := hex.EncodeToString(policyDigest[:]); actual != opts.expectedPolicySha256 {
		return fmt.Errorf("patch policy SHA-256 mismatch: got %s", actual)
	}
	var policy controller.PatchPolicy
	decoder := json.NewDecoder(bytes.NewReader(policyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return fmt.Errorf("decode patch policy: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("patch policy must contain exactly one JSON value")
	}
	if policy.MaxPatchBytes <= 0 || 262144 < policy.MaxPatchBytes || len(policy.AllowedPaths) == 0 || len(policy.ForbiddenPaths) == 0 {
		return errors.New("patch policy is incomplete or outside frozen limits")
	}

	patchBytes, err := readRegularFile(opts.patchPath, int64(policy.MaxPatchBytes))
	if err != nil {
		return fmt.Errorf("read canonical patch: %w", err)
	}
	patch, validationErr := controller.ValidateAndCanonicalizePatch(string(patchBytes), policy)
	if validationErr != nil {
		return fmt.Errorf("validate canonical patch: %s", validationErr.Code)
	}
	if patch.Sha256 != opts.expectedPatchSha256 {
		return fmt.Errorf("canonical patch SHA-256 mismatch: got %s", patch.Sha256)
	}
	identity := imageIdentity{
		Schema:       1,
		BaseSha:      opts.baseSha,
		PatchSha256:  patch.Sha256,
		PolicySha256: opts.expectedPolicySha256,
		Paths:        patch.Paths,
	}
	if opts.repositoryPatchDirectory != "" {
		patches, evalError := controller.SplitCompetitionRepositoryPatch(string(patchBytes), policy)
		if evalError != nil {
			return fmt.Errorf("route canonical patch: %s", evalError.Code)
		}
		identity.RepositoryPatches, err = writeRepositoryPatches(opts.repositoryPatchDirectory, patches)
		if err != nil {
			return fmt.Errorf("write routed patches: %w", err)
		}
	}
	if opts.sourceRoot != "" {
		patches, routingError := controller.SplitCompetitionRepositoryPatch(string(patchBytes), policy)
		if routingError != nil {
			return &repositoryApplicationError{cause: errors.New("source application requires exact routed patch bytes")}
		}
		identityBytes, err := readRegularFile(opts.sourceIdentityPath, maxPolicyBytes)
		if err != nil {
			return &repositoryApplicationError{cause: errors.New("original source identity is unavailable")}
		}
		identity.CandidateCommits, err = applyRepositoryPatchesContext(ctx, opts.sourceRoot, identityBytes, opts.expectedSourceIdentitySha256, opts.baseSha, patch.Sha256, policy, patch, patches, defaultRepositoryApplySettings())
		if err != nil {
			return &repositoryApplicationError{cause: err}
		}
	} else if opts.sourceIdentityPath != "" || opts.expectedSourceIdentitySha256 != "" {
		return errors.New("source identity options require --apply-source-root")
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(identity); err != nil {
		if opts.sourceRoot != "" {
			return &repositoryApplicationError{cause: errors.New("source application receipt could not be written")}
		}
		return err
	}
	return nil
}

// Exclusive creation avoids replacing prior evidence or following a directory
// symlink. Partial output on error is retained, but no successful receipt emits.
func writeRepositoryPatches(directory string, patches map[string][]byte) (map[string]repositoryPatchIdentity, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == string(filepath.Separator) || len(patches) == 0 {
		return nil, errors.New("canonical absolute private output directory required")
	}
	parent := filepath.Dir(directory)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return nil, errors.New("output parent is unavailable or redirected")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	identities := map[string]repositoryPatchIdentity{}
	repositories := make([]string, 0, len(patches))
	for repository := range patches {
		if repository == "" || filepath.Base(repository) != repository || repository == "." || repository == ".." {
			return nil, errors.New("unsafe repository output name")
		}
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	for _, repository := range repositories {
		content := patches[repository]
		if len(content) == 0 {
			return nil, errors.New("empty routed patch")
		}
		name := repository + ".patch"
		file, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := file.Write(content)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(content)
		identities[repository] = repositoryPatchIdentity{Path: name, Sha256: hex.EncodeToString(digest[:]), Bytes: len(content)}
	}
	file, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	return identities, errors.Join(file.Sync(), file.Close())
}

func readRegularFile(path string, maxBytes int64) ([]byte, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || maxBytes < info.Size() {
		return nil, errors.New("file must be a nonempty regular file within its size limit")
	}
	return os.ReadFile(path)
}
