// `sidecarctl compute models mirror|index|serve|status` (CSRV-M11-02; DESIGN-COMPUTE §6.5's model downloads, §14.5's
// compute_model_mirror; CO-49, D-INFER-12; DESIGN §4.10): the release tooling of the api's model mirror, over the env's
// blob store as the api and taskworker reach it (server's ComputeModelBlobStore, the object family compute-models/,
// metadata row compute-12), driving sidecar/model's ComputeModelMirror. A release takes its local model files through
// the four commands in order:
//
//   - mirror <env> <draft-manifest> copies each local model file the draft names that the mirror lacks from Hugging
//     Face's litert-community at the commit the draft's url pins, in this process, each file checked whole against
//     the draft's size and SHA-256 as it streams and marked copied, never served. Once CSRV-M12-05 registers
//     compute_model_mirror, the copy is queued as that job keyed by the file's SHA-256 instead; until then it runs
//     here. --recopy copies again a file the mirror holds copied or indexed, as after index found its stored chunks are
//     not the draft's file; a served file is final and left as it is.
//   - index <env> <draft-manifest> computes each copied file's chunk index from the mirrored bytes, stores it, and
//     writes its index_sha256 into the draft, which then parses as a released manifest, before the release is signed
//     (DESIGN §4.10's pipeline signs it after). Every file is indexed before the draft is written, and the draft is
//     written whole or not at all; a pin it already carries must be the mirror's, so the tooling never moves one, and
//     a draft that already carries every pin is left byte for byte as it is.
//   - serve <env> <manifest> --sums=<SHA256SUMS> --signature=<SHA256SUMS.sig> serves the files a released manifest
//     names once the release's SHA256SUMS, under the release key's signature, lists the manifest's own SHA-256: each
//     file's stored index is checked against the manifest's pin and every stored chunk against the index before the
//     file is marked served and the api's routes answer it. This build pins the release key attest_policy_root.go
//     holds, none until the key ceremony, so until then serve refuses every manifest, which is the fail-closed answer.
//   - status <env> <manifest> lists each local model file a draft or released manifest names with its state in the
//     mirror: absent, copied, indexed or served, and the index pin of an indexed or served one.
//
// Each command reaches one env, the one WARP_ENV names, so each refuses an <env> that is not it before it opens the
// store: the operator names the env twice, and a slip in either changes nothing. mirror, index and serve go on past a
// file that fails to the rest, then exit 1 naming how many failed; nothing here signs a manifest or reads a member's
// data, and the files are public, by SHA-256.

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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// An env's name: one directory under config/ and vault/, as warp lays them out.
var computeModelsEnvRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Server's store is the mirror's: the routes read what these commands write.
var _ model.ComputeModelMirrorStore = (*server.ComputeModelBlobStore)(nil)

// The commands' registration. It is the only change to the binary they need.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute models mirror",
		usage:   "<env> <draft-manifest> [--recopy]",
		summary: "Copy each local model file a draft manifest names into the api's model mirror from Hugging Face at its pinned commit, checked whole, never served (§6.5, §14.5).",
		run:     runComputeModelsMirror,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute models index",
		usage:   "<env> <draft-manifest>",
		summary: "Compute each copied model file's chunk index from the mirrored bytes and write its index_sha256 into the draft manifest before the release is signed.",
		run:     runComputeModelsIndex,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute models serve",
		usage:   "<env> <manifest> --sums=<SHA256SUMS> --signature=<SHA256SUMS.sig>",
		summary: "Serve a released manifest's model files from the api once the release key's signed SHA256SUMS lists the manifest, every stored chunk checked against its pinned index.",
		run:     runComputeModelsServe,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute models status",
		usage:   "<env> <manifest>",
		summary: "List each local model file a manifest names with its state in the api's model mirror: absent, copied, indexed or served.",
		run:     runComputeModelsStatus,
	})
}

// How a run reaches its env. Tests replace the store, the hub and the release keys.
type computeModelsCommandSettings struct {
	// The mirror's: the hub, its bounds and retries, each step's bound, the staging and the release sums' bounds.
	mirror *model.ComputeModelMirrorSettings
	// The format's: the chunk size, and the manifest's and the index's bounds.
	infer *sidecar.InferSettings
	// The release keys serve checks the sums against: the ones this build pins.
	releaseKeys func() []ssh.PublicKey
	// Bounds status's reads, the store's state of each file, so a store that does not answer fails the runbook step
	// rather than holding it. Non-positive expires at once.
	statusTimeout time.Duration
	// Opens env's mirror, refusing an env this process does not run as and a process with no blob store.
	openMirror func(ctx context.Context, env string, settings *computeModelsCommandSettings) (*model.ComputeModelMirror, error)
}

// The settings of a run. A caller changes its own copy.
func defaultComputeModelsCommandSettings() *computeModelsCommandSettings {
	return &computeModelsCommandSettings{
		mirror:      model.DefaultComputeModelMirrorSettings(),
		infer:       sidecar.DefaultInferSettings(),
		releaseKeys: sidecar.ComputeReleaseKeys,
		// one state object's read a file, well under a second each on a healthy store
		statusTimeout: time.Minute,
		openMirror:    openServerComputeModelMirror,
	}
}

// The env's mirror as server reaches it, which is the one WARP_ENV names, over the blob store its minio.yml selects
// and the hub: any other env, no WARP_ENV, or no blob store is refused before anything is read.
func openServerComputeModelMirror(ctx context.Context, env string, settings *computeModelsCommandSettings) (*model.ComputeModelMirror, error) {
	processEnv, err := server.Env()
	if err != nil {
		return nil, fmt.Errorf("WARP_ENV is not set, and this reaches the env it names; run it as that env's services run, with WARP_ENV=%s", env)
	}
	if processEnv != env {
		return nil, fmt.Errorf("this process runs as WARP_ENV=%s and was asked for %s; run it with WARP_ENV=%s", processEnv, env, env)
	}
	var store server.BlobStore
	ok := false
	if err := sidecarctlServerCall(func() error {
		store, ok = server.LoadBlobStore()
		return nil
	}); err != nil {
		return nil, err
	}
	if !ok || store == nil {
		return nil, fmt.Errorf("%s configures no blob store (minio.yml), which holds the model mirror", env)
	}
	return model.NewComputeModelMirror(
		server.NewComputeModelBlobStore(nil),
		model.NewComputeModelHubFetcher(settings.mirror, nil, nil),
		settings.mirror,
		settings.infer,
		nil,
		nil,
	), nil
}

// `sidecarctl compute models mirror`, against the env as server reaches it.
func runComputeModelsMirror(args []string, out io.Writer) error {
	return runComputeModelsMirrorWithSettings(args, defaultComputeModelsCommandSettings(), out)
}

// `sidecarctl compute models index`, against the env as server reaches it.
func runComputeModelsIndex(args []string, out io.Writer) error {
	return runComputeModelsIndexWithSettings(args, defaultComputeModelsCommandSettings(), out)
}

// `sidecarctl compute models serve`, against the env as server reaches it.
func runComputeModelsServe(args []string, out io.Writer) error {
	return runComputeModelsServeWithSettings(args, defaultComputeModelsCommandSettings(), out)
}

// `sidecarctl compute models status`, against the env as server reaches it.
func runComputeModelsStatus(args []string, out io.Writer) error {
	return runComputeModelsStatusWithSettings(args, defaultComputeModelsCommandSettings(), out)
}

// The env and the manifest's path from positionals: a usage error for any other count or an env that is not a
// directory's name.
func computeModelsPositionals(positionals []string) (string, string, error) {
	if len(positionals) != 2 {
		return "", "", sidecarctlUsageErrorf("want <env> and a manifest's path, got %q", positionals)
	}
	env := positionals[0]
	if !computeModelsEnvRegex.MatchString(env) {
		return "", "", sidecarctlUsageErrorf("%q is not an env's name, one directory under config/", env)
	}
	if positionals[1] == "" {
		return "", "", sidecarctlUsageErrorf("an empty manifest path")
	}
	return env, positionals[1], nil
}

// The file at path, refused past maxByteCount bytes unread beyond that.
func readComputeModelsFile(path string, what string, maxByteCount int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the %s does not open: %w", what, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, max(0, maxByteCount)+1))
	if err != nil {
		return nil, fmt.Errorf("the %s does not read: %w", what, err)
	}
	if maxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("the %s is past %d bytes", what, maxByteCount)
	}
	return data, nil
}

// The draft or released manifest at path, parsed as a draft, which a released manifest also is.
func readComputeModelsDraft(path string, settings *computeModelsCommandSettings) ([]byte, *sidecar.InferManifest, error) {
	data, err := readComputeModelsFile(path, "manifest", int64(settings.infer.ManifestMaxByteCount))
	if err != nil {
		return nil, nil, err
	}
	manifest, err := model.ParseComputeModelDraftManifest(data, settings.infer)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return data, manifest, nil
}

// Opens env's mirror, refusing a failed or empty open.
func openComputeModelsMirror(ctx context.Context, env string, settings *computeModelsCommandSettings) (*model.ComputeModelMirror, error) {
	mirror, err := settings.openMirror(ctx, env, settings)
	if err == nil && mirror == nil {
		err = errors.New("it opened no mirror")
	}
	if err != nil {
		return nil, fmt.Errorf("nothing was read or written in %s: %w", env, err)
	}
	return mirror, nil
}

// The models each file's SHA-256 is listed under, in order, for what the commands print.
func computeModelsFileModels(manifest *sidecar.InferManifest) map[string][]string {
	sha256Models := map[string][]string{}
	for _, entry := range manifest.Models {
		if entry == nil {
			continue
		}
		for _, file := range entry.Files {
			if file != nil {
				sha256Models[file.Sha256] = append(sha256Models[file.Sha256], entry.Model)
			}
		}
	}
	return sha256Models
}

// One file as the commands name it: its name and the models listing it.
func computeModelsFileLabel(file *sidecar.InferManifestModelFile, sha256Models map[string][]string) string {
	return file.Name + " (" + strings.Join(sha256Models[file.Sha256], ", ") + ")"
}

// Parses mirror's command line and copies each file the draft names that the mirror lacks.
func runComputeModelsMirrorWithSettings(args []string, settings *computeModelsCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute models mirror", flag.ContinueOnError)
	recopy := flags.Bool("recopy", false, "copy again a file the mirror holds copied or indexed")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	env, path, err := computeModelsPositionals(positionals)
	if err != nil {
		return err
	}
	_, manifest, err := readComputeModelsDraft(path, settings)
	if err != nil {
		return err
	}
	ctx := context.Background()
	mirror, err := openComputeModelsMirror(ctx, env, settings)
	if err != nil {
		return err
	}
	sha256Models := computeModelsFileModels(manifest)
	files := model.ComputeModelManifestFiles(manifest)
	failedCount := 0
	for _, file := range files {
		label := computeModelsFileLabel(file, sha256Models)
		line, err := func() (string, error) {
			if !*recopy {
				record, copied, err := mirror.Copy(ctx, file)
				if err != nil {
					return "", err
				}
				if copied {
					return "copied", nil
				}
				return "already " + string(record.State), nil
			}
			record, err := mirror.Status(ctx, file.Sha256)
			if err != nil {
				return "", err
			}
			if record.State == model.ComputeModelFileStateServed {
				return "served, final, left as it is", nil
			}
			if _, err := mirror.Recopy(ctx, file); err != nil {
				return "", err
			}
			return "copied again", nil
		}()
		if err != nil {
			failedCount += 1
			fmt.Fprintf(out, "failed  %s  %s: %s\n", file.Sha256, label, err)
			continue
		}
		fmt.Fprintf(out, "%s  %s  %s\n", line, file.Sha256, label)
	}
	if 0 < failedCount {
		return fmt.Errorf("%d of %d files were not copied into %s's mirror", failedCount, len(files), env)
	}
	return nil
}

// Parses index's command line, indexes every file the draft names and writes their pins into it.
func runComputeModelsIndexWithSettings(args []string, settings *computeModelsCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute models index", flag.ContinueOnError)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	env, path, err := computeModelsPositionals(positionals)
	if err != nil {
		return err
	}
	_, manifest, err := readComputeModelsDraft(path, settings)
	if err != nil {
		return err
	}
	ctx := context.Background()
	mirror, err := openComputeModelsMirror(ctx, env, settings)
	if err != nil {
		return err
	}
	sha256Models := computeModelsFileModels(manifest)
	files := model.ComputeModelManifestFiles(manifest)
	sha256IndexSha256s := map[string]string{}
	failedCount := 0
	for _, file := range files {
		label := computeModelsFileLabel(file, sha256Models)
		indexSha256, err := mirror.Index(ctx, file)
		if err != nil {
			failedCount += 1
			fmt.Fprintf(out, "failed   %s  %s: %s\n", file.Sha256, label, err)
			continue
		}
		sha256IndexSha256s[file.Sha256] = indexSha256
		fmt.Fprintf(out, "indexed  %s  %s  index %s\n", file.Sha256, label, indexSha256)
	}
	if 0 < failedCount {
		return fmt.Errorf("%d of %d files were not indexed, so %s is left as it was", failedCount, len(files), path)
	}
	changed := false
	for _, entry := range manifest.Models {
		for _, file := range entry.Files {
			if file.IndexSha256 != sha256IndexSha256s[file.Sha256] {
				changed = true
			}
		}
	}
	if !changed {
		fmt.Fprintf(out, "%s already carries every pin, so it is left as it is\n", path)
		return nil
	}
	pinned, err := model.ComputeModelDraftManifestPinned(manifest, sha256IndexSha256s, settings.infer)
	if err != nil {
		return fmt.Errorf("%s is left as it was: %w", path, err)
	}
	if err := writeComputeModelsFileAtomically(path, pinned); err != nil {
		return fmt.Errorf("%s is left as it was: %w", path, err)
	}
	fmt.Fprintf(out, "wrote the index pins of %d files into %s, whose SHA-256 is now %s; sign it with the release\n",
		len(files), path, computeModelsSha256(pinned))
	return nil
}

// The SHA-256 of data, lowercase hex.
func computeModelsSha256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Writes path through a temporary file beside it, keeping its mode, so a failure leaves the file as it was.
func writeComputeModelsFileAtomically(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	staged, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	if _, err := staged.Write(data); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := os.Chmod(staged.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(staged.Name(), path)
}

// Parses serve's command line, checks the release's signed sums list the manifest, and serves its files.
func runComputeModelsServeWithSettings(args []string, settings *computeModelsCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute models serve", flag.ContinueOnError)
	sumsPath := flags.String("sums", "", "the release's SHA256SUMS")
	signaturePath := flags.String("signature", "", "the release's SHA256SUMS.sig")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	env, path, err := computeModelsPositionals(positionals)
	if err != nil {
		return err
	}
	if *sumsPath == "" || *signaturePath == "" {
		return sidecarctlUsageErrorf("--sums and --signature name the release's signed SHA256SUMS and its signature")
	}
	data, err := readComputeModelsFile(path, "manifest", int64(settings.infer.ManifestMaxByteCount))
	if err != nil {
		return err
	}
	sums, err := readComputeModelsFile(*sumsPath, "SHA256SUMS", settings.mirror.ReleaseSumsMaxByteCount)
	if err != nil {
		return err
	}
	signature, err := readComputeModelsFile(*signaturePath, "SHA256SUMS.sig", settings.mirror.ReleaseSignatureMaxByteCount)
	if err != nil {
		return err
	}
	releaseSums, err := model.VerifyComputeModelReleaseSums(sums, signature, settings.releaseKeys(), settings.mirror)
	if errors.Is(err, sidecar.ErrComputeReleaseKeyAbsent) {
		return fmt.Errorf("nothing was served: no release key exists to check the release's SHA256SUMS, since this build pins none until the key ceremony (RELEASE-KEY.pub)")
	}
	if err != nil {
		return fmt.Errorf("nothing was served: %w", err)
	}
	manifestSha256 := computeModelsSha256(data)
	listedName, listed := releaseSums.Lists(manifestSha256)
	if !listed {
		return fmt.Errorf("nothing was served: the release's signed SHA256SUMS does not list %s, whose SHA-256 is %s", path, manifestSha256)
	}
	manifest, err := sidecar.ParseInferManifest(data, settings.infer)
	if err != nil {
		return fmt.Errorf("nothing was served: %s is no released manifest: %w", path, err)
	}
	ctx := context.Background()
	mirror, err := openComputeModelsMirror(ctx, env, settings)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "the release's signed SHA256SUMS lists %s as %s\n", manifestSha256, listedName)
	sha256Models := computeModelsFileModels(manifest)
	files := model.ComputeModelManifestFiles(manifest)
	failedCount := 0
	for _, file := range files {
		label := computeModelsFileLabel(file, sha256Models)
		if err := mirror.Serve(ctx, file); err != nil {
			failedCount += 1
			fmt.Fprintf(out, "failed  %s  %s: %s\n", file.Sha256, label, err)
			continue
		}
		fmt.Fprintf(out, "served  %s  %s  index %s\n", file.Sha256, label, file.IndexSha256)
	}
	if 0 < failedCount {
		return fmt.Errorf("%d of %d files were not served from %s's mirror", failedCount, len(files), env)
	}
	return nil
}

// Parses status's command line and prints each file's state in the mirror.
func runComputeModelsStatusWithSettings(args []string, settings *computeModelsCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute models status", flag.ContinueOnError)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	env, path, err := computeModelsPositionals(positionals)
	if err != nil {
		return err
	}
	_, manifest, err := readComputeModelsDraft(path, settings)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), settings.statusTimeout)
	defer cancel()
	mirror, err := openComputeModelsMirror(ctx, env, settings)
	if err != nil {
		return err
	}
	sha256Models := computeModelsFileModels(manifest)
	table := &bytes.Buffer{}
	writer := tabwriter.NewWriter(table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "STATE\tSHA256\tBYTES\tINDEX\tFILE")
	for _, file := range model.ComputeModelManifestFiles(manifest) {
		record, err := mirror.Status(ctx, file.Sha256)
		if err != nil {
			return fmt.Errorf("%s's mirror does not read: %w", env, err)
		}
		indexSha256 := record.IndexSha256
		if indexSha256 == "" {
			indexSha256 = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\n", record.State, file.Sha256, file.ByteCount, indexSha256, computeModelsFileLabel(file, sha256Models))
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	_, err = out.Write(table.Bytes())
	return err
}
