// Preparation and verification stream opaque files; neither interprets outcomes
// nor decides whether calibration passed. Every source file must be inventoried.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Invalid settings refuse rather than silently disabling a resource limit.
func validateSettings(settings archiveSettings) error {
	if settings.MaximumArchiveBytes <= 0 || settings.MaximumArchiveBytes > math.MaxInt64-512 ||
		settings.MaximumMetadataBytes <= 0 || settings.MaximumMetadataBytes == math.MaxInt64 ||
		settings.MaximumEntries <= 0 || settings.MaximumTreeEntries < settings.MaximumEntries ||
		settings.MaximumSourceDepth <= 0 || settings.DirectoryBatchEntries <= 0 ||
		settings.DirectoryBatchEntries > settings.MaximumTreeEntries ||
		settings.MaximumMetadataDepth <= 0 || settings.MinimumFreeBytes <= 0 ||
		settings.MaximumStagingBytes <= 0 || settings.BufferBytes <= 0 ||
		int64(settings.BufferBytes) > settings.MaximumMetadataBytes ||
		settings.VerificationTimeout <= 0 || settings.UploadTimeout <= 0 {
		return errors.New("positive bounded archive settings required")
	}
	return nil
}

// A nonce names storage only, never a calibration seed or replacement workload.
func workDirectory(policy archivePolicy, nonce string) (string, error) {
	decoded, err := hex.DecodeString(nonce)
	if err != nil || len(decoded) != 24 || hex.EncodeToString(decoded) != nonce ||
		!validDigest(policy.PublicPlanSha256) || policy.Context == "" || policy.Owner == "" ||
		!filepath.IsAbs(policy.WorkParent) || filepath.Clean(policy.WorkParent) != policy.WorkParent ||
		policy.WorkPrefix == "" || strings.ContainsAny(policy.WorkPrefix, "/\\\x00\r\n") ||
		!validMember(policy.KeyPrefix) || policy.Bucket == "" {
		return "", errors.New("new canonical archive namespace required")
	}
	return filepath.Join(policy.WorkParent, policy.WorkPrefix+nonce), nil
}

// Only the exact compiled first-committed roots are eligible, never a bridge tree.
func sourceUnit(root string, policy archivePolicy) (string, error) {
	unit, exists := policy.SourceRootUnits[root]
	if !exists || !filepath.IsAbs(root) || filepath.Clean(root) != root ||
		!validMember(unit) || strings.Contains(unit, "/") {
		return "", errors.New("source is outside the first-committed context")
	}
	if err := checkPath(root, policy, false); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", errors.New("source root must be a protected private directory")
	}
	return unit, nil
}

// The complete bounded census includes failed/partial artifacts without judgment.
// Directory-only entries are not tar members; links and special files always refuse.
func censusSource(ctx context.Context, root string, policy archivePolicy, settings archiveSettings) ([]archiveEntry, error) {
	if _, err := sourceUnit(root, policy); err != nil {
		return nil, err
	}
	entries := []archiveEntry{}
	examined := 0
	totalBytes := int64(0)
	var walk func(string, int) error
	walk = func(value string, depth int) error {
		if ctx.Err() != nil || depth > settings.MaximumSourceDepth {
			return errors.New("source census interrupted or unavailable")
		}
		examined++
		if examined > settings.MaximumTreeEntries {
			return errors.New("source tree exceeds its entry bound")
		}
		if err := checkPath(value, policy, false); err != nil {
			return err
		}
		info, err := os.Lstat(value)
		if err != nil {
			return errors.New("source member changed during census")
		}
		if info.IsDir() {
			fd, err := syscall.Open(value, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
			if err != nil {
				return errors.New("source directory open refused")
			}
			directory := os.NewFile(uintptr(fd), value)
			defer directory.Close()
			opened, err := directory.Stat()
			if err != nil || !os.SameFile(info, opened) {
				return errors.New("source directory changed during open")
			}
			for {
				if ctx.Err() != nil {
					return errors.New("source directory census canceled")
				}
				batchSize := min(settings.DirectoryBatchEntries, settings.MaximumTreeEntries-examined+1)
				children, readErr := directory.ReadDir(batchSize)
				for _, child := range children {
					if err := walk(filepath.Join(value, child.Name()), depth+1); err != nil {
						return err
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					return errors.New("source directory read incomplete")
				}
			}
			return recheckFile(directory, opened)
		}
		if len(entries) >= settings.MaximumEntries || !info.Mode().IsRegular() {
			return errors.New("source census contains special files or exceeds its bound")
		}
		relative, err := filepath.Rel(root, value)
		if err != nil {
			return errors.New("source relative path unavailable")
		}
		name := filepath.ToSlash(relative)
		if !validMember(name) || name == inventoryMemberName {
			return errors.New("source member path is unsafe or reserved")
		}
		record, err := hashSource(ctx, value, policy, settings)
		if err != nil {
			return err
		}
		if record.Bytes > settings.MaximumArchiveBytes-totalBytes {
			return errors.New("source payload exceeds archive bound")
		}
		record.Name = name
		totalBytes += record.Bytes
		entries = append(entries, record)
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("empty source evidence census refused")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// Ustar block sizes are format facts. This calculation also bounds disk staging.
func expectedTarBytes(entries []archiveEntry, inventoryBytes int64, maximumBytes int64) (int64, error) {
	total := int64(2 * 512)
	sizes := make([]int64, 0, len(entries)+1)
	for _, entry := range entries {
		sizes = append(sizes, entry.Bytes)
	}
	sizes = append(sizes, inventoryBytes)
	for _, size := range sizes {
		if size < 0 || size > maximumBytes {
			return 0, errors.New("tar member exceeds its byte bound")
		}
		padded := ((size + 511) / 512) * 512
		if padded > maximumBytes-512 || total > maximumBytes-512-padded {
			return 0, errors.New("tar would exceed archive bound")
		}
		total += 512 + padded
	}
	return total, nil
}

// Metadata is small, canonical JSON, separate from opaque payload contents.
func encodeMetadata(value any, settings archiveSettings) ([]byte, error) {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil || int64(len(content)) > settings.MaximumMetadataBytes {
		return nil, errors.New("archive metadata exceeds its bound")
	}
	return content, nil
}

// Preparation reserves room for the new tar; verification does not double-count
// an existing tar. Both reserve additional metadata and explicit staging space.
func checkHeadroom(directory string, stagingBytes int64, newTarBytes int64, settings archiveSettings, hooks archiveHooks) error {
	if stagingBytes < 0 || stagingBytes > settings.MaximumStagingBytes ||
		newTarBytes < 0 || newTarBytes > settings.MaximumArchiveBytes {
		return errors.New("staging reservation exceeds its bound")
	}
	required := settings.MinimumFreeBytes
	for _, amount := range []int64{stagingBytes, newTarBytes, settings.MaximumMetadataBytes, settings.MaximumMetadataBytes} {
		if amount > int64(^uint64(0)>>1)-required {
			return errors.New("disk reservation overflow")
		}
		required += amount
	}
	available, err := hooks.AvailableBytes(directory)
	if err != nil || available < required {
		return errors.New("insufficient disk headroom; preserve all existing evidence")
	}
	return nil
}

// Loads independently pinned private metadata and its complete source inventory.
func loadPlan(pin filePin, policy archivePolicy, settings archiveSettings, hooks archiveHooks) (*archivePlan, *archiveInventory, []byte, error) {
	content, err := readPinned(pin, policy, settings)
	if err != nil {
		return nil, nil, nil, err
	}
	var plan archivePlan
	if err := decodeMetadata(content, &plan, settings); err != nil {
		return nil, nil, nil, err
	}
	directory, err := workDirectory(policy, plan.StorageNonce)
	if err != nil {
		return nil, nil, nil, err
	}
	unit, err := sourceUnit(plan.SourceRoot, policy)
	if err != nil {
		return nil, nil, nil, err
	}
	if plan.Schema != 1 || plan.Kind != archivePlanKind || plan.Context != policy.Context ||
		plan.PublicPlanSha256 != policy.PublicPlanSha256 || plan.Unit != unit ||
		pin.Path != filepath.Join(directory, "archive-plan.json") ||
		plan.Inventory.Path != filepath.Join(directory, "inventory.json") ||
		plan.ArchivePath != filepath.Join(directory, "full-evidence.tar") ||
		plan.Owner != policy.Owner || !plan.RetainUntil.Equal(policy.RetainUntil) ||
		!hooks.Now().UTC().Before(plan.RetainUntil) || !validDigest(plan.ArchiveSha256) ||
		plan.ArchiveBytes <= 0 || plan.ArchiveBytes > settings.MaximumArchiveBytes {
		return nil, nil, nil, errors.New("archive plan changed context, source, paths, retention, or bounds")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, nil, nil, errors.New("private archive directory required")
	}
	prefix := policy.KeyPrefix + "/" + plan.Unit + "/" + plan.StorageNonce + "/"
	if plan.Key != prefix+plan.ArchiveSha256+".tar" {
		return nil, nil, nil, errors.New("archive key is outside its new content-addressed namespace")
	}
	inventoryBytes, err := readPinned(plan.Inventory, policy, settings)
	if err != nil {
		return nil, nil, nil, err
	}
	var inventory archiveInventory
	if err := decodeMetadata(inventoryBytes, &inventory, settings); err != nil {
		return nil, nil, nil, err
	}
	if inventory.Schema != 1 || inventory.Kind != archiveInventoryKind || inventory.Context != plan.Context ||
		inventory.PublicPlanSha256 != plan.PublicPlanSha256 || inventory.SourceRoot != plan.SourceRoot ||
		inventory.Unit != plan.Unit || !inventory.ExcludesItself ||
		len(inventory.Entries) == 0 || len(inventory.Entries) > settings.MaximumEntries {
		return nil, nil, nil, errors.New("pinned inventory context or count changed")
	}
	previous := ""
	for _, entry := range inventory.Entries {
		if !validMember(entry.Name) || entry.Name == inventoryMemberName || entry.Name <= previous ||
			entry.Bytes < 0 || !validDigest(entry.Sha256) {
			return nil, nil, nil, errors.New("inventory duplicate, traversal, digest, or ordering refused")
		}
		previous = entry.Name
	}
	size, err := expectedTarBytes(inventory.Entries, int64(len(inventoryBytes)), settings.MaximumArchiveBytes)
	if err != nil || size != plan.ArchiveBytes {
		return nil, nil, nil, errors.New("archive byte count differs from its pinned inventory")
	}
	if err := checkHeadroom(directory, plan.StagingReserveBytes, 0, settings, hooks); err != nil {
		return nil, nil, nil, err
	}
	return &plan, &inventory, inventoryBytes, nil
}

// Verifies every original source and exact tar member, including the embedded
// inventory. No missing member, hidden format extension, or trailing payload is accepted.
func verifyLocal(ctx context.Context, pin filePin, policy archivePolicy, settings archiveSettings, hooks archiveHooks) (*archivePlan, *archiveInventory, error) {
	plan, inventory, inventoryBytes, err := loadPlan(pin, policy, settings, hooks)
	if err != nil {
		return nil, nil, err
	}
	actual, err := censusSource(ctx, plan.SourceRoot, policy, settings)
	if err != nil || !reflect.DeepEqual(actual, inventory.Entries) {
		return nil, nil, errors.New("source coverage, size, or content differs from pinned inventory")
	}
	file, err := openProtected(plan.ArchivePath, policy, true)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || before.Size() != plan.ArchiveBytes {
		return nil, nil, errors.New("existing archive size mismatch")
	}
	digest := sha256.New()
	limited := &io.LimitedReader{R: contextReader{Context: ctx, Reader: file}, N: plan.ArchiveBytes + 1}
	stream := io.TeeReader(limited, digest)
	reader := tar.NewReader(stream)
	expected := map[string]archiveEntry{}
	for _, entry := range inventory.Entries {
		expected[entry.Name] = entry
	}
	expected[inventoryMemberName] = archiveEntry{Name: inventoryMemberName, Bytes: int64(len(inventoryBytes)), Sha256: plan.Inventory.Sha256}
	buffer := make([]byte, settings.BufferBytes)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, errors.New("archive structure refused")
		}
		entry, exists := expected[header.Name]
		if !exists || !validMember(header.Name) || header.Typeflag != tar.TypeReg ||
			header.Size != entry.Bytes || header.Linkname != "" || header.Format != tar.FormatUSTAR ||
			header.Mode != 0400 || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" ||
			!header.ModTime.Equal(time.Unix(0, 0)) || header.Devmajor != 0 || header.Devminor != 0 ||
			len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
			return nil, nil, errors.New("archive linked, duplicate, unlisted, extended, or changed member refused")
		}
		memberDigest := sha256.New()
		count, err := io.CopyBuffer(memberDigest, reader, buffer)
		if err != nil || count != entry.Bytes || hex.EncodeToString(memberDigest.Sum(nil)) != entry.Sha256 {
			return nil, nil, errors.New("archive member hash or length mismatch")
		}
		delete(expected, header.Name)
	}
	if len(expected) != 0 {
		return nil, nil, errors.New("archive members missing")
	}
	for {
		count, err := stream.Read(buffer)
		for _, value := range buffer[:count] {
			if value != 0 {
				return nil, nil, errors.New("trailing archive payload refused")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, errors.New("archive tail read interrupted")
		}
	}
	if plan.ArchiveBytes+1-limited.N != plan.ArchiveBytes || hex.EncodeToString(digest.Sum(nil)) != plan.ArchiveSha256 {
		return nil, nil, errors.New("complete archive hash or length mismatch")
	}
	if err := recheckFile(file, before); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, errors.New("local verification owner canceled")
	}
	return plan, inventory, nil
}

// A fresh protected directory permanently spends preparation, even on failure.
func prepareArchive(ctx context.Context, request prepareRequest, policy archivePolicy, settings archiveSettings, hooks archiveHooks) (*archiveReport, error) {
	unit, err := sourceUnit(request.SourceRoot, policy)
	if err != nil {
		return nil, err
	}
	directory, err := workDirectory(policy, request.StorageNonce)
	if err != nil {
		return nil, err
	}
	if err := checkPath(directory, policy, true); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("preparation namespace exists; no automatic retry or overwrite")
	}
	if directory == request.SourceRoot || strings.HasPrefix(directory, request.SourceRoot+string(filepath.Separator)) {
		return nil, errors.New("archive output cannot be nested in its source")
	}
	entries, err := censusSource(ctx, request.SourceRoot, policy, settings)
	if err != nil {
		return nil, err
	}
	inventory := archiveInventory{Schema: 1, Kind: archiveInventoryKind, Context: policy.Context,
		PublicPlanSha256: policy.PublicPlanSha256, SourceRoot: request.SourceRoot, Unit: unit,
		ExcludesItself: true, Entries: entries}
	inventoryBytes, err := encodeMetadata(inventory, settings)
	if err != nil {
		return nil, err
	}
	tarBytes, err := expectedTarBytes(entries, int64(len(inventoryBytes)), settings.MaximumArchiveBytes)
	if err != nil {
		return nil, err
	}
	if err := checkHeadroom(policy.WorkParent, request.StagingReserveBytes, tarBytes, settings, hooks); err != nil {
		return nil, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, errors.New("exclusive preparation namespace creation failed")
	}
	if err := syncDirectory(policy.WorkParent); err != nil {
		return nil, err
	}
	started, err := encodeMetadata(map[string]any{"schema": 1, "kind": "first-committed-archive-preparation",
		"context": policy.Context, "source_root": request.SourceRoot, "started_at": hooks.Now().UTC(),
		"no_network": true, "automatic_retry": false, "calibration_acceptance_claimed": false}, settings)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(directory, "prepare.started.json"), started, policy, settings); err != nil {
		return nil, err
	}
	if err := hooks.AfterStep("prepare-claimed"); err != nil {
		return nil, err
	}
	inventoryPath := filepath.Join(directory, "inventory.json")
	if err := writeExclusive(inventoryPath, inventoryBytes, policy, settings); err != nil {
		return nil, err
	}
	archivePath := filepath.Join(directory, "full-evidence.tar")
	archiveSha256, err := createTar(ctx, archivePath, request.SourceRoot, entries, inventoryBytes, policy, settings)
	if err != nil {
		return nil, err
	}
	plan := archivePlan{Schema: 1, Kind: archivePlanKind, Context: policy.Context, PublicPlanSha256: policy.PublicPlanSha256,
		SourceRoot: request.SourceRoot, Unit: unit, Inventory: filePin{Path: inventoryPath, Sha256: digestBytes(inventoryBytes)},
		ArchivePath: archivePath, ArchiveBytes: tarBytes, ArchiveSha256: archiveSha256, StorageNonce: request.StorageNonce,
		Key:   policy.KeyPrefix + "/" + unit + "/" + request.StorageNonce + "/" + archiveSha256 + ".tar",
		Owner: policy.Owner, RetainUntil: policy.RetainUntil, StagingReserveBytes: request.StagingReserveBytes}
	planBytes, err := encodeMetadata(plan, settings)
	if err != nil {
		return nil, err
	}
	pin := filePin{Path: filepath.Join(directory, "archive-plan.json"), Sha256: digestBytes(planBytes)}
	if err := writeExclusive(pin.Path, planBytes, policy, settings); err != nil {
		return nil, err
	}
	if _, _, err := verifyLocal(ctx, pin, policy, settings, hooks); err != nil {
		return nil, err
	}
	if err := hooks.AfterStep("prepared"); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("preparation owner canceled; retain partial namespace")
	}
	report := reportArchive("prepared-private-only", pin, plan, inventory)
	content, err := encodeMetadata(report, settings)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(directory, "prepare.complete.json"), content, policy, settings); err != nil {
		return nil, err
	}
	return report, nil
}

// The only producer is the explicitly requested private preparation mode.
// Payload modes are normalized inside the tar; original files are never changed.
func createTar(ctx context.Context, destination, sourceRoot string, entries []archiveEntry, inventoryBytes []byte, policy archivePolicy, settings archiveSettings) (string, error) {
	if err := checkPath(destination, policy, true); err != nil {
		return "", err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("exclusive tar creation failed; retain partial namespace")
	}
	defer func() { _ = file.Sync(); _ = file.Close() }()
	digest := sha256.New()
	writer := tar.NewWriter(io.MultiWriter(file, digest))
	members := append(append([]archiveEntry{}, entries...), archiveEntry{Name: inventoryMemberName, Bytes: int64(len(inventoryBytes)), Sha256: digestBytes(inventoryBytes)})
	for _, entry := range members {
		if err := ctx.Err(); err != nil {
			return "", errors.New("tar preparation canceled; retain partial namespace")
		}
		if err := writer.WriteHeader(&tar.Header{Name: entry.Name, Size: entry.Bytes, Mode: 0400,
			ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return "", errors.New("source name cannot be represented in bounded ustar")
		}
		if entry.Name == inventoryMemberName {
			if _, err := io.Copy(writer, bytes.NewReader(inventoryBytes)); err != nil {
				return "", errors.New("inventory tar write failed")
			}
			continue
		}
		source, err := openProtected(filepath.Join(sourceRoot, filepath.FromSlash(entry.Name)), policy, false)
		if err != nil {
			return "", err
		}
		before, statErr := source.Stat()
		sourceDigest := sha256.New()
		count, copyErr := io.CopyBuffer(io.MultiWriter(writer, sourceDigest),
			io.LimitReader(contextReader{Context: ctx, Reader: source}, entry.Bytes+1), make([]byte, settings.BufferBytes))
		if statErr == nil {
			statErr = recheckFile(source, before)
		}
		closeErr := source.Close()
		if statErr != nil || copyErr != nil || closeErr != nil || count != entry.Bytes ||
			hex.EncodeToString(sourceDigest.Sum(nil)) != entry.Sha256 {
			return "", errors.New("source changed during tar preparation; retain partial namespace")
		}
	}
	if err := writer.Close(); err != nil {
		return "", errors.New("tar completion failed; retain partial namespace")
	}
	if err := errors.Join(file.Chmod(0400), file.Sync(), file.Close()); err != nil {
		return "", errors.New("tar durability failed; retain partial namespace")
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// A report never turns file retention into statistical or launch acceptance.
func reportArchive(mode string, pin filePin, plan archivePlan, inventory archiveInventory) *archiveReport {
	return &archiveReport{Mode: mode, Plan: pin, InventorySha256: plan.Inventory.Sha256,
		ArchiveSha256: plan.ArchiveSha256, ArchiveBytes: plan.ArchiveBytes,
		PayloadFiles: len(inventory.Entries), ArchiveMembers: len(inventory.Entries) + 1,
		AcceptanceClaimed: false}
}

// Prefixes are derived only from the frozen policy and exact plan identities.
func objectPrefix(plan archivePlan, policy archivePolicy) string {
	return path.Join(policy.KeyPrefix, plan.Unit, plan.StorageNonce) + "/"
}
