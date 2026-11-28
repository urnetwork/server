// Passive verification of the source-owned single-payload scratch image format.
// Archives are streamed and never extracted, executed, loaded or sent to Docker.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Permission rejection is distinct from corrupted/unknown packaging identity.
var errPayloadPermission = errors.New("image payload is not the declared immutable nonroot executable")

// All work and memory limits belong to one verifier invocation; nonpositive
// values are invalid, never an unbounded or disabled verification mode.
type verifierSettings struct {
	MaximumArchiveBytes  int64
	MaximumNativeBytes   int64
	MaximumLayerBytes    int64
	MaximumDocumentBytes int64
	MaximumMetadataBytes int64
	MaximumEntries       int
	MaximumJsonDepth     int
	MaximumJsonValues    int
	CopyBufferBytes      int
	TimeBudget           time.Duration
}

// Bounds cover the measured worker with room for compilation growth, not an
// arbitrary base image, multiple payload layers or a generic archive reaper.
func defaultVerifierSettings() verifierSettings {
	return verifierSettings{MaximumArchiveBytes: 512 * 1024 * 1024, MaximumNativeBytes: 256 * 1024 * 1024,
		MaximumLayerBytes: 512 * 1024 * 1024, MaximumDocumentBytes: 1024 * 1024, MaximumMetadataBytes: 8 * 1024 * 1024,
		MaximumEntries: 64, MaximumJsonDepth: 32, MaximumJsonValues: 4096, CopyBufferBytes: 64 * 1024, TimeBudget: time.Minute}
}

// Expected identities come from reviewed build receipts, not archive claims.
type archiveExpectation struct {
	NativeSha256  string
	ArchiveSha256 string
	ImageId       string
	RepoTag       string
	Revision      string
	User          string
}

// Only finite public artifact identities leave the verifier; no raw JSON,
// arbitrary archive names, filesystem errors or captured payload bytes escape.
type archiveReport struct {
	Passed         bool   `json:"passed"`
	Phase          string `json:"phase"`
	Code           string `json:"code"`
	NativeSha256   string `json:"native_sha256,omitempty"`
	ArchiveSha256  string `json:"archive_sha256,omitempty"`
	ImageId        string `json:"image_id,omitempty"`
	LayerSha256    string `json:"layer_sha256,omitempty"`
	PayloadSha256  string `json:"payload_sha256,omitempty"`
	PayloadBytes   int64  `json:"payload_bytes,omitempty"`
	PayloadMode    string `json:"payload_mode,omitempty"`
	ConfiguredUser string `json:"configured_user,omitempty"`
	Revision       string `json:"revision,omitempty"`
}

// The outer inventory stores only bounded small documents and digest metadata.
type archiveEntry struct {
	bytes  int64
	sha256 string
	doc    []byte
}

// Docker's manifest and OCI descriptors must describe the same single image.
type archiveDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

// The retained source format uses an OCI-indexed Docker image save archive.
type archiveManifest struct {
	Config       string                       `json:"Config"`
	RepoTags     []string                     `json:"RepoTags"`
	Layers       []string                     `json:"Layers"`
	LayerSources map[string]archiveDescriptor `json:"LayerSources"`
}

// Configuration must preserve the reviewed runtime identity and entry point.
type archiveConfig struct {
	Architecture string `json:"architecture"`
	Os           string `json:"os"`
	Config       struct {
		User       string            `json:"User"`
		Entrypoint []string          `json:"Entrypoint"`
		Labels     map[string]string `json:"Labels"`
	} `json:"config"`
	Rootfs struct {
		Type    string   `json:"type"`
		DiffIds []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// Cancellation is checked between each bounded streaming read, not deferred
// until the archive has already been copied or parsed in full.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// File or kernel I/O already executing has no fabricated all-platform bound.
func (self contextReader) Read(content []byte) (int, error) {
	if err := self.ctx.Err(); err != nil {
		return 0, err
	}
	return self.reader.Read(content)
}

// Zero padding is the only permitted suffix after either complete tar stream.
type zeroPaddingWriter struct{}

// A second hidden tar or nonzero suffix never acquires archive identity.
func (self zeroPaddingWriter) Write(content []byte) (int, error) {
	for _, value := range content {
		if value != 0 {
			return 0, errors.New("nonzero archive suffix")
		}
	}
	return len(content), nil
}

// Input paths select only regular owned artifacts, not extraction destinations.
func openArtifact(name string, maximum int64) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || maximum < before.Size() {
		return nil, nil, errors.New("artifact is not a bounded regular file")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	opened, statErr := file.Stat()
	after, pathErr := os.Lstat(name)
	if statErr != nil || pathErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		file.Close()
		return nil, nil, errors.New("artifact identity changed while opening")
	}
	return file, opened, nil
}

// Digests are canonical lowercase SHA-256, with no aliases or empty pins.
func validDigest(value string) bool {
	content, err := hex.DecodeString(value)
	return err == nil && len(content) == sha256.Size && hex.EncodeToString(content) == value
}

// Paths are compared within streams only; none is passed to filesystem writes.
func canonicalArchiveName(name string, directory bool) (string, error) {
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	name = strings.TrimPrefix(name, "./")
	if directory && (name == "" || name == ".") {
		return ".", nil
	}
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
		return "", errors.New("noncanonical archive name")
	}
	return name, nil
}

// Duplicate keys and excessive nested metadata fail before typed projection.
func decodeArchiveJson(content []byte, destination any, settings verifierSettings) error {
	if len(content) == 0 || settings.MaximumDocumentBytes < int64(len(content)) {
		return errors.New("metadata document exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	values := 0
	var visit func(int) error
	visit = func(depth int) error {
		values++
		if settings.MaximumJsonDepth < depth || settings.MaximumJsonValues < values {
			return errors.New("metadata structure exceeds bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for decoder.More() {
					key, err := decoder.Token()
					name, ok := key.(string)
					if err != nil || !ok || seen[name] {
						return errors.New("duplicate or malformed metadata key")
					}
					seen[name] = true
					if err := visit(depth + 1); err != nil {
						return err
					}
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim('}') {
					return errors.New("unterminated metadata object")
				}
			case '[':
				for decoder.More() {
					if err := visit(depth + 1); err != nil {
						return err
					}
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim(']') {
					return errors.New("unterminated metadata array")
				}
			default:
				return errors.New("unexpected metadata delimiter")
			}
		}
		return nil
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("metadata has trailing values")
	}
	return json.Unmarshal(content, destination)
}

// Native and archive file content is streamed with bounded scratch memory.
func hashArtifact(ctx context.Context, file *os.File, settings verifierSettings) (string, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	digest := sha256.New()
	count, err := io.CopyBuffer(digest, contextReader{ctx: ctx, reader: file}, make([]byte, settings.CopyBufferBytes))
	return hex.EncodeToString(digest.Sum(nil)), count, err
}

// The complete outer tar and every named blob are independently hashed.
func inventoryArchive(ctx context.Context, file *os.File, settings verifierSettings) (map[string]archiveEntry, string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	digest := sha256.New()
	stream := io.TeeReader(contextReader{ctx: ctx, reader: file}, digest)
	reader := tar.NewReader(stream)
	entries := map[string]archiveEntry{}
	metadataBytes, examined := int64(0), 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		examined++
		if err != nil || settings.MaximumEntries < examined || header.Size < 0 || settings.MaximumArchiveBytes < header.Size {
			return nil, "", errors.New("outer archive is malformed or exceeds bound")
		}
		name, err := canonicalArchiveName(header.Name, header.Typeflag == tar.TypeDir)
		if err != nil {
			return nil, "", err
		}
		if _, duplicate := entries[name]; duplicate {
			return nil, "", errors.New("duplicate outer archive entry")
		}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return nil, "", errors.New("directory contains unexpected data")
			}
			entries[name] = archiveEntry{}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, "", errors.New("outer archive contains a link or special file")
		}
		entryDigest := sha256.New()
		var document bytes.Buffer
		var destination io.Writer = entryDigest
		if header.Size <= settings.MaximumDocumentBytes {
			metadataBytes += header.Size
			if settings.MaximumMetadataBytes < metadataBytes {
				return nil, "", errors.New("metadata inventory exceeds bound")
			}
			destination = io.MultiWriter(destination, &document)
		}
		count, err := io.CopyBuffer(destination, reader, make([]byte, settings.CopyBufferBytes))
		if err != nil || count != header.Size {
			return nil, "", errors.New("outer archive entry is truncated")
		}
		entry := archiveEntry{bytes: count, sha256: hex.EncodeToString(entryDigest.Sum(nil)), doc: document.Bytes()}
		if strings.HasPrefix(name, "blobs/sha256/") && strings.TrimPrefix(name, "blobs/sha256/") != entry.sha256 {
			return nil, "", errors.New("content-addressed blob identity mismatch")
		}
		entries[name] = entry
	}
	if _, err := io.CopyBuffer(zeroPaddingWriter{}, stream, make([]byte, settings.CopyBufferBytes)); err != nil {
		return nil, "", err
	}
	return entries, hex.EncodeToString(digest.Sum(nil)), nil
}

// A descriptor must select an existing exact content-addressed document.
func descriptorEntry(entries map[string]archiveEntry, descriptor archiveDescriptor) (archiveEntry, error) {
	if !strings.HasPrefix(descriptor.Digest, "sha256:") || !validDigest(strings.TrimPrefix(descriptor.Digest, "sha256:")) {
		return archiveEntry{}, errors.New("invalid OCI descriptor digest")
	}
	entry, ok := entries["blobs/sha256/"+strings.TrimPrefix(descriptor.Digest, "sha256:")]
	if !ok || entry.bytes != descriptor.Size || entry.sha256 != strings.TrimPrefix(descriptor.Digest, "sha256:") {
		return archiveEntry{}, errors.New("OCI descriptor does not match retained blob")
	}
	return entry, nil
}

// Docker and OCI references, platform, revision and single-layer identity agree.
func verifyMetadata(entries map[string]archiveEntry, wanted archiveExpectation, settings verifierSettings) (string, archiveConfig, error) {
	var manifests []archiveManifest
	config := archiveConfig{}
	if err := decodeArchiveJson(entries["manifest.json"].doc, &manifests, settings); err != nil || len(manifests) != 1 {
		return "", config, errors.New("archive does not contain one Docker manifest")
	}
	manifest := manifests[0]
	configName := "blobs/sha256/" + strings.TrimPrefix(wanted.ImageId, "sha256:")
	if manifest.Config != configName || len(manifest.RepoTags) != 1 || manifest.RepoTags[0] != wanted.RepoTag || len(manifest.Layers) != 1 || len(manifest.LayerSources) != 1 {
		return "", config, errors.New("Docker manifest changed expected image identity")
	}
	configEntry, ok := entries[configName]
	if !ok || "sha256:"+configEntry.sha256 != wanted.ImageId || decodeArchiveJson(configEntry.doc, &config, settings) != nil ||
		config.Architecture != "amd64" || config.Os != "linux" || config.Config.User != wanted.User || len(config.Config.Entrypoint) != 1 || config.Config.Entrypoint[0] != "/competitionworker" ||
		config.Config.Labels["org.opencontainers.image.revision"] != wanted.Revision || config.Rootfs.Type != "layers" || len(config.Rootfs.DiffIds) != 1 {
		return "", config, errors.New("configuration changed platform, user, entrypoint or revision")
	}
	layerName := manifest.Layers[0]
	layerEntry, ok := entries[layerName]
	layerDigest := "sha256:" + layerEntry.sha256
	layerSource, hasSource := manifest.LayerSources[layerDigest]
	if !ok || layerName != "blobs/sha256/"+layerEntry.sha256 || config.Rootfs.DiffIds[0] != layerDigest || !hasSource || layerSource.MediaType != "application/vnd.oci.image.layer.v1.tar" || layerSource.Digest != layerDigest || layerSource.Size != layerEntry.bytes {
		return "", config, errors.New("layer descriptor is not the exact uncompressed scratch layer")
	}
	var layout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if decodeArchiveJson(entries["oci-layout"].doc, &layout, settings) != nil || layout.ImageLayoutVersion != "1.0.0" {
		return "", config, errors.New("unsupported OCI archive layout")
	}
	var index struct {
		SchemaVersion int                 `json:"schemaVersion"`
		MediaType     string              `json:"mediaType"`
		Manifests     []archiveDescriptor `json:"manifests"`
	}
	if decodeArchiveJson(entries["index.json"].doc, &index, settings) != nil || index.SchemaVersion != 2 || index.MediaType != "application/vnd.oci.image.index.v1+json" || len(index.Manifests) != 1 {
		return "", config, errors.New("archive does not contain one OCI image index")
	}
	image := index.Manifests[0]
	imageName := image.Annotations["io.containerd.image.name"]
	if image.MediaType != "application/vnd.oci.image.manifest.v1+json" || imageName != wanted.RepoTag && imageName != "docker.io/"+wanted.RepoTag {
		return "", config, errors.New("OCI index selected another image")
	}
	imageEntry, err := descriptorEntry(entries, image)
	if err != nil {
		return "", config, err
	}
	var imageManifest struct {
		SchemaVersion int                 `json:"schemaVersion"`
		MediaType     string              `json:"mediaType"`
		Config        archiveDescriptor   `json:"config"`
		Layers        []archiveDescriptor `json:"layers"`
	}
	if decodeArchiveJson(imageEntry.doc, &imageManifest, settings) != nil || imageManifest.SchemaVersion != 2 || imageManifest.MediaType != image.MediaType ||
		imageManifest.Config.MediaType != "application/vnd.oci.image.config.v1+json" || imageManifest.Config.Digest != wanted.ImageId || imageManifest.Config.Size != configEntry.bytes || len(imageManifest.Layers) != 1 ||
		imageManifest.Layers[0].Digest != layerDigest || imageManifest.Layers[0].Size != layerEntry.bytes || imageManifest.Layers[0].MediaType != layerSource.MediaType {
		return "", config, errors.New("OCI and Docker configuration/layer identities disagree")
	}
	return layerName, config, nil
}

// The only payload is hashed before mode classification; wrong bytes can never
// masquerade as the known packaging-only permission defect.
func verifyLayer(ctx context.Context, archive *os.File, layerName string, expectedEntry archiveEntry, nativeSha string, nativeBytes int64, settings verifierSettings) (int64, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	outer := tar.NewReader(contextReader{ctx: ctx, reader: archive})
	for {
		header, err := outer.Next()
		if err != nil {
			return 0, errors.New("expected layer is unavailable")
		}
		name, err := canonicalArchiveName(header.Name, header.Typeflag == tar.TypeDir)
		if err != nil {
			return 0, err
		}
		if name != layerName {
			continue
		}
		if header.Size != expectedEntry.bytes || settings.MaximumLayerBytes < header.Size {
			return 0, errors.New("layer exceeds its bound or identity")
		}
		layerHash := sha256.New()
		stream := io.TeeReader(outer, layerHash)
		layer := tar.NewReader(stream)
		payloads, examined := 0, 0
		mode := int64(0)
		var payloadHash string
		for {
			entry, err := layer.Next()
			if err == io.EOF {
				break
			}
			examined++
			if err != nil || settings.MaximumEntries < examined {
				return 0, errors.New("malformed or excessive layer inventory")
			}
			entryName, err := canonicalArchiveName(entry.Name, entry.Typeflag == tar.TypeDir)
			if err != nil {
				return 0, err
			}
			if entry.Typeflag == tar.TypeDir && entryName == "." && entry.Size == 0 && entry.Mode&0001 != 0 {
				continue
			}
			if entryName != "competitionworker" || entry.Typeflag != tar.TypeReg && entry.Typeflag != tar.TypeRegA || entry.Size != nativeBytes || entry.Uid != 0 || entry.Gid != 0 {
				return 0, errors.New("layer contains an unknown, linked or differently owned payload")
			}
			payloads++
			if payloads != 1 {
				return 0, errors.New("duplicate executable payload")
			}
			payload := sha256.New()
			count, err := io.CopyBuffer(payload, layer, make([]byte, settings.CopyBufferBytes))
			if err != nil || count != nativeBytes {
				return 0, errors.New("payload is truncated")
			}
			payloadHash, mode = hex.EncodeToString(payload.Sum(nil)), entry.Mode
		}
		if _, err := io.CopyBuffer(zeroPaddingWriter{}, stream, make([]byte, settings.CopyBufferBytes)); err != nil {
			return 0, err
		}
		if payloads != 1 || payloadHash != nativeSha || hex.EncodeToString(layerHash.Sum(nil)) != expectedEntry.sha256 {
			return 0, errors.New("payload bytes or complete layer digest changed")
		}
		return mode, nil
	}
}

// The verifier rejects malformed expectations before opening any artifact.
func verifyArchive(ctx context.Context, nativeName, archiveName string, wanted archiveExpectation, settings verifierSettings) (archiveReport, error) {
	report := archiveReport{Phase: "admission", Code: "invalid_expectation"}
	revision, revisionErr := hex.DecodeString(wanted.Revision)
	if ctx == nil || settings.MaximumArchiveBytes <= 0 || settings.MaximumNativeBytes <= 0 || settings.MaximumLayerBytes <= 0 || settings.MaximumDocumentBytes <= 0 || settings.MaximumMetadataBytes <= 0 ||
		settings.MaximumEntries <= 0 || settings.MaximumJsonDepth <= 0 || settings.MaximumJsonValues <= 0 || settings.CopyBufferBytes <= 0 || settings.TimeBudget <= 0 ||
		!validDigest(wanted.NativeSha256) || !validDigest(wanted.ArchiveSha256) || !strings.HasPrefix(wanted.ImageId, "sha256:") || !validDigest(strings.TrimPrefix(wanted.ImageId, "sha256:")) || revisionErr != nil || len(revision) != 20 || hex.EncodeToString(revision) != wanted.Revision || wanted.RepoTag == "" {
		return report, errors.New("invalid verifier expectation or bounds")
	}
	userParts := strings.Split(wanted.User, ":")
	if len(userParts) != 2 {
		return report, errors.New("expected user must name one numeric nonroot uid/gid")
	}
	for _, value := range userParts {
		number, err := strconv.ParseUint(value, 10, 32)
		if err != nil || number == 0 || strconv.FormatUint(number, 10) != value {
			return report, errors.New("expected user is not canonical nonroot")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, settings.TimeBudget)
	defer cancel()
	if err := ctx.Err(); err != nil {
		report.Code = "canceled"
		return report, err
	}
	report.Phase, report.Code = "native", "native_identity"
	native, nativeInfo, err := openArtifact(nativeName, settings.MaximumNativeBytes)
	if err != nil {
		return report, err
	}
	defer native.Close()
	nativeSha, nativeBytes, err := hashArtifact(ctx, native, settings)
	if err != nil || nativeSha != wanted.NativeSha256 || nativeBytes != nativeInfo.Size() {
		return report, errors.New("native bytes changed")
	}
	report.NativeSha256 = nativeSha
	report.Phase, report.Code = "archive", "archive_identity"
	archive, archiveInfo, err := openArtifact(archiveName, settings.MaximumArchiveBytes)
	if err != nil {
		return report, err
	}
	defer archive.Close()
	entries, archiveSha, err := inventoryArchive(ctx, archive, settings)
	if err != nil || archiveSha != wanted.ArchiveSha256 {
		return report, errors.New("archive bytes or inventory changed")
	}
	report.ArchiveSha256 = archiveSha
	report.Phase, report.Code = "metadata", "config_layer_identity"
	layerName, config, err := verifyMetadata(entries, wanted, settings)
	if err != nil {
		return report, err
	}
	report.ImageId, report.ConfiguredUser, report.Revision = wanted.ImageId, config.Config.User, wanted.Revision
	report.LayerSha256 = entries[layerName].sha256
	report.Phase, report.Code = "payload", "payload_identity"
	mode, err := verifyLayer(ctx, archive, layerName, entries[layerName], nativeSha, nativeBytes, settings)
	if err != nil {
		return report, err
	}
	report.PayloadSha256, report.PayloadBytes, report.PayloadMode = nativeSha, nativeBytes, fmt.Sprintf("%04o", mode)
	// Rehash after parsing to refuse an artifact changing between either pass.
	finalArchiveSha, finalArchiveBytes, err := hashArtifact(ctx, archive, settings)
	finalNativeSha, finalNativeBytes, nativeErr := hashArtifact(ctx, native, settings)
	if err != nil || nativeErr != nil || finalArchiveSha != archiveSha || finalArchiveBytes != archiveInfo.Size() || finalNativeSha != nativeSha || finalNativeBytes != nativeBytes || ctx.Err() != nil ||
		!sameArtifact(nativeName, native, nativeInfo) || !sameArtifact(archiveName, archive, archiveInfo) {
		report.Code = "artifact_changed_or_canceled"
		return report, errors.New("artifact or request changed before final publication")
	}
	if mode != 0555 {
		report.Phase, report.Code = "permissions", "payload_permission"
		return report, errPayloadPermission
	}
	report.Passed, report.Phase, report.Code = true, "complete", "verified"
	return report, nil
}

// Fresh descriptor/path checks keep a successful report bound to both original
// regular artifacts, rather than only an old open descriptor after a path swap.
func sameArtifact(name string, file *os.File, before os.FileInfo) bool {
	opened, openErr := file.Stat()
	current, pathErr := os.Lstat(name)
	return openErr == nil && pathErr == nil && current.Mode().IsRegular() && os.SameFile(before, opened) && os.SameFile(before, current) &&
		opened.Size() == before.Size() && opened.ModTime() == before.ModTime() && opened.Mode() == before.Mode()
}
