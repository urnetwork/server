package usagearchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The change a clear makes, and the one its reversal makes.
const (
	reviewClearChange   = "attribution_review_required true -> false"
	reviewRestoreChange = "attribution_review_required false -> true"
)

// What a clear (or its reversal) will change, from reads only, and the digest
// a commit must name (--expect): any difference in the run, its batches, its
// verification or the payments' rows changes it.
type ReviewClearPlan struct {
	Change             string
	Run                *ReviewRun
	Batches            int64
	Contracts          int64
	PaymentContracts   map[string]int64
	ManifestsSha256    string
	VerificationFile   string
	VerificationSha256 string
	// the payments' rows now
	Payments []ReviewPayment
	Digest   string
}

func (self *ReviewClearPlan) digest() string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\nchange %s\nrun %s\nbatches %d\ncontracts %d\nmanifests %s\nverification %s %s\n",
		ReviewClearFormat, self.Change, self.Run.RunId, self.Batches, self.Contracts, self.ManifestsSha256, filepath.Base(self.VerificationFile), self.VerificationSha256)
	for _, payment := range self.Payments {
		fmt.Fprintf(hash, "payment %s %d %s\n", payment.PaymentId, self.PaymentContracts[payment.PaymentId], payment.Row)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Whether two rows of to_jsonb text are the same row, as PostgreSQL compares
// jsonb.
func sameReviewRow(ctx context.Context, q reviewQuerier, a string, b string) (bool, error) {
	var same bool
	err := q.QueryRow(ctx, `SELECT $1::jsonb = $2::jsonb`, a, b).Scan(&same)
	return same, err
}

// The run's batches and the newest verification, refused unless the export is
// complete and verified now: no file of an unfinished batch, a verification of
// these manifests no older than settings.MaxVerificationAge that read every
// stored copy back and checked the coverage on Main, every stored copy read
// back again now, and the coverage checked again now.
func requireReviewVerified(ctx context.Context, conn *pgx.Conn, dir string, run *ReviewRun, settings *ReviewSettings) (plan *ReviewClearPlan, batches []*ReviewBatch, returnErr error) {
	batches, unfinished, err := reviewBatchState(dir, run)
	if err != nil {
		return nil, nil, err
	}
	if 0 < len(unfinished) {
		return nil, nil, reviewRefusal("the files of an unfinished batch are present: %v; finish the export first", unfinished)
	}
	if len(batches) == 0 {
		return nil, nil, reviewRefusal("the run holds no batch")
	}
	plan = &ReviewClearPlan{Run: run, Batches: int64(len(batches)), PaymentContracts: map[string]int64{}}
	for _, batch := range batches {
		plan.Contracts += batch.Contracts
		for paymentId, contracts := range batch.PaymentContracts {
			plan.PaymentContracts[paymentId] += contracts
		}
	}
	if plan.ManifestsSha256, err = reviewManifestsSha256(dir, batches); err != nil {
		return nil, nil, err
	}
	verification, path, err := latestReviewVerification(dir)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case verification == nil:
		return nil, nil, reviewRefusal("the export has no verification; run review verify first")
	case !verification.Ok || !verification.FilesChecked || !verification.MainChecked:
		return nil, nil, reviewRefusal("the newest verification %s did not pass with its copies read back and the coverage checked on Main: %v", filepath.Base(path), verification.Failures)
	case verification.RunId != run.RunId || verification.ManifestsSha256 != plan.ManifestsSha256 || verification.Batches != plan.Batches || verification.Contracts != plan.Contracts:
		return nil, nil, reviewRefusal("the newest verification %s is of other batches than the run holds now; verify again", filepath.Base(path))
	}
	verifyTime, err := time.Parse(time.RFC3339Nano, verification.VerifyTime)
	if err != nil {
		return nil, nil, err
	}
	if age := settings.now().Sub(verifyTime); settings.MaxVerificationAge < age {
		return nil, nil, reviewRefusal("the newest verification %s is %s old, more than %s; verify again", filepath.Base(path), age.Round(time.Second), settings.MaxVerificationAge)
	}
	plan.VerificationFile = path
	if plan.VerificationSha256, err = fileSha256(path); err != nil {
		return nil, nil, err
	}
	for _, batch := range batches {
		for i, copied := range batch.Tables {
			if err := verifyReviewCopy(dir, run.Tables[i], batch, copied); err != nil {
				return nil, nil, reviewRefusal("batch %d: %s", batch.Batch, err)
			}
		}
	}
	failures, err := checkReviewCoverage(ctx, conn, run, batches, settings)
	if err != nil {
		return nil, nil, err
	}
	if 0 < len(failures) {
		return nil, nil, reviewRefusal("the export does not cover every tied contract now: %s", strings.Join(failures, "; "))
	}
	return plan, batches, nil
}

// Plans the clear of the attribution review flag of the run's payments, and
// with commit makes it. Reads only, the plan refuses unless the export is
// complete and verified now (requireReviewVerified) and each payment is the
// row the run recorded when it started, still completed under review. A
// commit refuses unless expect is the plan's digest. It then writes the rows
// as they are to dir, and in one transaction locks them, clears the flag of
// each, only where the row is still exactly that row, and journals the change
// (audit_account_payment, attribution_review_cleared). Any difference rolls
// back everything. Then it writes the rows as they are after.
func ClearReview(ctx context.Context, conn *pgx.Conn, dir string, host string, commit bool, expect string, settings *ReviewSettings) (*ReviewClearPlan, *ReviewClearRecord, error) {
	run, err := ReadReviewRun(dir)
	if err != nil {
		return nil, nil, err
	}
	plan, _, err := requireReviewVerified(ctx, conn, dir, run, settings)
	if err != nil {
		return nil, nil, err
	}
	plan.Change = reviewClearChange
	payments, err := readReviewPayments(ctx, conn, run.PaymentIds())
	if err != nil {
		return nil, nil, err
	}
	for _, recorded := range run.Payments {
		payment := payments[recorded.PaymentId]
		if blocker := reviewPaymentBlocker(recorded.PaymentId, payment); blocker != "" {
			return nil, nil, reviewRefusal("%s", blocker)
		}
		same, err := sameReviewRow(ctx, conn, payment.Row, recorded.Row)
		if err != nil {
			return nil, nil, err
		}
		if !same {
			return nil, nil, reviewRefusal("payment %s changed since the export started: it is %s, the run recorded %s", recorded.PaymentId, payment.Row, recorded.Row)
		}
		plan.Payments = append(plan.Payments, payment.ReviewPayment)
	}
	plan.Digest = plan.digest()
	if !commit {
		return plan, nil, nil
	}
	if expect != plan.Digest {
		return plan, nil, reviewRefusal("--expect %q is not the plan's digest %s: read the plan again", expect, plan.Digest)
	}
	record, err := commitReviewFlag(ctx, conn, dir, host, plan, plan.Payments, false, ReviewClearedEventType, settings)
	return plan, record, err
}

// Writes the rows as they are, then in one READ COMMITTED transaction locks
// the payments in id order, sets each one's flag to value only where the row
// is still exactly its row in rows, checks that the flag is all that changed,
// journals each change as eventType, and commits; then writes the rows as they
// are after. Nothing is written to Main unless every payment is as rows has
// it.
func commitReviewFlag(
	ctx context.Context,
	conn *pgx.Conn,
	dir string,
	host string,
	plan *ReviewClearPlan,
	rows []ReviewPayment,
	value bool,
	eventType string,
	settings *ReviewSettings,
) (*ReviewClearRecord, error) {
	stamp := settings.now()
	name := "clear"
	if value {
		name = "restore"
	}
	base := filepath.Join(dir, fmt.Sprintf("%s-%s", name, stamp.Format("20060102T150405.000000000Z")))
	record := &ReviewClearRecord{
		Format:             ReviewClearFormat,
		Stage:              "before",
		Change:             plan.Change,
		RunId:              plan.Run.RunId,
		Dir:                dir,
		Host:               host,
		Time:               stamp.Format(time.RFC3339Nano),
		PlanDigest:         plan.Digest,
		ManifestsSha256:    plan.ManifestsSha256,
		VerificationFile:   filepath.Base(plan.VerificationFile),
		VerificationSha256: plan.VerificationSha256,
		Contracts:          plan.Contracts,
		Payments:           rows,
	}
	// the rows as they are, before anything is written, so the change can be
	// reversed
	if err := writeReviewJson(base+"-before.json", record); err != nil {
		return nil, err
	}
	if settings.BeforeClearUpdate != nil {
		if err := settings.BeforeClearUpdate(); err != nil {
			return nil, err
		}
	}
	paymentIds := make([]string, 0, len(rows))
	for _, row := range rows {
		paymentIds = append(paymentIds, row.PaymentId)
	}
	after := &ReviewClearRecord{}
	*after = *record
	after.Stage = "after"
	after.Payments = []ReviewPayment{}
	after.EventIds = []string{}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := execAll(ctx, tx, []string{
		fmt.Sprintf(`SET LOCAL lock_timeout = %d`, settings.LockTimeout.Milliseconds()),
		fmt.Sprintf(`SET LOCAL statement_timeout = %d`, settings.StatementTimeout.Milliseconds()),
	}); err != nil {
		return nil, err
	}
	var locked int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT p.payment_id FROM account_payment AS p
			WHERE p.payment_id = ANY($1::text[]::uuid[])
			ORDER BY p.payment_id
			FOR UPDATE OF p
		) AS locked
	`, paymentIds).Scan(&locked); err != nil {
		return nil, err
	}
	if locked != len(rows) {
		return nil, reviewRefusal("locked %d of the %d payments; nothing was written", locked, len(rows))
	}
	for _, row := range rows {
		var updated string
		err := tx.QueryRow(ctx, `
			UPDATE account_payment AS p
			SET attribution_review_required = $3
			WHERE p.payment_id = $1::uuid AND to_jsonb(p) = $2::jsonb AND p.attribution_review_required = NOT $3
			RETURNING to_jsonb(p)::text
		`, row.PaymentId, row.Row, value).Scan(&updated)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, reviewRefusal("payment %s is no longer the row the plan read; nothing was written", row.PaymentId)
		}
		if err != nil {
			return nil, err
		}
		var onlyTheFlag bool
		if err := tx.QueryRow(ctx, `SELECT $1::jsonb = ($2::jsonb || jsonb_build_object('attribution_review_required', $3::boolean))`, updated, row.Row, value).Scan(&onlyTheFlag); err != nil {
			return nil, err
		}
		if !onlyTheFlag {
			return nil, fmt.Errorf("payment %s became %s, not its row with only the flag changed; nothing was written", row.PaymentId, updated)
		}
		eventId, err := newTimeOrderedId(settings.now())
		if err != nil {
			return nil, err
		}
		rowSum := sha256.Sum256([]byte(row.Row))
		details, err := json.Marshal(map[string]any{
			"change":              plan.Change,
			"run_id":              plan.Run.RunId,
			"export_dir":          dir,
			"host":                host,
			"contracts":           plan.Contracts,
			"payment_contracts":   plan.PaymentContracts[row.PaymentId],
			"manifests_sha256":    plan.ManifestsSha256,
			"verification_sha256": plan.VerificationSha256,
			"plan_digest":         plan.Digest,
			"prior_row_sha256":    hex.EncodeToString(rowSum[:]),
		})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_account_payment (event_id, event_time, payment_id, event_type, event_details)
			VALUES ($1::uuid, now() AT TIME ZONE 'UTC', $2::uuid, $3, $4)
		`, eventId, row.PaymentId, eventType, string(details)); err != nil {
			return nil, err
		}
		after.Payments = append(after.Payments, ReviewPayment{PaymentId: row.PaymentId, Row: updated})
		after.EventIds = append(after.EventIds, eventId)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	after.Time = settings.now().Format(time.RFC3339Nano)
	if err := writeReviewJson(base+"-after.json", after); err != nil {
		return after, fmt.Errorf("the change committed, but its record was not written: %w", err)
	}
	return after, nil
}

// Plans the reversal of a clear from the record it wrote before its change
// (clear-...-before.json), and with commit makes it: each payment must be its
// recorded row with only the flag cleared; the flag is set again in one
// transaction, guarded and journaled as the clear's change was
// (attribution_review_restored). Contracts the reaper deleted meanwhile stay
// deleted; their rows are in the export.
func RestoreReviewFlag(ctx context.Context, conn *pgx.Conn, dir string, priorPath string, host string, commit bool, expect string, settings *ReviewSettings) (*ReviewClearPlan, *ReviewClearRecord, error) {
	run, err := ReadReviewRun(dir)
	if err != nil {
		return nil, nil, err
	}
	prior := &ReviewClearRecord{}
	if err := readReviewJson(priorPath, prior); err != nil {
		return nil, nil, err
	}
	if prior.Format != ReviewClearFormat || prior.Stage != "before" || prior.Change != reviewClearChange || prior.RunId != run.RunId {
		return nil, nil, reviewRefusal("%s is not the record a clear of run %s wrote before its change", priorPath, run.RunId)
	}
	priorSha256, err := fileSha256(priorPath)
	if err != nil {
		return nil, nil, err
	}
	plan := &ReviewClearPlan{
		Change:             reviewRestoreChange,
		Run:                run,
		Contracts:          prior.Contracts,
		PaymentContracts:   map[string]int64{},
		ManifestsSha256:    prior.ManifestsSha256,
		VerificationFile:   priorPath,
		VerificationSha256: priorSha256,
	}
	paymentIds := make([]string, 0, len(prior.Payments))
	for _, payment := range prior.Payments {
		paymentIds = append(paymentIds, payment.PaymentId)
	}
	payments, err := readReviewPayments(ctx, conn, paymentIds)
	if err != nil {
		return nil, nil, err
	}
	expected := []ReviewPayment{}
	for _, recorded := range prior.Payments {
		payment := payments[recorded.PaymentId]
		if payment == nil {
			return nil, nil, reviewRefusal("payment %s does not exist", recorded.PaymentId)
		}
		var cleared bool
		if err := conn.QueryRow(ctx, `SELECT $1::jsonb = ($2::jsonb || '{"attribution_review_required": false}'::jsonb)`, payment.Row, recorded.Row).Scan(&cleared); err != nil {
			return nil, nil, err
		}
		if !cleared {
			return nil, nil, reviewRefusal("payment %s is %s, not its recorded row with only the flag cleared", recorded.PaymentId, payment.Row)
		}
		plan.Payments = append(plan.Payments, payment.ReviewPayment)
		expected = append(expected, payment.ReviewPayment)
	}
	plan.Digest = plan.digest()
	if !commit {
		return plan, nil, nil
	}
	if expect != plan.Digest {
		return plan, nil, reviewRefusal("--expect %q is not the plan's digest %s: read the plan again", expect, plan.Digest)
	}
	record, err := commitReviewFlag(ctx, conn, dir, host, plan, expected, true, ReviewRestoredEventType, settings)
	return plan, record, err
}
