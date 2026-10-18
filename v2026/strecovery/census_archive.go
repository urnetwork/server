// Archives and restoration have no database or chain write port. A reviewed
// census seal authorizes only local custody publication of original EVM bytes.
package strecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/urfoundation/sn/v2026/protocol"
)

// A strict local config rejects unknown fields, repeated/case-folded keys and
// trailing JSON before any database connection can be selected.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	raw, err := readPrivateFile(ctx, path, 64*1024)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := decodeJson(raw, &config); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

// Loading is entirely offline, including complete replay of the union and fee
// envelopes against the archived source images.
func LoadArchive(ctx context.Context, path string) (*Archive, error) {
	raw, err := readPrivateFile(ctx, path, MaximumArchiveBytes)
	if err != nil {
		return nil, err
	}
	var archive Archive
	if err := decodeJson(raw, &archive); err != nil {
		return nil, err
	}
	if err := archive.Validate(ctx); err != nil {
		return nil, err
	}
	return &archive, nil
}

// Existing identical archive bytes are an idempotent restart. A different
// census never replaces an earlier evidence artifact at the same path.
func WriteArchive(ctx context.Context, path string, archive *Archive) (resultErr error) {
	if err := archive.Validate(ctx); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(archive, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > MaximumArchiveBytes {
		return errors.New("complete recovery archive exceeds its hard byte bound")
	}
	if !absolutePath(path) {
		return errors.New("recovery archive path must be absolute and canonical")
	}
	directory, err := openPrivatePath(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	unlock, err := lockDirectory(directory)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = publishFile(ctx, directory, filepath.Base(path), raw)
	return err
}

// The result describes only local file publication. It carries no claim about
// receipts, database reconciliation, actual fees or permission to rebroadcast.
type RestoreResult struct {
	CensusHash     string   `json:"census_hash"`
	Created        []string `json:"created"`
	AlreadyPresent []string `json:"already_present"`
}

// All existing destination files are read before the first publication. Known
// conflicts refuse the whole write phase. An interrupted prefix is safe to
// resume: each signature is atomically create-only and checked byte for byte.
func Restore(ctx context.Context, archive *Archive, directoryPath, acceptedHash string) (*RestoreResult, error) {
	return restore(ctx, archive, directoryPath, acceptedHash, nil)
}

// The test hook forces interruption after a durable publication, without sleeps
// or scheduler assumptions. It is absent from the public command surface.
func restore(ctx context.Context, archive *Archive, directoryPath, acceptedHash string, afterCreate func(string) error) (result *RestoreResult, resultErr error) {
	if err := archive.Validate(ctx); err != nil {
		return nil, err
	}
	if acceptedHash != archive.CensusHash {
		return nil, errors.New("restoration requires the independently reviewed census hash")
	}
	directory, err := openPrivatePath(directoryPath, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := directory.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	unlock, err := lockDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// The destination descriptor stays pinned throughout preflight and publication.
	names, err := directoryNames(directory, archive.Selection.Limits.MaximumAttempts)
	if err != nil {
		return nil, err
	}
	targetByName := map[string][]byte{}
	for _, tx := range archive.Transactions {
		targetByName[strings.TrimPrefix(tx.Hash, "0x")+".rlp"] = tx.Raw
	}
	existing := map[string]bool{}
	totalBytes := 0
	for _, name := range names {
		if kind, _ := storeFilename(name); kind == "" {
			return nil, &Refusal{Source: "restore", Record: name, Cause: "destination contains unclassified evidence; existing bytes were preserved"}
		}
		file, err := openPrivateChild(directory, name)
		if err != nil {
			return nil, &Refusal{Source: "restore", Record: name, Cause: "destination file is not private physical custody"}
		}
		raw, readErr := readOpened(ctx, file, archive.Selection.Limits.MaximumTransactionBytes)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return nil, err
		}
		totalBytes += len(raw)
		if totalBytes > archive.Selection.Limits.MaximumTotalBytes {
			return nil, errors.New("destination census exceeds its explicit byte bound")
		}
		if expected := targetByName[name]; expected != nil && !bytes.Equal(raw, expected) {
			return nil, &Refusal{Source: "restore", Record: name, Cause: "destination conflicts with original signature; no census files were published"}
		}
		existing[name] = true
	}
	plannedFiles := len(names)
	for name, raw := range targetByName {
		if !existing[name] {
			plannedFiles++
			totalBytes += len(raw)
		}
	}
	if plannedFiles > archive.Selection.Limits.MaximumAttempts || totalBytes > archive.Selection.Limits.MaximumTotalBytes {
		return nil, errors.New("restored destination would exceed its explicit file or byte bounds")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result = &RestoreResult{CensusHash: archive.CensusHash, Created: []string{}, AlreadyPresent: []string{}}
	for _, tx := range archive.Transactions {
		name := strings.TrimPrefix(tx.Hash, "0x") + ".rlp"
		created, err := publishFile(ctx, directory, name, tx.Raw)
		if err != nil {
			return result, errors.Join(&Refusal{Source: "restore", Record: name, Cause: "create-only publication failed; retain evidence and resume the same census"}, ctx.Err())
		}
		if created {
			result.Created = append(result.Created, tx.Hash)
			if afterCreate != nil {
				if err := afterCreate(tx.Hash); err != nil {
					return result, err
				}
			}
		} else {
			result.AlreadyPresent = append(result.AlreadyPresent, tx.Hash)
		}
	}
	return result, ctx.Err()
}

// Accepted names have one exact lower-case transaction identity. Native hashes
// are syntax-checked only; their bytes remain uninterpreted retained evidence.
func storeFilename(name string) (kind, hash string) {
	for _, extension := range []string{".rlp", ".scale"} {
		if strings.HasSuffix(name, extension) {
			hash := strings.TrimSuffix(name, extension)
			if canonicalHex("0x"+hash, 32) {
				return strings.TrimPrefix(extension, "."), "0x" + hash
			}
		}
	}
	return "", ""
}

// The shared admission parser checks repeated keys before standard struct
// decoding, whose ordinary last-key-wins behavior is unsafe for source pins.
func decodeJson(raw []byte, value any) error {
	if err := protocol.ValidateUniqueJsonKeys(raw); err != nil {
		return errors.New("recovery JSON is ambiguous or malformed")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("recovery JSON has unknown fields or invalid values")
	}
	return nil
}
