// Upload is a separately authorized one-use action; every ambiguous or partial
// attempt retains its claim and requires manual inspection rather than retry.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Preparation and verification must acquire the same canonical slot as upload
// before any source scan. Tests supply an instance-local synthetic slot.
func executeArchive(ctx context.Context, request archiveRequest, preparation *prepareRequest, policy archivePolicy, settings archiveSettings, hooks archiveHooks) (*archiveReport, error) {
	if err := validateSettings(settings); err != nil {
		return nil, err
	}
	if hooks.Now == nil || hooks.AvailableBytes == nil || hooks.AcquireSlot == nil ||
		hooks.NewTransport == nil || hooks.AfterStep == nil {
		return nil, errors.New("complete archive observation hooks required")
	}
	if preparation != nil && (request.Upload || request.PlanPin.Path != "" || request.PlanPin.Sha256 != "" || request.UploadAuthorizationSha256 != "") {
		return nil, errors.New("prepare cannot verify or upload an existing plan")
	}
	if request.Upload && (!validDigest(request.PlanPin.Sha256) || request.UploadAuthorizationSha256 != request.PlanPin.Sha256) {
		return nil, errors.New("upload requires separate exact-plan authorization")
	}
	if !request.Upload && request.UploadAuthorizationSha256 != "" {
		return nil, errors.New("upload authorization cannot silently change verification mode")
	}
	slot, err := hooks.AcquireSlot()
	if err != nil || slot == nil {
		if slot != nil {
			slot.Close()
		}
		return nil, errors.New("canonical scoring slot active or unavailable")
	}
	defer slot.Close()
	verificationCtx, cancel := context.WithTimeout(ctx, settings.VerificationTimeout)
	defer cancel()
	if preparation != nil {
		return prepareArchive(verificationCtx, *preparation, policy, settings, hooks)
	}
	plan, inventory, err := verifyLocal(verificationCtx, request.PlanPin, policy, settings, hooks)
	if err != nil {
		return nil, err
	}
	if !request.Upload {
		return reportArchive("verified-local-only", request.PlanPin, *plan, *inventory), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("upload canceled before claim")
	}
	directory := filepath.Dir(request.PlanPin.Path)
	for _, name := range []string{"upload.started.json", "protection.private.json", "retention.private.json", "receipt.private.json"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("upload spent or partially recorded; no automatic retry")
		}
	}
	claim, err := encodeMetadata(map[string]any{"schema": 1, "kind": "first-committed-retained-archive-one-use-upload",
		"context": policy.Context, "public_plan_sha256": policy.PublicPlanSha256, "plan_sha256": request.PlanPin.Sha256,
		"inventory_sha256": plan.Inventory.Sha256, "archive_sha256": plan.ArchiveSha256, "key": plan.Key,
		"started_at": hooks.Now().UTC(), "maximum_operator_put_retained_calls": 1,
		"automatic_operator_retry": false, "sdk_http_retry_count_claimed": false}, settings)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(directory, "upload.started.json"), claim, policy, settings); err != nil {
		return nil, err
	}
	if err := hooks.AfterStep("upload-claimed"); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("upload canceled after durable claim; hold")
	}
	// A client is constructed only after the permanent claim is durable.
	store, err := hooks.NewTransport(policy)
	if err != nil || store == nil {
		return nil, errors.New("native protected transport unavailable; hold spent claim")
	}
	uploadCtx, uploadCancel := context.WithTimeout(ctx, settings.UploadTimeout)
	defer uploadCancel()
	protection, err := store.CheckProtection(uploadCtx)
	if err != nil || protection.Authority == "" || protection.Bucket != policy.Bucket ||
		!protection.Versioning || protection.ObjectLock != "COMPLIANCE" || len(protection.ReplicationTargets) == 0 {
		return nil, errors.New("protected versioning/compliance/replication proof unavailable; hold")
	}
	content, err := encodeMetadata(protection, settings)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(directory, "protection.private.json"), content, policy, settings); err != nil {
		return nil, err
	}
	occupied := false
	err = store.ListVersions(uploadCtx, objectPrefix(*plan, policy), func() error {
		occupied = true
		return errors.New("occupied storage prefix")
	})
	if err != nil || occupied {
		return nil, errors.New("storage prefix is occupied or history is uncertain; hold")
	}
	// Recheck private metadata immediately before the only write request.
	if _, err := readPinned(request.PlanPin, policy, settings); err != nil {
		return nil, err
	}
	if _, err := readPinned(plan.Inventory, policy, settings); err != nil {
		return nil, err
	}
	if _, _, err := verifyLocal(uploadCtx, request.PlanPin, policy, settings, hooks); err != nil {
		return nil, err
	}
	if err := uploadCtx.Err(); err != nil {
		return nil, errors.New("upload canceled before retained write; hold")
	}
	retention, err := store.PutRetained(uploadCtx, plan.Key, plan.ArchivePath, plan.RetainUntil)
	if err != nil {
		return nil, errors.New("retained write ambiguous or failed; hold, never automatically rerun")
	}
	content, err = encodeMetadata(retention, settings)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(directory, "retention.private.json"), content, policy, settings); err != nil {
		return nil, err
	}
	if retention.Key != plan.Key || retention.Size != plan.ArchiveBytes ||
		retention.VersionId == "" || retention.VersionId == "null" ||
		strings.ContainsAny(retention.VersionId, "\x00\r\n") || retention.Mode != "COMPLIANCE" ||
		!retention.RetainUntil.Equal(plan.RetainUntil) {
		return nil, errors.New("returned retained source-version identity mismatch; hold")
	}
	reader, err := store.GetVersion(uploadCtx, plan.Key, retention.VersionId)
	if err != nil || reader == nil {
		if reader != nil {
			reader.Close()
		}
		return nil, errors.New("exact retained version readback unavailable; hold")
	}
	digest := sha256.New()
	count, readErr := io.CopyBuffer(digest,
		io.LimitReader(contextReader{Context: uploadCtx, Reader: reader}, plan.ArchiveBytes+1), make([]byte, settings.BufferBytes))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || uploadCtx.Err() != nil || count != plan.ArchiveBytes ||
		hex.EncodeToString(digest.Sum(nil)) != plan.ArchiveSha256 {
		return nil, errors.New("exact source-version content readback mismatch; hold")
	}
	if err := hooks.AfterStep("source-version-read-back"); err != nil {
		return nil, err
	}
	if err := uploadCtx.Err(); err != nil {
		return nil, errors.New("receipt owner canceled; retain partial evidence and hold")
	}
	receipt, err := encodeMetadata(map[string]any{"schema": 1, "kind": "first-committed-retained-archive-receipt",
		"context": policy.Context, "public_plan_sha256": policy.PublicPlanSha256, "owner": policy.Owner,
		"completed_at": hooks.Now().UTC(), "plan_sha256": request.PlanPin.Sha256,
		"inventory_sha256": plan.Inventory.Sha256, "source_root": plan.SourceRoot,
		"archive_sha256": plan.ArchiveSha256, "archive_bytes": plan.ArchiveBytes,
		"payload_files": len(inventory.Entries), "archive_members": len(inventory.Entries) + 1,
		"protection": protection, "retention": retention,
		"all_source_and_member_hashes_verified_before_upload": true, "exact_version_sha256_readback_equal": true,
		"operator_put_retained_calls": 1, "sdk_http_retry_count_claimed": false,
		"atomic_retained_if_absent_claimed": false, "replica_delivery_readback_verified": false,
		"offsite_backup_claimed": false, "source_deletions": 0, "configuration_changes": 0,
		"calibration_or_launch_acceptance_claimed": false}, settings)
	if err != nil {
		return nil, err
	}
	receiptPath := filepath.Join(directory, "receipt.private.json")
	if err := writeExclusive(receiptPath, receipt, policy, settings); err != nil {
		return nil, err
	}
	report := reportArchive("retained-source-version-verified", request.PlanPin, *plan, *inventory)
	report.ReceiptPath = receiptPath
	return report, nil
}
