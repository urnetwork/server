// Synthetic archives exercise the exact exported Docker/OCI scratch format.
// No production identity, payload, artifact path or daemon is used by tests.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Homogeneous faults keep all unrelated digests consistent, so each assertion
// proves its own identity/permission guard rather than an earlier pin mismatch.
type fixtureOptions struct {
	mode                int64
	payloadChanged      bool
	configUser          string
	configRevision      string
	architecture        string
	entrypoint          string
	duplicateJsonKey    bool
	diffIdChanged       bool
	ociConfigChanged    bool
	layerSourceChanged  bool
	duplicatePayload    bool
	extraPayload        bool
	linkedPayload       bool
	duplicateOuter      bool
	traversalOuter      bool
	linkedOuter         bool
	hiddenSuffix        bool
	compatibilityFields bool
}

// Files are read-only inputs to verification and live in a fresh test root.
type archiveFixture struct {
	native  string
	archive string
	wanted  archiveExpectation
}

// A generic test tar member is not used to extract filesystem content.
type fixtureTarEntry struct {
	name     string
	content  []byte
	mode     int64
	typeflag byte
	linkname string
}

// Deterministic JSON serialization supplies exact source-controlled documents.
func fixtureJson(t *testing.T, value any) []byte {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Small synthetic content is hashed by the same SHA-256 identity format.
func fixtureDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Explicit headers retain permissions/owners that bytes-only checks overlook.
func fixtureTar(t *testing.T, entries []fixtureTarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		size := int64(len(entry.content))
		if entry.typeflag == tar.TypeSymlink {
			size = 0
		}
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: size, Typeflag: entry.typeflag, Uid: 0, Gid: 0, Linkname: entry.linkname}); err != nil {
			t.Fatal(err)
		}
		if size != 0 {
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// The exact actual-format metadata fields are synthetic; no real image identity
// or worker bytes are copied into this fixture, including compatibility extras.
func newArchiveFixture(t *testing.T, options fixtureOptions) archiveFixture {
	t.Helper()
	directory := t.TempDir()
	native := bytes.Repeat([]byte("synthetic immutable worker\n"), 64)
	payload := bytes.Clone(native)
	if options.payloadChanged {
		payload[0] ^= 1
	}
	entry := fixtureTarEntry{name: "competitionworker", content: payload, mode: options.mode, typeflag: tar.TypeReg}
	if options.linkedPayload {
		entry.typeflag, entry.linkname, entry.content = tar.TypeSymlink, "untrusted-target", nil
	}
	layerEntries := []fixtureTarEntry{entry}
	if options.duplicatePayload {
		layerEntries = append(layerEntries, entry)
	}
	if options.extraPayload {
		layerEntries = append(layerEntries, fixtureTarEntry{name: "unknown.bin", content: []byte("synthetic extra"), mode: 0555, typeflag: tar.TypeReg})
	}
	layer := fixtureTar(t, layerEntries)
	layerSha := fixtureDigest(layer)
	layerDescriptor := archiveDescriptor{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: "sha256:" + layerSha, Size: int64(len(layer))}
	config := archiveConfig{Architecture: "amd64", Os: "linux"}
	config.Config.User, config.Config.Entrypoint = "65532:65532", []string{"/competitionworker"}
	config.Config.Labels = map[string]string{"org.opencontainers.image.revision": strings.Repeat("a", 40)}
	config.Rootfs.Type, config.Rootfs.DiffIds = "layers", []string{layerDescriptor.Digest}
	if options.configUser != "" {
		config.Config.User = options.configUser
	}
	if options.configRevision != "" {
		config.Config.Labels["org.opencontainers.image.revision"] = options.configRevision
	}
	if options.architecture != "" {
		config.Architecture = options.architecture
	}
	if options.entrypoint != "" {
		config.Config.Entrypoint = []string{options.entrypoint}
	}
	if options.diffIdChanged {
		config.Rootfs.DiffIds[0] = "sha256:" + strings.Repeat("b", 64)
	}
	configBytes := fixtureJson(t, config)
	if options.compatibilityFields {
		var full map[string]any
		if err := json.Unmarshal(configBytes, &full); err != nil {
			t.Fatal(err)
		}
		full["created"] = "2035-01-02T03:04:05Z"
		full["history"] = []any{map[string]any{"created_by": "COPY competitionworker /competitionworker # buildkit"}, map[string]any{"created_by": "USER 65532:65532", "empty_layer": true}}
		fullConfig := full["config"].(map[string]any)
		fullConfig["Env"], fullConfig["WorkingDir"] = []string{"PATH=/usr/bin:/bin"}, "/"
		configBytes = fixtureJson(t, full)
	}
	if options.duplicateJsonKey {
		configBytes = bytes.Replace(configBytes, []byte(`"User":"65532:65532"`), []byte(`"User":"65532:65532","User":"65532:65532"`), 1)
	}
	configSha := fixtureDigest(configBytes)
	configDescriptor := archiveDescriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:" + configSha, Size: int64(len(configBytes))}
	if options.ociConfigChanged {
		configDescriptor.Digest = "sha256:" + strings.Repeat("c", 64)
	}
	imageManifest := fixtureJson(t, struct {
		SchemaVersion int                 `json:"schemaVersion"`
		MediaType     string              `json:"mediaType"`
		Config        archiveDescriptor   `json:"config"`
		Layers        []archiveDescriptor `json:"layers"`
	}{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []archiveDescriptor{layerDescriptor}})
	manifestSha := fixtureDigest(imageManifest)
	tag := "archive-fixture.example/worker:synthetic"
	index := fixtureJson(t, struct {
		SchemaVersion int                 `json:"schemaVersion"`
		MediaType     string              `json:"mediaType"`
		Manifests     []archiveDescriptor `json:"manifests"`
	}{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []archiveDescriptor{{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:" + manifestSha, Size: int64(len(imageManifest)), Annotations: map[string]string{"io.containerd.image.name": "docker.io/" + tag, "org.opencontainers.image.ref.name": "synthetic"}}}})
	manifest := archiveManifest{Config: "blobs/sha256/" + configSha, RepoTags: []string{tag}, Layers: []string{"blobs/sha256/" + layerSha}, LayerSources: map[string]archiveDescriptor{layerDescriptor.Digest: layerDescriptor}}
	if options.layerSourceChanged {
		changed := layerDescriptor
		changed.Size++
		manifest.LayerSources[layerDescriptor.Digest] = changed
	}
	manifestBytes := fixtureJson(t, []archiveManifest{manifest})
	outer := []fixtureTarEntry{
		{name: "blobs/", mode: 0755, typeflag: tar.TypeDir}, {name: "blobs/sha256/", mode: 0755, typeflag: tar.TypeDir},
		{name: "blobs/sha256/" + layerSha, content: layer, mode: 0644, typeflag: tar.TypeReg},
		{name: "blobs/sha256/" + configSha, content: configBytes, mode: 0644, typeflag: tar.TypeReg},
		{name: "blobs/sha256/" + manifestSha, content: imageManifest, mode: 0644, typeflag: tar.TypeReg},
		{name: "index.json", content: index, mode: 0644, typeflag: tar.TypeReg},
		{name: "oci-layout", content: []byte(`{"imageLayoutVersion":"1.0.0"}`), mode: 0644, typeflag: tar.TypeReg},
		{name: "manifest.json", content: manifestBytes, mode: 0644, typeflag: tar.TypeReg},
	}
	if options.compatibilityFields {
		legacy := fixtureJson(t, map[string]any{"id": strings.Repeat("d", 64), "created": "2035-01-02T03:04:05Z", "container_config": map[string]any{"User": "", "Env": nil}, "config": config.Config, "architecture": "amd64", "os": "linux"})
		outer = append(outer, fixtureTarEntry{name: "blobs/sha256/" + fixtureDigest(legacy), content: legacy, mode: 0644, typeflag: tar.TypeReg}, fixtureTarEntry{name: "repositories", content: []byte(`{}`), mode: 0644, typeflag: tar.TypeReg})
	}
	if options.duplicateOuter {
		outer = append(outer, fixtureTarEntry{name: "manifest.json", content: manifestBytes, mode: 0644, typeflag: tar.TypeReg})
	}
	if options.traversalOuter {
		outer = append(outer, fixtureTarEntry{name: "../escaped", content: []byte("not extracted"), mode: 0644, typeflag: tar.TypeReg})
	}
	if options.linkedOuter {
		outer = append(outer, fixtureTarEntry{name: "alias", mode: 0777, typeflag: tar.TypeSymlink, linkname: "manifest.json"})
	}
	archive := fixtureTar(t, outer)
	if options.hiddenSuffix {
		archive = append(archive, []byte("synthetic hidden suffix")...)
	}
	fixture := archiveFixture{native: filepath.Join(directory, "native"), archive: filepath.Join(directory, "image.tar"), wanted: archiveExpectation{
		NativeSha256: fixtureDigest(native), ArchiveSha256: fixtureDigest(archive), ImageId: "sha256:" + configSha, RepoTag: tag, Revision: strings.Repeat("a", 40), User: "65532:65532"}}
	if err := os.WriteFile(fixture.native, native, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.archive, archive, 0600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Root-owned 0700 bytes are identical but unusable by the configured nonroot.
func TestArchiveRejectsInheritedRootOnlyPayloadMode(t *testing.T) {
	fixture := newArchiveFixture(t, fixtureOptions{mode: 0700})
	report, err := verifyArchive(t.Context(), fixture.native, fixture.archive, fixture.wanted, defaultVerifierSettings())
	if !errors.Is(err, errPayloadPermission) || report.Passed || report.Code != "payload_permission" || report.PayloadMode != "0700" || report.PayloadSha256 != fixture.wanted.NativeSha256 {
		t.Fatal("byte-identical but unusable image did not fail its permission assertion", report, err)
	}
}

// The fixed mode is usable and immutable for every configured nonroot uid/gid.
func TestArchiveAcceptsImmutableNonrootExecutable(t *testing.T) {
	fixture := newArchiveFixture(t, fixtureOptions{mode: 0555})
	report, err := verifyArchive(t.Context(), fixture.native, fixture.archive, fixture.wanted, defaultVerifierSettings())
	if err != nil || !report.Passed || report.PayloadMode != "0555" || report.PayloadSha256 != fixture.wanted.NativeSha256 || report.ImageId != fixture.wanted.ImageId {
		t.Fatal(report, err)
	}
}

// Modern Docker-save OCI indexing and its extra legacy blob remain supported.
func TestArchiveAcceptsExactDockerSaveCompatibilityMetadata(t *testing.T) {
	fixture := newArchiveFixture(t, fixtureOptions{mode: 0555, compatibilityFields: true})
	report, err := verifyArchive(t.Context(), fixture.native, fixture.archive, fixture.wanted, defaultVerifierSettings())
	if err != nil || !report.Passed {
		t.Fatal("actual-format compatibility metadata was rejected", report, err)
	}
}

// Wrong bytes plus the old mode are not the known packaging-only defect.
func TestArchiveWrongPayloadCannotBecomePermissionOnlyFailure(t *testing.T) {
	fixture := newArchiveFixture(t, fixtureOptions{mode: 0700, payloadChanged: true})
	report, err := verifyArchive(t.Context(), fixture.native, fixture.archive, fixture.wanted, defaultVerifierSettings())
	if err == nil || errors.Is(err, errPayloadPermission) || report.Passed || report.Code != "payload_identity" {
		t.Fatal(report, err)
	}
}

// All outer/config/layer identities remain mandatory before permission success.
func TestArchiveRejectsIdentityAndMetadataFaults(t *testing.T) {
	for _, options := range []fixtureOptions{
		{mode: 0555, configUser: "0:0"}, {mode: 0555, configRevision: strings.Repeat("b", 40)}, {mode: 0555, architecture: "arm64"},
		{mode: 0555, entrypoint: "/another-worker"}, {mode: 0555, duplicateJsonKey: true}, {mode: 0555, diffIdChanged: true},
		{mode: 0555, ociConfigChanged: true}, {mode: 0555, layerSourceChanged: true}, {mode: 0555, duplicatePayload: true},
		{mode: 0555, extraPayload: true}, {mode: 0555, linkedPayload: true}, {mode: 0555, duplicateOuter: true},
		{mode: 0555, traversalOuter: true}, {mode: 0555, linkedOuter: true}, {mode: 0555, hiddenSuffix: true},
	} {
		fixture := newArchiveFixture(t, options)
		report, err := verifyArchive(t.Context(), fixture.native, fixture.archive, fixture.wanted, defaultVerifierSettings())
		if err == nil || report.Passed || errors.Is(err, errPayloadPermission) {
			t.Fatal("unknown image fault was accepted or mislabeled as permissions", options, report, err)
		}
	}
}

// External receipt pins cannot be replaced by matching claims inside the tar.
func TestArchiveRejectsChangedExpectedPins(t *testing.T) {
	for _, fault := range []string{"native", "archive", "image", "tag", "revision"} {
		fixture := newArchiveFixture(t, fixtureOptions{mode: 0555})
		switch fault {
		case "native":
			fixture.wanted.NativeSha256 = strings.Repeat("0", 64)
		case "archive":
			fixture.wanted.ArchiveSha256 = strings.Repeat("0", 64)
		case "image":
			fixture.wanted.ImageId = "sha256:" + strings.Repeat("0", 64)
		case "tag":
			fixture.wanted.RepoTag = "another-fixture.example/worker:synthetic"
		case "revision":
			fixture.wanted.Revision = strings.Repeat("b", 40)
		}
		report, err := verifyArchive(t.Context(), fixture.native, fixture.archive, fixture.wanted, defaultVerifierSettings())
		if err == nil || report.Passed {
			t.Fatal("changed external identity pin was accepted", fault, report)
		}
	}
}

// Owned bounds fail closed rather than allocating to the archive's declarations.
func TestArchiveRejectsBoundsAndCancellation(t *testing.T) {
	for _, fault := range []string{"archive", "native", "layer", "entries", "document", "metadata", "depth", "values", "disabled", "canceled"} {
		fixture := newArchiveFixture(t, fixtureOptions{mode: 0555})
		settings := defaultVerifierSettings()
		ctx, cancel := context.WithCancel(t.Context())
		switch fault {
		case "archive":
			settings.MaximumArchiveBytes = 1
		case "native":
			settings.MaximumNativeBytes = 1
		case "layer":
			settings.MaximumLayerBytes = 1
		case "entries":
			settings.MaximumEntries = 1
		case "document":
			settings.MaximumDocumentBytes = 1
		case "metadata":
			settings.MaximumMetadataBytes = 1
		case "depth":
			settings.MaximumJsonDepth = 1
		case "values":
			settings.MaximumJsonValues = 1
		case "disabled":
			settings.CopyBufferBytes = 0
		case "canceled":
			cancel()
		}
		report, err := verifyArchive(ctx, fixture.native, fixture.archive, fixture.wanted, settings)
		cancel()
		if err == nil || report.Passed {
			t.Fatal("missing bound or cancellation acquired image authority", fault, report)
		}
	}
}

// An alias is not the root-protected artifact whose independent pin was given.
func TestArchiveRejectsSymlinkArtifacts(t *testing.T) {
	for _, which := range []string{"native", "archive"} {
		fixture := newArchiveFixture(t, fixtureOptions{mode: 0555})
		alias := filepath.Join(t.TempDir(), "artifact-alias")
		target := fixture.native
		if which == "archive" {
			target = fixture.archive
		}
		if err := os.Symlink(target, alias); err != nil {
			t.Fatal(err)
		}
		if which == "native" {
			fixture.native = alias
		} else {
			fixture.archive = alias
		}
		report, err := verifyArchive(t.Context(), fixture.native, fixture.archive, fixture.wanted, defaultVerifierSettings())
		if err == nil || report.Passed {
			t.Fatal("artifact symlink was accepted", which, report)
		}
	}
}
