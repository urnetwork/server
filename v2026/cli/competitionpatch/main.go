// competitionpatch revalidates the exact patch bytes used to build a
// competition submission image. It is compiled into the trusted evaluator
// base image; submissions cannot replace it.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/urnetwork/server/v2026/competition"
)

const maxPolicyBytes = 1 << 20

var (
	gitShaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type options struct {
	baseSha              string
	expectedPatchSha256  string
	expectedPolicySha256 string
	patchPath            string
	policyPath           string
}

type imageIdentity struct {
	Schema       int      `json:"schema"`
	BaseSha      string   `json:"base_sha"`
	PatchSha256  string   `json:"patch_sha256"`
	PolicySha256 string   `json:"policy_sha256"`
	Paths        []string `json:"paths"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("competitionpatch", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts options
	flags.StringVar(&opts.baseSha, "base-sha", "", "trusted base Git SHA")
	flags.StringVar(&opts.expectedPatchSha256, "expected-patch-sha256", "", "expected canonical patch SHA-256")
	flags.StringVar(&opts.expectedPolicySha256, "expected-policy-sha256", "", "expected policy-file SHA-256")
	flags.StringVar(&opts.patchPath, "patch", "", "canonical patch path")
	flags.StringVar(&opts.policyPath, "policy", "", "patch policy JSON path")
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
	var policy competition.PatchPolicy
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
	patch, validationErr := competition.ValidateAndCanonicalizePatch(string(patchBytes), policy)
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
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(identity)
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
