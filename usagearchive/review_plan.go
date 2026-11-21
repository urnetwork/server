package usagearchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sys/unix"
)

// A payment of the plan, as it is now.
type ReviewPlanPayment struct {
	PaymentId string         `json:"payment_id"`
	Exists    bool           `json:"exists"`
	Blocker   string         `json:"blocker,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
	// its sweeps, and the distinct contracts they pay
	Sweeps          int64  `json:"sweeps"`
	Contracts       int64  `json:"contracts"`
	FirstContractId string `json:"first_contract_id,omitempty"`
	LastContractId  string `json:"last_contract_id,omitempty"`
}

// The tied contracts minted in one contract week (Monday 00:00 UTC), the
// usage archive's week.
type ReviewPlanWeek struct {
	Week      string `json:"week"`
	Contracts int64  `json:"contracts"`
	// the week's usage archive leaf exists; otherwise the reaper's capture of
	// these contracts lands in the archive's DEFAULT partition
	ArchiveLeaf bool `json:"archive_leaf"`
}

// One table's rows in the plan's sample batch, and projected over every tied
// contract.
type ReviewPlanTable struct {
	Table              string  `json:"table"`
	SampleRows         int64   `json:"sample_rows"`
	SampleCopyBytes    int64   `json:"sample_copy_bytes"`
	SampleSeconds      float64 `json:"sample_seconds"`
	ProjectedRows      int64   `json:"projected_rows"`
	ProjectedCopyBytes int64   `json:"projected_copy_bytes"`
}

// What an export of the payments' contracts would read and write, from counts
// only.
type ReviewPlanReport struct {
	Payments []*ReviewPlanPayment `json:"payments"`
	// distinct tied contracts of all the payments
	Contracts        int64              `json:"contracts"`
	Weeks            []*ReviewPlanWeek  `json:"weeks"`
	DefaultContracts int64              `json:"default_contracts"`
	SampleContracts  int64              `json:"sample_contracts"`
	SampleSeconds    float64            `json:"sample_seconds"`
	Tables           []*ReviewPlanTable `json:"tables"`
	// projected over every tied contract
	ProjectedCopyBytes int64 `json:"projected_copy_bytes"`
	ProjectedBatches   int64 `json:"projected_batches"`
	// at the sample's read rate, and at the export's pace
	ReadContractsPerSecond float64 `json:"read_contracts_per_second"`
	ProjectedSeconds       float64 `json:"projected_seconds"`
	// the run in the directory, when one is named
	Dir            string   `json:"dir,omitempty"`
	DirRun         string   `json:"dir_run,omitempty"`
	DirBatches     int64    `json:"dir_batches"`
	DirContracts   int64    `json:"dir_contracts"`
	DirUnfinished  int      `json:"dir_unfinished"`
	DirFreeBytes   uint64   `json:"dir_free_bytes"`
	DirFreePath    string   `json:"dir_free_path,omitempty"`
	Blockers       []string `json:"blockers"`
	Seconds        float64  `json:"seconds"`
	StatementLimit string   `json:"statement_limit"`
}

func (self *ReviewPlanReport) Json() string {
	data, err := json.Marshal(self)
	if err != nil {
		return fmt.Sprintf("%+v", *self)
	}
	return string(data)
}

// The Monday 00:00 UTC of t's week.
func reviewWeekOf(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
}

// Plans an export of the payments' contracts with reads only, each statement
// in a READ ONLY transaction under settings.StatementTimeout: each payment's
// row and its tied contracts by mint day from the sweep index, the weeks whose
// usage archive leaf was already dropped, one sample batch's rows of each
// table, counted with the export's reads, and the run in dir, if any. Writes
// what it finds to out as it goes.
func PlanReview(ctx context.Context, conn *pgx.Conn, paymentIds []string, dir string, settings *ReviewSettings, out io.Writer) (*ReviewPlanReport, error) {
	start := time.Now()
	report := &ReviewPlanReport{
		Payments:       []*ReviewPlanPayment{},
		Weeks:          []*ReviewPlanWeek{},
		Tables:         []*ReviewPlanTable{},
		Blockers:       []string{},
		StatementLimit: settings.StatementTimeout.String(),
	}
	defer func() {
		report.Seconds = time.Since(start).Seconds()
	}()

	// the payments
	var payments map[string]*reviewPaymentState
	err := reviewReadTx(ctx, conn, settings, pgx.ReadCommitted, func(tx pgx.Tx) error {
		var err error
		payments, err = readReviewPayments(ctx, tx, paymentIds)
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, paymentId := range paymentIds {
		planned := &ReviewPlanPayment{PaymentId: paymentId}
		report.Payments = append(report.Payments, planned)
		payment := payments[paymentId]
		planned.Blocker = reviewPaymentBlocker(paymentId, payment)
		if planned.Blocker != "" {
			report.Blockers = append(report.Blockers, planned.Blocker)
		}
		if payment == nil {
			continue
		}
		planned.Exists = true
		row := map[string]any{}
		if err := json.Unmarshal([]byte(payment.Row), &row); err != nil {
			return nil, err
		}
		planned.Fields = map[string]any{}
		for _, field := range []string{"network_id", "completed", "complete_time", "canceled", "attribution_review_required", "payout_byte_count", "payout_nano_cents", "create_time", "min_sweep_time"} {
			planned.Fields[field] = row[field]
		}
		fmt.Fprintf(out, "payment %s: %v\n", paymentId, planned.Fields)
	}

	// each payment's tied contracts, first and last
	var first, last string
	for _, planned := range report.Payments {
		err := reviewReadTx(ctx, conn, settings, pgx.ReadCommitted, func(tx pgx.Tx) error {
			var low, high *string
			if err := tx.QueryRow(ctx, `
				SELECT
					(SELECT contract_id::text FROM transfer_escrow_sweep WHERE payment_id = $1::uuid ORDER BY contract_id LIMIT 1),
					(SELECT contract_id::text FROM transfer_escrow_sweep WHERE payment_id = $1::uuid ORDER BY contract_id DESC LIMIT 1)
			`, planned.PaymentId).Scan(&low, &high); err != nil {
				return err
			}
			if low != nil && high != nil {
				planned.FirstContractId = *low
				planned.LastContractId = *high
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if planned.FirstContractId == "" {
			fmt.Fprintf(out, "payment %s pays no sweep\n", planned.PaymentId)
			continue
		}
		if first == "" || planned.FirstContractId < first {
			first = planned.FirstContractId
		}
		if last == "" || last < planned.LastContractId {
			last = planned.LastContractId
		}
	}

	// tied contracts by mint day, each payment's and all of them
	if first != "" {
		firstTime, err := ContractIdTime(first)
		if err != nil {
			return nil, err
		}
		lastTime, err := ContractIdTime(last)
		if err != nil {
			return nil, err
		}
		weeks := map[time.Time]*ReviewPlanWeek{}
		perPayment := []string{}
		perPaymentDistinct := []string{}
		union := []string{}
		for _, paymentId := range paymentIds {
			rangeSql := `FROM transfer_escrow_sweep WHERE payment_id = '` + paymentId + `'::uuid AND $1::uuid <= contract_id AND contract_id < $2::uuid`
			perPayment = append(perPayment, `(SELECT count(*) `+rangeSql+`)`)
			perPaymentDistinct = append(perPaymentDistinct, `(SELECT count(DISTINCT contract_id) `+rangeSql+`)`)
			union = append(union, `SELECT contract_id `+rangeSql)
		}
		daySql := `SELECT ARRAY[` + strings.Join(perPayment, ", ") + `]::bigint[], ARRAY[` + strings.Join(perPaymentDistinct, ", ") + `]::bigint[], (SELECT count(*) FROM (` + strings.Join(union, " UNION ") + `) AS tied)`
		day := time.Date(firstTime.Year(), firstTime.Month(), firstTime.Day(), 0, 0, 0, 0, time.UTC)
		for !lastTime.Before(day) {
			next := day.AddDate(0, 0, 1)
			var sweeps, contracts []int64
			var dayContracts int64
			err := reviewReadTx(ctx, conn, settings, pgx.ReadCommitted, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, daySql, ContractIdFloor(day), ContractIdFloor(next)).Scan(&sweeps, &contracts, &dayContracts)
			})
			if err != nil {
				return nil, fmt.Errorf("the tied contracts of %s: %w", day.Format(time.DateOnly), err)
			}
			for i, planned := range report.Payments {
				planned.Sweeps += sweeps[i]
				planned.Contracts += contracts[i]
			}
			report.Contracts += dayContracts
			if 0 < dayContracts {
				weekStart := reviewWeekOf(day)
				week := weeks[weekStart]
				if week == nil {
					week = &ReviewPlanWeek{Week: weekStart.Format(time.DateOnly)}
					weeks[weekStart] = week
				}
				week.Contracts += dayContracts
				fmt.Fprintf(out, "%s: %d tied contracts (by payment %v, sweeps %v)\n", day.Format(time.DateOnly), dayContracts, contracts, sweeps)
			}
			day = next
		}
		leaves, err := ListWeekLeaves(ctx, conn)
		if err != nil {
			return nil, err
		}
		leafWeeks := map[string]bool{}
		for _, leaf := range leaves {
			leafWeeks[leaf.WeekStart.Format(time.DateOnly)] = true
		}
		for _, week := range weeks {
			week.ArchiveLeaf = leafWeeks[week.Week]
			if !week.ArchiveLeaf {
				report.DefaultContracts += week.Contracts
			}
			report.Weeks = append(report.Weeks, week)
		}
		sort.Slice(report.Weeks, func(i, j int) bool { return report.Weeks[i].Week < report.Weeks[j].Week })
		for _, week := range report.Weeks {
			fmt.Fprintf(out, "week %s: %d tied contracts, usage archive leaf present: %t\n", week.Week, week.Contracts, week.ArchiveLeaf)
		}
	}
	for _, planned := range report.Payments {
		fmt.Fprintf(out, "payment %s: %d sweeps pay %d contracts, %s..%s\n", planned.PaymentId, planned.Sweeps, planned.Contracts, planned.FirstContractId, planned.LastContractId)
	}
	fmt.Fprintf(out, "tied contracts, all payments: %d; minted in weeks whose usage archive leaf is gone: %d\n", report.Contracts, report.DefaultContracts)

	// one sample batch, counted with the export's reads
	if 0 < report.Contracts && 0 < settings.SampleContracts {
		tables, err := ReadReviewSchema(ctx, conn)
		if err != nil {
			return nil, err
		}
		sampleStart := time.Now()
		err = reviewReadTx(ctx, conn, settings, pgx.RepeatableRead, func(tx pgx.Tx) error {
			contractIds, _, err := readReviewBatchIds(ctx, tx, paymentIds, zeroContractId, settings.SampleContracts)
			if err != nil || len(contractIds) == 0 {
				return err
			}
			report.SampleContracts = int64(len(contractIds))
			if err := execAll(ctx, tx, reviewBitmapSql(settings)); err != nil {
				return err
			}
			idsCte := reviewIdsCteSql(paymentIds, zeroContractId, contractIds[len(contractIds)-1])
			for _, table := range tables {
				planned := &ReviewPlanTable{Table: table.Table}
				tableStart := time.Now()
				if err := tx.QueryRow(ctx, idsCte+`SELECT count(*), coalesce(sum(octet_length(`+ReviewCopyLineSql(table, "t")+`) + 1), 0)::bigint `+reviewRowsWhereSql(table)).Scan(&planned.SampleRows, &planned.SampleCopyBytes); err != nil {
					return fmt.Errorf("sample %s: %w", table.Table, err)
				}
				planned.SampleSeconds = time.Since(tableStart).Seconds()
				report.Tables = append(report.Tables, planned)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		report.SampleSeconds = time.Since(sampleStart).Seconds()
		if 0 < report.SampleContracts {
			scale := float64(report.Contracts) / float64(report.SampleContracts)
			for _, planned := range report.Tables {
				planned.ProjectedRows = int64(float64(planned.SampleRows) * scale)
				planned.ProjectedCopyBytes = int64(float64(planned.SampleCopyBytes) * scale)
				report.ProjectedCopyBytes += planned.ProjectedCopyBytes
				fmt.Fprintf(out, "sample %s: %d rows, %d COPY bytes in %.2fs; projected %d rows, %d COPY bytes\n",
					planned.Table, planned.SampleRows, planned.SampleCopyBytes, planned.SampleSeconds, planned.ProjectedRows, planned.ProjectedCopyBytes)
			}
			report.ReadContractsPerSecond = float64(report.SampleContracts) / max(report.SampleSeconds, 0.001)
			batch := max(1, settings.BatchContracts)
			report.ProjectedBatches = (report.Contracts + int64(batch) - 1) / int64(batch)
			rate := report.ReadContractsPerSecond
			if 0 < settings.MaxContractsPerSecond {
				rate = min(rate, settings.MaxContractsPerSecond)
			}
			report.ProjectedSeconds = float64(report.Contracts) / rate
			fmt.Fprintf(out, "sample: %d contracts read in %.2fs (%.0f a second); projected %d batches of %d, %d COPY bytes, %.0fs at the pace of %.0f a second\n",
				report.SampleContracts, report.SampleSeconds, report.ReadContractsPerSecond, report.ProjectedBatches, batch, report.ProjectedCopyBytes, report.ProjectedSeconds, rate)
		}
	}

	// the run in the directory
	if dir != "" {
		report.Dir = dir
		run, batches, unfinished, err := ReviewStatus(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(out, "%s holds no run yet\n", dir)
		case err != nil:
			report.Blockers = append(report.Blockers, fmt.Sprintf("%s: %s", dir, err))
		default:
			report.DirRun = run.RunId
			report.DirBatches = int64(len(batches))
			report.DirUnfinished = len(unfinished)
			for _, batch := range batches {
				report.DirContracts += batch.Contracts
			}
			if strings.Join(run.PaymentIds(), ",") != strings.Join(paymentIds, ",") {
				report.Blockers = append(report.Blockers, fmt.Sprintf("the run in %s exports payments %v", dir, run.PaymentIds()))
			}
			fmt.Fprintf(out, "%s: run %s, %d batches, %d contracts, %d unfinished files\n", dir, run.RunId, report.DirBatches, report.DirContracts, report.DirUnfinished)
		}
		path := dir
		for {
			var stat unix.Statfs_t
			if err := unix.Statfs(path, &stat); err == nil {
				report.DirFreeBytes = uint64(stat.Bavail) * uint64(stat.Bsize)
				report.DirFreePath = path
				break
			}
			parent := filepath.Dir(path)
			if parent == path {
				break
			}
			path = parent
		}
		fmt.Fprintf(out, "free on %s: %d bytes (the export stops under %d)\n", report.DirFreePath, report.DirFreeBytes, settings.MinFreeBytes)
		if report.DirFreeBytes < settings.MinFreeBytes+uint64(report.ProjectedCopyBytes/2) {
			report.Blockers = append(report.Blockers, fmt.Sprintf("%s has %d bytes free, under the floor %d plus half the projected COPY bytes", report.DirFreePath, report.DirFreeBytes, settings.MinFreeBytes))
		}
	}
	return report, nil
}
