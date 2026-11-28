package usagearchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The tied contract ids after < contract_id <= upper, read from each
// payment's range of the sweep index, merged; and how many each payment paid.
func readReviewRangeIds(ctx context.Context, q reviewQuerier, paymentIds []string, after string, upper string) (contractIds []string, paymentContracts map[string]int64, returnErr error) {
	merged := map[string]bool{}
	paymentContracts = map[string]int64{}
	for _, paymentId := range paymentIds {
		var ids []string
		if err := q.QueryRow(ctx, `
			SELECT ARRAY(
				SELECT DISTINCT contract_id FROM transfer_escrow_sweep
				WHERE payment_id = $1::uuid AND $2::uuid < contract_id AND contract_id <= $3::uuid
				ORDER BY contract_id
			)::text[]
		`, paymentId, after, upper).Scan(&ids); err != nil {
			return nil, nil, err
		}
		if 0 < len(ids) {
			paymentContracts[paymentId] = int64(len(ids))
		}
		for _, id := range ids {
			merged[id] = true
		}
	}
	for id := range merged {
		contractIds = append(contractIds, id)
	}
	sort.Strings(contractIds)
	return contractIds, paymentContracts, nil
}

// The payments' tied contracts after after, one of each payment's at most.
func readReviewIdsAfter(ctx context.Context, q reviewQuerier, paymentIds []string, after string) ([]string, error) {
	found := []string{}
	for _, paymentId := range paymentIds {
		var contractId string
		err := q.QueryRow(ctx, `
			SELECT contract_id::text FROM transfer_escrow_sweep
			WHERE payment_id = $1::uuid AND $2::uuid < contract_id
			ORDER BY contract_id
			LIMIT 1
		`, paymentId, after).Scan(&contractId)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found = append(found, contractId)
	}
	return found, nil
}

// One read only transaction with the review's session settings.
func reviewReadTx(ctx context.Context, conn *pgx.Conn, settings *ReviewSettings, isoLevel pgx.TxIsoLevel, read func(tx pgx.Tx) error) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: isoLevel, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := execAll(ctx, tx, reviewSessionSql(settings)); err != nil {
		return err
	}
	if err := read(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// The coverage of the run's tied contracts on Main now: every batch's tied
// contracts, counted again from the sweep index, are its manifest's, by count,
// digest and payment, and no tied contract follows the last batch. Returns
// each difference.
func checkReviewCoverage(ctx context.Context, conn *pgx.Conn, run *ReviewRun, batches []*ReviewBatch, settings *ReviewSettings) ([]string, error) {
	failures := []string{}
	paymentIds := run.PaymentIds()
	for _, batch := range batches {
		err := reviewReadTx(ctx, conn, settings, pgx.ReadCommitted, func(tx pgx.Tx) error {
			contractIds, paymentContracts, err := readReviewRangeIds(ctx, tx, paymentIds, batch.AfterContractId, batch.UpperContractId)
			if err != nil {
				return err
			}
			if int64(len(contractIds)) != batch.Contracts || reviewIdDigest(contractIds) != batch.ContractIdDigest {
				failures = append(failures, fmt.Sprintf("batch %d: Main holds %d tied contracts in (%s, %s] with digest %s, the batch %d with %s",
					batch.Batch, len(contractIds), batch.AfterContractId, batch.UpperContractId, reviewIdDigest(contractIds), batch.Contracts, batch.ContractIdDigest))
			}
			for _, paymentId := range paymentIds {
				if paymentContracts[paymentId] != batch.PaymentContracts[paymentId] {
					failures = append(failures, fmt.Sprintf("batch %d: payment %s paid %d of its contracts on Main, %d in the batch",
						batch.Batch, paymentId, paymentContracts[paymentId], batch.PaymentContracts[paymentId]))
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	after := zeroContractId
	if 0 < len(batches) {
		after = batches[len(batches)-1].UpperContractId
	}
	err := reviewReadTx(ctx, conn, settings, pgx.ReadCommitted, func(tx pgx.Tx) error {
		beyond, err := readReviewIdsAfter(ctx, tx, paymentIds, after)
		if err != nil {
			return err
		}
		if 0 < len(beyond) {
			failures = append(failures, fmt.Sprintf("tied contracts follow the last batch's %s, such as %s: the export is not complete", after, strings.Join(beyond, ", ")))
		}
		return nil
	})
	return failures, err
}

// Reads every batch's rows on Main again and matches each table's row count
// and digest with the batch's. Returns each difference.
func checkReviewRows(ctx context.Context, conn *pgx.Conn, run *ReviewRun, batches []*ReviewBatch, settings *ReviewSettings) ([]string, error) {
	failures := []string{}
	for _, batch := range batches {
		err := reviewReadTx(ctx, conn, settings, pgx.RepeatableRead, func(tx pgx.Tx) error {
			if err := execAll(ctx, tx, reviewBitmapSql(settings)); err != nil {
				return err
			}
			idsCte := reviewIdsCteSql(run.PaymentIds(), batch.AfterContractId, batch.UpperContractId)
			for i, table := range run.Tables {
				var rows int64
				var digest string
				if err := tx.QueryRow(ctx, reviewStatsSql(table, idsCte)).Scan(&rows, &digest); err != nil {
					return err
				}
				if copied := batch.Tables[i]; rows != copied.Rows || digest != copied.RowDigest {
					failures = append(failures, fmt.Sprintf("batch %d: Main holds %d rows of %s with digest %s, the copy %d with %s",
						batch.Batch, rows, table.Table, digest, copied.Rows, copied.RowDigest))
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return failures, nil
}

// Verifies the run in dir and writes the verification beside it, whether it
// passed or not: every stored copy read back from storage and matched; unless
// filesOnly, the coverage on Main (checkReviewCoverage); with mainRows, every
// batch's rows read on Main again and matched. A directory whose batches do
// not tile the id space, or that holds the files of an unfinished batch,
// fails. Returns the verification and its path.
func VerifyReview(ctx context.Context, conn *pgx.Conn, dir string, host string, filesOnly bool, mainRows bool, settings *ReviewSettings) (*ReviewVerification, string, error) {
	run, err := ReadReviewRun(dir)
	if err != nil {
		return nil, "", err
	}
	batches, unfinished, err := reviewBatchState(dir, run)
	if err != nil {
		return nil, "", err
	}
	verification := &ReviewVerification{
		Format:           ReviewVerificationFormat,
		RunId:            run.RunId,
		VerifyTime:       settings.now().Format(time.RFC3339Nano),
		Host:             host,
		Batches:          int64(len(batches)),
		PaymentContracts: map[string]int64{},
		TableRows:        map[string]int64{},
		Failures:         []string{},
	}
	if verification.ManifestsSha256, err = reviewManifestsSha256(dir, batches); err != nil {
		return nil, "", err
	}
	if 0 < len(unfinished) {
		verification.Failures = append(verification.Failures, fmt.Sprintf("the files of an unfinished batch are present: %v", unfinished))
	}
	if len(batches) == 0 {
		verification.Failures = append(verification.Failures, "the run holds no batch")
	}
	for _, batch := range batches {
		verification.Contracts += batch.Contracts
		for paymentId, contracts := range batch.PaymentContracts {
			verification.PaymentContracts[paymentId] += contracts
		}
		for i, copied := range batch.Tables {
			verification.TableRows[copied.Table] += copied.Rows
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			if err := verifyReviewCopy(dir, run.Tables[i], batch, copied); err != nil {
				verification.Failures = append(verification.Failures, fmt.Sprintf("batch %d: %s", batch.Batch, err))
			}
		}
	}
	verification.FilesChecked = len(verification.Failures) == 0
	if !filesOnly {
		failures, err := checkReviewCoverage(ctx, conn, run, batches, settings)
		if err != nil {
			return nil, "", err
		}
		verification.Failures = append(verification.Failures, failures...)
		verification.MainChecked = len(failures) == 0 && 0 < len(batches)
		if mainRows {
			failures, err := checkReviewRows(ctx, conn, run, batches, settings)
			if err != nil {
				return nil, "", err
			}
			verification.Failures = append(verification.Failures, failures...)
			verification.MainRowsChecked = len(failures) == 0 && 0 < len(batches)
		}
	}
	verification.Ok = len(verification.Failures) == 0 && verification.FilesChecked && (filesOnly || verification.MainChecked)
	path := filepath.Join(dir, "verify-"+settings.now().Format("20060102T150405.000000000Z")+".json")
	if err := writeReviewJson(path, verification); err != nil {
		return nil, "", err
	}
	return verification, path, nil
}

// The newest verification in dir and its path, or nil when there is none.
func latestReviewVerification(dir string) (*ReviewVerification, string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, reviewVerificationGlob))
	if err != nil {
		return nil, "", err
	}
	if len(paths) == 0 {
		return nil, "", nil
	}
	sort.Strings(paths)
	path := paths[len(paths)-1]
	verification := &ReviewVerification{}
	if err := readReviewJson(path, verification); err != nil {
		return nil, "", err
	}
	if verification.Format != ReviewVerificationFormat {
		return nil, "", fmt.Errorf("%s: format %q is not %q", path, verification.Format, ReviewVerificationFormat)
	}
	return verification, path, nil
}

// The verification as one line of JSON, for logs.
func (self *ReviewVerification) Json() string {
	data, err := json.Marshal(self)
	if err != nil {
		return fmt.Sprintf("%+v", *self)
	}
	return string(data)
}
