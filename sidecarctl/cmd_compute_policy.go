// `sidecarctl compute policy build|sign|verify|publish|status|staging` (ATTEST-M12-02; DESIGN-COMPUTE §5.4, §14.10,
// D-ATTEST-2, D-ATTEST-8, D-ATTEST-9): the compute policy's pipeline, which the compute rota's sittings run
// (xops' COMPUTE-OPERATIONS.md), on the builder sidecar's root gives (sidecar/attest_policy_build.go):
//
//   - build <out.json> [--previous=<version.json>] [--changes=<changes.json>] [--version=<n>] [--not-after=<rfc3339>]
//     writes a policy's canonical bytes into a new file from the policy before it, verified under the .sig beside it
//     as a device verifies it, and the sitting's change set, refusing what the builder refuses (a version not above
//     the last, a not_after past 90 days, a profile with no review record, a disclosure whose fact changed without
//     its version raised), and prints each change and the bytes' SHA-256 for the holders' review;
//   - sign <version.json> --key=<name-cert.pub> signs it under ur-compute-policy by the compute-policy key on its
//     holder's token through ssh-keygen -Y sign, so no private key touches the tool: --key names the key's OpenSSH
//     certificate, of which ssh-keygen finds the private half on the token or in the agent, asking for a PIN or a
//     touch on the terminal. The tool reads the certificate alone, refusing a name that is not a certificate's
//     unread, and one a device would not take or that ends before the policy does; then it checks what came back as a
//     device would, signed by that certificate, and writes <version.json>.sig. "Two of four holders sign" is the key
//     ceremony's two-person custody of the one key (QD-1256): one signature, which this command makes;
//   - verify <version.json> [--staging] runs the device's verifier over it and its .sig, now;
//   - publish <version.json> [--tree=<dir>] [--release=<dir>] [--staging] puts it and its .sig, verified, into a
//     serving tree (mmm's ur.io/astro/public/compute/policy/v1/) as <version>.json and <version>.json.sig, only above
//     the tree's newest version, and into the release's copy (sidecar/attest_policy_release/'s compute-policy.json,
//     or compute-policy-staging.json), only above the copy there, which a release then embeds;
//   - status <tree> [--store=<dir>] [--staging] prints the days to the newest policy's not_after and to the newest
//     ticket's horizon, sidecar.ComputePolicyHorizon's figures, which server/monitor pages on at 14 and 7 days;
//   - staging <tree> --key=<staging-cert.pub> [--changes=<changes.json>] [--release=<dir>] makes the week's staging
//     policy on the CI host: from the newest staging policy in the tree and the changes, a gated line's first home
//     (§4.7, §19.2), expiring 7 days after its signing, signed under ur-compute-policy-staging by the staging key as
//     sign signs, verified as a staging build verifies it, and published as publish puts one.
//
// Every check pins this build's release key, the line attest_policy_root.go holds: none until the key ceremony
// (External), so until then nothing verifies, sign, publish and staging refuse every policy and status reports none,
// which is the fail-closed answer. No command reads a private key's file, none writes over a file it did not make but
// the release's copy, which it replaces only by a higher version, and each writes through a temporary file, so a
// reader never sees part of one. The freshness tickets' commands are cmd_compute_policy_tickets.go's.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/urnetwork/sidecar"
)

// The commands' registration. It is the only change to the binary they need.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy build",
		usage:   "<out.json> [--previous=<version.json>] [--changes=<changes.json>] [--version=<n>] [--not-after=<rfc3339>]",
		summary: "Write a compute policy's canonical bytes from the verified policy before it and a sitting's change set, and print each change for the holders' review (§5.4).",
		run:     runComputePolicyBuild,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy sign",
		usage:   "<version.json> --key=<name-cert.pub>",
		summary: "Sign a compute policy under ur-compute-policy through ssh-keygen -Y sign with the certified key on its holder's token, and check what came back as a device would.",
		run:     runComputePolicySign,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy verify",
		usage:   "<version.json> [--staging]",
		summary: "Verify a compute policy and its .sig as a device of this build verifies it.",
		run:     runComputePolicyVerify,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy publish",
		usage:   "<version.json> [--tree=<serving dir>] [--release=<attest_policy_release dir>] [--staging]",
		summary: "Put a verified compute policy into a serving tree as <version>.json and its .sig, and into the release's copy, each only above what is there.",
		run:     runComputePolicyPublish,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy status",
		usage:   "<serving dir> [--store=<ticket store>] [--staging]",
		summary: "Print the days to the newest compute policy's not_after and to the newest ticket's horizon, which the monitor pages on at 14 and 7 days (§14.7).",
		run:     runComputePolicyStatus,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy staging",
		usage:   "<serving dir> --key=<staging-cert.pub> [--changes=<changes.json>] [--release=<attest_policy_release dir>]",
		summary: "Make the week's staging compute policy on the CI host: build, sign under ur-compute-policy-staging with the staging key, verify and publish it, expiring after 7 days.",
		run:     runComputePolicyStaging,
	})
}

// Signs message under namespace with the key whose OpenSSH certificate certificatePath holds, answering the armored
// SSHSIG: ssh-keygen -Y sign on a holder's token, or a test's software key.
type computePolicySignFunc func(ctx context.Context, settings *computePolicyCommandSettings, certificatePath string, namespace string, message []byte) ([]byte, error)

// How the pipeline's commands run. Tests replace the release keys, the clock, the random source and the signer.
type computePolicyCommandSettings struct {
	// The pipeline's: the format's limits, §5.4's bounds, the lifetimes and the status warnings.
	build *sidecar.ComputePolicyBuildSettings
	// The release keys every check pins: nil, this build's (sidecar.ComputeReleaseKeys), none until the key ceremony;
	// a test gives test keys.
	releaseKeys []ssh.PublicKey
	clock       sidecar.Clock
	// What a ticket batch's id is drawn from; nil, crypto/rand.
	random io.Reader
	// How a document or a ticket is signed.
	sign computePolicySignFunc
	// The ssh-keygen sign runs, found on PATH.
	sshKeygen string
	// How long one signature may take, a holder's PIN and touch included: 5 min. Non-positive leaves it unbounded.
	signTimeout time.Duration
}

// The settings of a run. A caller changes its own copy.
func defaultComputePolicyCommandSettings() *computePolicyCommandSettings {
	return &computePolicyCommandSettings{
		build:       sidecar.DefaultComputePolicyBuildSettings(),
		clock:       sidecar.RealClock{},
		sign:        computePolicySshKeygenSign,
		sshKeygen:   "ssh-keygen",
		signTimeout: 5 * time.Minute,
	}
}

// A builder over settings for the production policy or, with staging, the staging policy.
func newComputePolicyCommandBuilder(settings *computePolicyCommandSettings, staging bool) *sidecar.ComputePolicyBuilder {
	return sidecar.NewComputePolicyBuilder(&sidecar.ComputePolicyBuilderArgs{
		Settings:    settings.build,
		Staging:     staging,
		ReleaseKeys: settings.releaseKeys,
		Clock:       settings.clock,
	})
}

// A writer that keeps at most maxByteCount bytes and fails past them.
type computePolicyBoundedBuffer struct {
	buffer       bytes.Buffer
	maxByteCount int
}

// Keeps data while the bound holds.
func (self *computePolicyBoundedBuffer) Write(data []byte) (int, error) {
	if self.maxByteCount < self.buffer.Len()+len(data) {
		return 0, fmt.Errorf("more than %d bytes", self.maxByteCount)
	}
	return self.buffer.Write(data)
}

// Signs message through `ssh-keygen -Y sign -f <certificatePath> -n <namespace>`, the message on its standard input and
// the armored signature on its standard output, within signTimeout. ssh-keygen loads the certificate and finds its
// private half itself, on the holder's token or in the agent; its standard error stays the terminal's, where it asks
// for a PIN or a touch.
func computePolicySshKeygenSign(ctx context.Context, settings *computePolicyCommandSettings, certificatePath string, namespace string, message []byte) ([]byte, error) {
	if 0 < settings.signTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, settings.signTimeout)
		defer cancel()
	}
	command := exec.CommandContext(ctx, settings.sshKeygen, "-Y", "sign", "-f", certificatePath, "-n", namespace)
	command.Stdin = bytes.NewReader(message)
	signature := &computePolicyBoundedBuffer{maxByteCount: settings.build.Attest.SignatureMaxByteCount}
	command.Stdout = signature
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s -Y sign: %w", settings.sshKeygen, err)
	}
	return bytes.Clone(signature.buffer.Bytes()), nil
}

// The file at path, refused past maxByteCount bytes unread beyond that.
func readComputePolicyFile(path string, what string, maxByteCount int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the %s does not open: %w", what, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(max(0, maxByteCount))+1))
	if err != nil {
		return nil, fmt.Errorf("the %s does not read: %w", what, err)
	}
	if maxByteCount < len(data) {
		return nil, fmt.Errorf("the %s at %s is past %d bytes", what, path, maxByteCount)
	}
	return data, nil
}

// The suffix ssh-keygen gives a key's certificate, <name>-cert.pub, the one name --key takes.
const computePolicyCertificateSuffix = "-cert.pub"

// The certificate line at path, which must name a certificate's file, <name>-cert.pub, and is read only then: no
// command reads a private key's file, even one named by mistake.
func readComputePolicyCertificate(path string, settings *computePolicyCommandSettings) ([]byte, error) {
	if path == "" || !strings.HasSuffix(filepath.Base(path), computePolicyCertificateSuffix) {
		return nil, sidecarctlUsageErrorf("--key names the key's OpenSSH certificate, <name>%s; the private key stays on its token", computePolicyCertificateSuffix)
	}
	return readComputePolicyFile(path, "certificate", settings.build.Attest.SignatureMaxByteCount)
}

// A policy document at path and its .sig beside it, verified as a device of builder's namespace verifies them now.
func readComputePolicyVerified(builder *sidecar.ComputePolicyBuilder, path string, settings *computePolicyCommandSettings) (*sidecar.ComputePolicy, error) {
	document, err := readComputePolicyFile(path, "policy", settings.build.Attest.PolicyMaxByteCount)
	if err != nil {
		return nil, err
	}
	signature, err := readComputePolicyFile(path+".sig", "policy's signature", settings.build.Attest.SignatureMaxByteCount)
	if err != nil {
		return nil, err
	}
	return builder.Verify(document, signature, nil)
}

// An RFC 3339 instant from a flag's text, zero for none.
func parseComputePolicyInstant(flagName string, text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, sidecarctlUsageErrorf("--%s=%q is not an RFC 3339 instant", flagName, text)
	}
	return at.UTC(), nil
}

// The SHA-256 of data, lowercase hex.
func computePolicySha256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// A duration in days, to a tenth, as status and the commands print it.
func computePolicyDays(duration time.Duration) string {
	return fmt.Sprintf("%.1f days", duration.Hours()/24)
}

// Who signed, as the commands print it.
func computePolicySignerText(signer sidecar.ComputePolicySigner) string {
	return fmt.Sprintf("%q serial %d (%s) under %s", signer.KeyId, signer.Serial, signer.Fingerprint, signer.Namespace)
}

// Syncs a directory, so what was created, renamed or removed in it survives a crash.
func syncComputePolicyDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// Writes data through a temporary file in path's directory, synced, and answers its name; the caller links or renames
// it into place and removes what is left.
func stageComputePolicyFile(path string, data []byte, mode fs.FileMode) (string, error) {
	staged, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	if _, err := staged.Write(data); err != nil {
		staged.Close()
		os.Remove(staged.Name())
		return "", err
	}
	if err := staged.Chmod(mode); err != nil {
		staged.Close()
		os.Remove(staged.Name())
		return "", err
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		os.Remove(staged.Name())
		return "", err
	}
	if err := staged.Close(); err != nil {
		os.Remove(staged.Name())
		return "", err
	}
	return staged.Name(), nil
}

// The permissions every file the pipeline writes takes: public files, read by the serving path and a release build.
const computePolicyFileMode fs.FileMode = 0o644

// Creates path with data, refusing one that exists: through a temporary file linked into place, so a failure leaves
// nothing and a reader never sees part of it.
func createComputePolicyFile(path string, data []byte) error {
	staged, err := stageComputePolicyFile(path, data, computePolicyFileMode)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	if err := os.Link(staged, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s exists, and nothing was written over it", path)
		}
		return err
	}
	return syncComputePolicyDirectory(filepath.Dir(path))
}

// Replaces path with data through a temporary file renamed over it, so a reader sees the old file or the new.
func replaceComputePolicyFile(path string, data []byte) error {
	staged, err := stageComputePolicyFile(path, data, computePolicyFileMode)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	if err := os.Rename(staged, path); err != nil {
		return err
	}
	return syncComputePolicyDirectory(filepath.Dir(path))
}

// Creates path with data where it is absent; where it holds data already, nothing is done; where it holds anything
// else, it is refused as a conflict, since one version has one document.
func putComputePolicyFile(path string, data []byte) (bool, error) {
	existing, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(existing, data):
		return false, nil
	case err == nil:
		return false, fmt.Errorf("%s holds other bytes, and one version has one document: nothing was written over it", path)
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	return true, createComputePolicyFile(path, data)
}

// The versions a serving tree holds, by their files' names, highest first. The tree must be a directory.
func computePolicyTreeVersions(tree string) ([]uint64, error) {
	entries, err := os.ReadDir(tree)
	if err != nil {
		return nil, fmt.Errorf("the serving tree %s does not read: %w", tree, err)
	}
	versions := []uint64{}
	for _, entry := range entries {
		if version, ok := sidecar.ParseComputePolicyFileName(entry.Name()); ok && entry.Type().IsRegular() {
			versions = append(versions, version)
		}
	}
	slices.Sort(versions)
	slices.Reverse(versions)
	return versions, nil
}

// The newest policy in tree that verifies as builder's namespace verifies it, nil where none does, and a line for each
// version above it that did not.
func computePolicyTreeNewest(builder *sidecar.ComputePolicyBuilder, tree string, settings *computePolicyCommandSettings) (*sidecar.ComputePolicy, []string, error) {
	versions, err := computePolicyTreeVersions(tree)
	if err != nil {
		return nil, nil, err
	}
	refusals := []string{}
	for _, version := range versions {
		policy, err := readComputePolicyVerified(builder, filepath.Join(tree, sidecar.ComputePolicyFileName(version)), settings)
		if err == nil {
			return policy, refusals, nil
		}
		refusals = append(refusals, fmt.Sprintf("version %d in the tree does not verify: %s", version, err))
	}
	return nil, refusals, nil
}

// `sidecarctl compute policy build`.
func runComputePolicyBuild(args []string, out io.Writer) error {
	return runComputePolicyBuildWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// `sidecarctl compute policy sign`.
func runComputePolicySign(args []string, out io.Writer) error {
	return runComputePolicySignWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// `sidecarctl compute policy verify`.
func runComputePolicyVerify(args []string, out io.Writer) error {
	return runComputePolicyVerifyWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// `sidecarctl compute policy publish`.
func runComputePolicyPublish(args []string, out io.Writer) error {
	return runComputePolicyPublishWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// `sidecarctl compute policy status`.
func runComputePolicyStatus(args []string, out io.Writer) error {
	return runComputePolicyStatusWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// `sidecarctl compute policy staging`.
func runComputePolicyStaging(args []string, out io.Writer) error {
	return runComputePolicyStagingWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// One path from positionals: a usage error for any other count.
func computePolicyOnePath(positionals []string, what string) (string, error) {
	if len(positionals) != 1 || positionals[0] == "" {
		return "", sidecarctlUsageErrorf("want %s, got %q", what, positionals)
	}
	return positionals[0], nil
}

// The change set at path, nil for none.
func readComputePolicyChanges(path string, settings *computePolicyCommandSettings) (*sidecar.ComputePolicyChangeSet, error) {
	if path == "" {
		return nil, nil
	}
	data, err := readComputePolicyFile(path, "change set", settings.build.Attest.PolicyMaxByteCount)
	if err != nil {
		return nil, err
	}
	return sidecar.ParseComputePolicyChangeSet(data, settings.build)
}

// Prints a build for the holders' review: its version, file, size and SHA-256, and each change.
func printComputePolicyBuild(out io.Writer, build *sidecar.ComputePolicyBuild, path string) {
	fmt.Fprintf(out, "built version %d into %s: %d bytes, sha256 %s\n", build.Policy.Version, path, len(build.Document), computePolicySha256(build.Document))
	for _, change := range build.Changes {
		fmt.Fprintf(out, "  %s\n", change)
	}
}

// Parses build's command line and writes the policy.
func runComputePolicyBuildWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy build", flag.ContinueOnError)
	previousPath := flags.String("previous", "", "the policy before it, <version>.json with its .sig beside it")
	changesPath := flags.String("changes", "", "the sitting's change set")
	version := flags.Uint64("version", 0, "the new version; the one after the previous by default")
	notAfterText := flags.String("not-after", "", "the new not_after, RFC 3339; 90 days after now by default")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	outPath, err := computePolicyOnePath(positionals, "the new policy's path")
	if err != nil {
		return err
	}
	notAfter, err := parseComputePolicyInstant("not-after", *notAfterText)
	if err != nil {
		return err
	}
	builder := newComputePolicyCommandBuilder(settings, false)
	var previous *sidecar.ComputePolicy
	if *previousPath != "" {
		previous, err = readComputePolicyVerified(builder, *previousPath, settings)
		if err != nil {
			return fmt.Errorf("the policy before it, %s, does not verify as a device verifies it: %w", *previousPath, err)
		}
	}
	changes, err := readComputePolicyChanges(*changesPath, settings)
	if err != nil {
		return err
	}
	build, err := builder.Build(&sidecar.ComputePolicyBuildArgs{Previous: previous, Version: *version, NotAfter: notAfter, Changes: changes})
	if err != nil {
		return err
	}
	if err := createComputePolicyFile(outPath, build.Document); err != nil {
		return err
	}
	printComputePolicyBuild(out, build, outPath)
	fmt.Fprintf(out, "two holders review the changes and sign it: sidecarctl compute policy sign %s --key=<name>%s\n", outPath, computePolicyCertificateSuffix)
	return nil
}

// Signs document under builder's namespace through settings' signer with the key certificatePath names, once the
// document is signable and the certificate is one a device takes until the policy's not_after; answers the signature
// and the policy, checked as a device checks them and signed by that certificate.
func signComputePolicy(builder *sidecar.ComputePolicyBuilder, document []byte, certificatePath string, settings *computePolicyCommandSettings) ([]byte, *sidecar.ComputePolicy, error) {
	policy, err := builder.CheckSignable(document)
	if err != nil {
		return nil, nil, err
	}
	line, err := readComputePolicyCertificate(certificatePath, settings)
	if err != nil {
		return nil, nil, err
	}
	certificate, err := builder.Signer(line, policy.NotAfter())
	if err != nil {
		return nil, nil, err
	}
	signature, err := settings.sign(context.Background(), settings, certificatePath, builder.Namespace(), document)
	if err != nil {
		return nil, nil, err
	}
	verified, err := builder.Verify(document, signature, certificate)
	if err != nil {
		return nil, nil, fmt.Errorf("what came back from signing does not verify, and nothing was written: %w", err)
	}
	return signature, verified, nil
}

// Parses sign's command line and writes the policy's .sig.
func runComputePolicySignWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy sign", flag.ContinueOnError)
	keyPath := flags.String("key", "", "the compute-policy key's OpenSSH certificate, <name>-cert.pub")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	documentPath, err := computePolicyOnePath(positionals, "the policy's path")
	if err != nil {
		return err
	}
	if *keyPath == "" {
		return sidecarctlUsageErrorf("--key names the compute-policy key's certificate, <name>%s", computePolicyCertificateSuffix)
	}
	signaturePath := documentPath + ".sig"
	if _, err := os.Stat(signaturePath); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s exists, and nothing was signed: remove it to sign again", signaturePath)
	}
	document, err := readComputePolicyFile(documentPath, "policy", settings.build.Attest.PolicyMaxByteCount)
	if err != nil {
		return err
	}
	builder := newComputePolicyCommandBuilder(settings, false)
	signature, policy, err := signComputePolicy(builder, document, *keyPath, settings)
	if err != nil {
		return err
	}
	if err := createComputePolicyFile(signaturePath, signature); err != nil {
		return err
	}
	fmt.Fprintf(out, "signed version %d, sha256 %s, by %s into %s\n", policy.Version, computePolicySha256(document), computePolicySignerText(policy.Signer()), signaturePath)
	return nil
}

// Parses verify's command line and verifies the policy as a device would.
func runComputePolicyVerifyWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy verify", flag.ContinueOnError)
	staging := flags.Bool("staging", false, "verify as a staging or CI build does")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	documentPath, err := computePolicyOnePath(positionals, "the policy's path")
	if err != nil {
		return err
	}
	builder := newComputePolicyCommandBuilder(settings, *staging)
	policy, err := readComputePolicyVerified(builder, documentPath, settings)
	if err != nil {
		return fmt.Errorf("%s does not verify as a device of this build verifies it: %w", documentPath, err)
	}
	now := settings.clock.Now()
	profileCount := 0
	for _, provider := range policy.Providers {
		profileCount += len(provider.Profiles)
	}
	fmt.Fprintf(out, "version %d verifies, sha256 %s, signed by %s\n", policy.Version, computePolicySha256(policy.Document()), computePolicySignerText(policy.Signer()))
	fmt.Fprintf(out, "  not_after %s, %s from now\n", policy.NotAfter().Format(time.RFC3339), computePolicyDays(policy.NotAfter().Sub(now)))
	fmt.Fprintf(out, "  %d TEE providers with %d profiles, %d direct providers, %d disclosures, %d revoked profiles\n", len(policy.Providers), profileCount,
		len(policy.Direct.Providers), len(policy.Disclosures), len(policy.Revoked.Profiles))
	if !policy.Current(now) {
		return fmt.Errorf("version %d verifies but is past its not_after: a device refuses remote work under it", policy.Version)
	}
	return nil
}

// Puts policy's document and signature into tree as <version>.json and its .sig, only above the tree's newest
// version, the same bytes again doing nothing; the signature goes first, so the document never stands without it.
func publishComputePolicyToTree(tree string, policy *sidecar.ComputePolicy, out io.Writer) error {
	versions, err := computePolicyTreeVersions(tree)
	if err != nil {
		return err
	}
	if 0 < len(versions) && policy.Version < versions[0] {
		return fmt.Errorf("version %d is not above the tree's newest, %d, and nothing was written", policy.Version, versions[0])
	}
	documentPath := filepath.Join(tree, sidecar.ComputePolicyFileName(policy.Version))
	signaturePath := filepath.Join(tree, sidecar.ComputePolicySignatureFileName(policy.Version))
	signatureWritten, err := putComputePolicyFile(signaturePath, policy.Signature())
	if err != nil {
		return err
	}
	documentWritten, err := putComputePolicyFile(documentPath, policy.Document())
	if err != nil {
		return err
	}
	if !signatureWritten && !documentWritten {
		fmt.Fprintf(out, "the tree already serves version %d as %s\n", policy.Version, documentPath)
		return nil
	}
	fmt.Fprintf(out, "published version %d into the tree as %s and its .sig\n", policy.Version, documentPath)
	return nil
}

// Puts policy into release, attest_policy_release/, as the release's copy, only above the copy there, which must
// itself verify: a copy this build does not take is a question for a person, never overwritten.
func publishComputePolicyToRelease(release string, builder *sidecar.ComputePolicyBuilder, policy *sidecar.ComputePolicy, settings *computePolicyCommandSettings, out io.Writer) error {
	if info, err := os.Stat(release); err != nil || !info.IsDir() {
		return fmt.Errorf("the release's copy's directory %s is not a directory", release)
	}
	documentName, signatureName := sidecar.ComputePolicyReleaseCopyNames(builder.Staging())
	documentPath := filepath.Join(release, documentName)
	signaturePath := filepath.Join(release, signatureName)
	_, documentErr := os.Stat(documentPath)
	_, signatureErr := os.Stat(signaturePath)
	switch {
	case errors.Is(documentErr, fs.ErrNotExist) && errors.Is(signatureErr, fs.ErrNotExist):
	case documentErr != nil || signatureErr != nil:
		return fmt.Errorf("the release's copy in %s is half there, which a release refuses: check why and remove it by hand", release)
	default:
		existing, err := readComputePolicyVerified(builder, documentPath, settings)
		if err != nil {
			return fmt.Errorf("the release's copy in %s does not verify, and nothing was written over it: check why and remove it by hand: %w", release, err)
		}
		switch {
		case policy.Version < existing.Version:
			return fmt.Errorf("version %d is older than the release's copy, version %d, and nothing was written", policy.Version, existing.Version)
		case policy.Version == existing.Version && bytes.Equal(policy.Document(), existing.Document()):
			fmt.Fprintf(out, "the release's copy is already version %d\n", policy.Version)
			return nil
		case policy.Version == existing.Version:
			return fmt.Errorf("the release's copy is version %d with other bytes, and one version has one document", policy.Version)
		}
	}
	if err := replaceComputePolicyFile(signaturePath, policy.Signature()); err != nil {
		return err
	}
	if err := replaceComputePolicyFile(documentPath, policy.Document()); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote version %d as the release's copy, %s and its .sig\n", policy.Version, documentPath)
	return nil
}

// Parses publish's command line and puts the verified policy where it names.
func runComputePolicyPublishWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy publish", flag.ContinueOnError)
	tree := flags.String("tree", "", "the serving tree, which holds <version>.json and tickets/")
	release := flags.String("release", "", "sidecar's attest_policy_release/, the copy a release embeds")
	staging := flags.Bool("staging", false, "the staging policy")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	documentPath, err := computePolicyOnePath(positionals, "the policy's path")
	if err != nil {
		return err
	}
	if *tree == "" && *release == "" {
		return sidecarctlUsageErrorf("name --tree, --release or both")
	}
	builder := newComputePolicyCommandBuilder(settings, *staging)
	policy, err := readComputePolicyVerified(builder, documentPath, settings)
	if err != nil {
		return fmt.Errorf("%s does not verify as a device of this build verifies it, and nothing was published: %w", documentPath, err)
	}
	if !policy.Current(settings.clock.Now()) {
		return fmt.Errorf("version %d is past its not_after, and nothing was published", policy.Version)
	}
	if *tree != "" {
		if err := publishComputePolicyToTree(*tree, policy, out); err != nil {
			return err
		}
	}
	if *release != "" {
		if err := publishComputePolicyToRelease(*release, builder, policy, settings, out); err != nil {
			return err
		}
	}
	return nil
}

// Every ticket file under directory that verifies as builder's namespace verifies it, and a line for each that does
// not. Hidden names are a writer's temporary files, and are skipped.
func readComputePolicyTicketFiles(builder *sidecar.ComputePolicyBuilder, directory string, settings *computePolicyCommandSettings) ([]*computePolicyTicketFile, []string, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return []*computePolicyTicketFile{}, []string{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	tickets := []*computePolicyTicketFile{}
	refusals := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !entry.Type().IsRegular() || !strings.HasSuffix(name, ".bin") {
			continue
		}
		path := filepath.Join(directory, name)
		file, err := readComputePolicyFile(path, "ticket", settings.build.Attest.TicketMaxByteCount)
		if err == nil {
			var ticket *sidecar.ComputePolicyTicket
			if ticket, err = builder.VerifyTicket(file, nil); err == nil {
				tickets = append(tickets, &computePolicyTicketFile{path: path, file: file, ticket: ticket})
				continue
			}
		}
		refusals = append(refusals, fmt.Sprintf("%s does not verify: %s", path, err))
	}
	return tickets, refusals, nil
}

// One ticket file read and verified: where it lies, its bytes and the ticket.
type computePolicyTicketFile struct {
	path   string
	file   []byte
	ticket *sidecar.ComputePolicyTicket
}

// Parses status's command line and prints the horizon.
func runComputePolicyStatusWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy status", flag.ContinueOnError)
	store := flags.String("store", "", "the ticket host's store of unreleased tickets")
	staging := flags.Bool("staging", false, "the staging policy's tree")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	tree, err := computePolicyOnePath(positionals, "the serving tree")
	if err != nil {
		return err
	}
	builder := newComputePolicyCommandBuilder(settings, *staging)
	now := settings.clock.Now()
	newest, refusals, err := computePolicyTreeNewest(builder, tree, settings)
	if err != nil {
		return err
	}
	released, ticketRefusals, err := readComputePolicyTicketFiles(builder, filepath.Join(tree, sidecar.ComputePolicyTicketDirectory), settings)
	if err != nil {
		return err
	}
	refusals = append(refusals, ticketRefusals...)
	unreleased := []*computePolicyTicketFile{}
	if *store != "" {
		storeTickets, storeRefusals, err := readComputePolicyStore(builder, *store, settings)
		if err != nil {
			return err
		}
		unreleased = storeTickets
		refusals = append(refusals, storeRefusals...)
	}
	tickets := []*sidecar.ComputePolicyTicket{}
	for _, entry := range slices.Concat(released, unreleased) {
		tickets = append(tickets, entry.ticket)
	}
	horizon := sidecar.NewComputePolicyHorizon(now, newest, tickets)
	pages := []string{}
	if newest == nil {
		fmt.Fprintf(out, "policy: none in %s verifies\n", tree)
	} else {
		fmt.Fprintf(out, "policy: version %d, not_after %s, %s left\n", newest.Version, newest.NotAfter().Format(time.RFC3339), computePolicyDays(horizon.PolicyLeft()))
	}
	if horizon.PolicyExpiring(settings.build) {
		pages = append(pages, fmt.Sprintf("the policy is within %s of its not_after, or past it: call a sitting", computePolicyDays(settings.build.PolicyExpiryWarning)))
	}
	var current *sidecar.ComputePolicyTicket
	for _, entry := range released {
		if entry.ticket.Current(now, settings.build.Attest) && (current == nil || entry.ticket.NotBefore.After(current.NotBefore)) {
			current = entry.ticket
		}
	}
	if current == nil {
		fmt.Fprintf(out, "released: no ticket in %s is current\n", tree)
	} else {
		fmt.Fprintf(out, "released: the ticket from %s of batch %s, naming version %d, current until %s\n", current.NotBefore.Format(time.RFC3339),
			current.Batch.Id, current.PolicyVersion, computePolicyMinTimeText(current.NotAfter, current.Batch.Expires))
	}
	if horizon.Ticket == nil {
		fmt.Fprintf(out, "ticket horizon: no signed ticket verifies\n")
	} else {
		batch := horizon.Ticket.Batch
		fmt.Fprintf(out, "ticket horizon: batch %s, signed %s, expires %s, %s left\n", batch.Id, batch.SignedAt.Format(time.RFC3339),
			batch.Expires.Format(time.RFC3339), computePolicyDays(horizon.TicketLeft()))
	}
	if horizon.TicketHorizonNear(settings.build) {
		pages = append(pages, fmt.Sprintf("the newest signed ticket is within %s of its horizon, or past it: call the tickets' sitting", computePolicyDays(settings.build.TicketHorizonWarning)))
	}
	if *store != "" {
		fmt.Fprintf(out, "store: %d unreleased tickets\n", len(unreleased))
	}
	for _, page := range pages {
		fmt.Fprintf(out, "page: %s\n", page)
	}
	for _, refusal := range refusals {
		fmt.Fprintf(out, "refused: %s\n", refusal)
	}
	switch {
	case newest == nil:
		return errors.New("no policy in the tree verifies: devices fetching it take none")
	case horizon.PolicyLeft() <= 0:
		return fmt.Errorf("version %d is past its not_after: remote work has stopped", newest.Version)
	case horizon.Ticket == nil || horizon.TicketLeft() <= 0:
		return errors.New("no signed ticket carries remote work past now: remote work stops as the last released one ends")
	case 0 < len(refusals):
		return fmt.Errorf("%d files did not verify", len(refusals))
	}
	return nil
}

// The earlier of a and b, as the commands print an instant.
func computePolicyMinTimeText(a time.Time, b time.Time) string {
	if b.Before(a) {
		a = b
	}
	return a.Format(time.RFC3339)
}

// Parses staging's command line and builds, signs, verifies and publishes the week's staging policy.
func runComputePolicyStagingWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy staging", flag.ContinueOnError)
	keyPath := flags.String("key", "", "the staging key's OpenSSH certificate, <name>-cert.pub")
	changesPath := flags.String("changes", "", "the week's change set: the gated lines under test")
	release := flags.String("release", "", "sidecar's attest_policy_release/, the copy a staging build embeds")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	tree, err := computePolicyOnePath(positionals, "the staging serving tree")
	if err != nil {
		return err
	}
	if *keyPath == "" {
		return sidecarctlUsageErrorf("--key names the staging key's certificate, <name>%s", computePolicyCertificateSuffix)
	}
	builder := newComputePolicyCommandBuilder(settings, true)
	previous, refusals, err := computePolicyTreeNewest(builder, tree, settings)
	if err != nil {
		return err
	}
	if 0 < len(refusals) {
		return fmt.Errorf("the staging tree holds a newer policy than any that verifies, and nothing was built: %s", strings.Join(refusals, "; "))
	}
	changes, err := readComputePolicyChanges(*changesPath, settings)
	if err != nil {
		return err
	}
	build, err := builder.Build(&sidecar.ComputePolicyBuildArgs{Previous: previous, Changes: changes})
	if err != nil {
		return err
	}
	_, policy, err := signComputePolicy(builder, build.Document, *keyPath, settings)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "built and signed staging version %d, sha256 %s, by %s\n", policy.Version, computePolicySha256(build.Document), computePolicySignerText(policy.Signer()))
	for _, change := range build.Changes {
		fmt.Fprintf(out, "  %s\n", change)
	}
	if err := publishComputePolicyToTree(tree, policy, out); err != nil {
		return err
	}
	if *release != "" {
		if err := publishComputePolicyToRelease(*release, builder, policy, settings, out); err != nil {
			return err
		}
	}
	return nil
}
