package sidecarrestore

// §19.10's abuse, dns and labels steps (OPS-M5-06; D-OPS-14, FS-41, FS-42, FI-12, I-14), in the frame's order. The
// mechanisms are their owners': §21.10's restore is ABUSE-M5-18's (controller.ParseAbuseRecords, PlanAbuseRestore,
// ApplyAbuseRestorePlan, ReplaceLegalHolds), §12.6's restore step DNS-M5-15's (dnshost.NewRestoreStep, the same step
// `sidecarctl dns restore` runs), and the CT replay DNS-M5-16's (work.ScheduleDnsCtReplay). These put them in order and
// keep each one's outcome in the incident's list:
//
//	abuse    first after ops, at any hour, before any block reads the restored rows: every unexpired action and every
//	         lift the abuse mailbox holds from after the sidecar dump re-applied in mail order, each record opened with
//	         the offline seal key and its MAC verified (QD-078), and the holds the legal-request mailbox holds from
//	         after the dump re-placed and listed for counsel, void after a site loss. The step prints its plan and
//	         changes nothing until a run says --apply, so the dns step's snapshot withholds what they suspend and the
//	         edges' first reloads keep every kill and halt
//	dns      next, needing neither api nor the token exchange: §12.6's restore step for each root dns.yml serves, its
//	         serial above every serial the lost primary issued, every running NS reloaded at it, the lists made and
//	         restoring cleared. It is idempotent by incident, so the frame's run and a runbook's `sidecarctl dns
//	         restore` under the same incident never repeat each other: whichever ran first, the next goes on from where
//	         it stood, and the frame records the end
//	labels   after api: dns_ct_replay scheduled once a root and incident from the sidecar dump's time, which the replay
//	         reads back 7 d from, so a name logged in CT that dns_label lost goes back in as retiring and I-14 holds
//	         across the gap; the replay runs in taskworker and retries until every CT log answers
//
// A restored database whose dumps predate MG5 holds no abuse or dns table, and then the abuse, dns and labels steps
// have nothing to restore: each lists that and ends. One that holds them while this binary does not link MG5 is
// refused, since skipping would lose what the steps restore.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/taskworker/work"
)

// The list's rows the M5 steps write.
const (
	// a record re-applied or found applied: table abuse_action, row the action, detail its kind and outcome
	ListKindAbuseRestored = "abuse-restored"
	// a mailbox message refused, never applied: table the mailbox, row its index in the export, detail why
	ListKindAbuseRefused = "abuse-refused"
	// a hold the legal-request mailbox holds from after the dump: table network_legal_hold, row its case, detail its
	// network, outcome and whether the site took what it kept
	ListKindLegalHoldRestored = "legal-hold-restored"
	// a root's restore step ended: table dns_root, row the root, detail its restore serial and the stopped NS
	ListKindDnsRestored = "dns-restored"
	// a root's CT replay scheduled: table dns_label, row the root, detail the dump time it reads back from
	ListKindCtReplayScheduled = "ct-replay-scheduled"
	// a step with nothing to restore: table the step, row the incident, detail why
	ListKindM5StepSkipped = "m5-step-skipped"
)

// The inputs the abuse step reads, each a file on the operator's host (`--input <name>=<path>`).
const (
	// the abuse mailbox's export, an mbox of its sealed records (§21.10)
	StepInputAbuseMailbox = "abuse_mailbox"
	// the legal-request mailbox's export, in the same form
	StepInputLegalMailbox = "legal_mailbox"
	// the mailboxes' offline seal private key, as `sidecarctl abuse mailbox-keygen` wrote it (QD-832)
	StepInputMailboxKey = "mailbox_key"
)

// The mailbox names the abuse step's refusals list.
const (
	abuseMailboxListName = "abuse mailbox"
	legalMailboxListName = "legal-request mailbox"
)

// The restored database's tables that say MG5 came back with the dumps.
const (
	m5AbuseTable = "abuse_action"
	m5DnsTable   = "dns_root"
)

var (
	// The restored database holds MG5's tables and this binary does not link MG5.
	ErrM5Unlinked = errors.New("the restored database holds MG5's tables, which this binary does not link")
)

// Appends row unless the list holds a row of its kind for its table and row already, so a step that runs again
// under the incident lists each thing once, as its first run found it.
func appendOnce(incident *Incident, row *ListRow) error {
	for _, held := range incident.RowsOfKind(row.Kind) {
		if held.Table == row.Table && held.RowId == row.RowId {
			return nil
		}
	}
	return incident.Append(row)
}

// Whether the restored database holds table, in schema public.
func restoredTablePresent(ctx context.Context, table string) (present bool, returnErr error) {
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT pg_catalog.to_regclass($1) IS NOT NULL`,
				"public."+table,
			).Scan(&present))
		})
	}); r != nil {
		return false, fmt.Errorf("whether the database holds %s could not be read: %v", table, r)
	}
	return present, nil
}

// Whether the step has MG5's tables to restore: false, listed, for a database before MG5; an error for one that
// holds them while the binary does not link MG5. tablePresent nil reads the database.
func m5StepHasTables(ctx context.Context, run *StepRun, step string, table string, tablePresent func(ctx context.Context, table string) (bool, error), linked func() bool) (bool, error) {
	if tablePresent == nil {
		tablePresent = restoredTablePresent
	}
	if linked == nil {
		linked = server.SidecarMg5Linked
	}
	present, err := tablePresent(ctx, table)
	if err != nil {
		return false, err
	}
	if !present {
		fmt.Fprintf(run.Out, "no %s in the restored database, which predates MG5: the %s step has nothing to restore\n", table, step)
		return false, appendOnce(run.Incident, &ListRow{Kind: ListKindM5StepSkipped, Table: step, RowId: run.Incident.IncidentId().String(), Detail: "no " + table + ": the database predates MG5"})
	}
	if !linked() {
		return false, fmt.Errorf("%w: run the %s step from a sidecarctl that links MG5", ErrM5Unlinked, step)
	}
	return true, nil
}

// What the abuse step runs on. A nil field takes the real one.
type AbuseStepDeps struct {
	// Whether the restored database holds table.
	TablePresent func(ctx context.Context, table string) (bool, error)
	// Whether this binary links MG5.
	Linked func() bool
	// Reads both exports' verified records with the run's inputs: controller's over the files and the vault's
	// abuse-mailbox.yml.
	LoadRecords func(run *StepRun) (*controller.AbuseMailboxRecords, *controller.LegalHoldMailboxRecords, error)
	// controller.ApplyAbuseRestorePlan.
	Apply func(ctx context.Context, plan *controller.AbuseRestorePlan, apply bool, run *StepRun) (*controller.AbuseRestoreResult, error)
	// controller.ReplaceLegalHolds.
	ReplaceHolds func(ctx context.Context, records []*controller.LegalHoldRecord, dumpTime time.Time, now time.Time, siteLost bool, apply bool, run *StepRun) ([]*controller.LegalHoldRestoreLine, error)
	// Whether to check the database's restore marker: always, but for a test with no database.
	SkipMarker bool
	Now        func() time.Time
}

// The abuse step (see the file header) under run's incident.
func RunAbuseStep(ctx context.Context, deps *AbuseStepDeps, run *StepRun) error {
	if deps == nil {
		deps = &AbuseStepDeps{}
	}
	incident := run.Incident
	if !deps.SkipMarker {
		if err := CheckRestoreMarker(ctx, incident.IncidentId()); err != nil {
			return err
		}
	}
	if ok, err := m5StepHasTables(ctx, run, StepAbuse, m5AbuseTable, deps.TablePresent, deps.Linked); !ok {
		return err
	}
	loadRecords := deps.LoadRecords
	if loadRecords == nil {
		loadRecords = loadAbuseStepRecords
	}
	apply := deps.Apply
	if apply == nil {
		apply = func(ctx context.Context, plan *controller.AbuseRestorePlan, apply bool, run *StepRun) (*controller.AbuseRestoreResult, error) {
			return controller.ApplyAbuseRestorePlan(ctx, plan, apply, run.Out)
		}
	}
	replaceHolds := deps.ReplaceHolds
	if replaceHolds == nil {
		replaceHolds = func(ctx context.Context, records []*controller.LegalHoldRecord, dumpTime time.Time, now time.Time, siteLost bool, apply bool, run *StepRun) ([]*controller.LegalHoldRestoreLine, error) {
			return controller.ReplaceLegalHolds(ctx, records, dumpTime, now, siteLost, apply, run.Out)
		}
	}
	now := deps.Now
	if now == nil {
		now = server.NowUtc
	}
	abuseRecords, legalRecords, err := loadRecords(run)
	if err != nil {
		return err
	}
	dumpTime, _, err := incident.SnapshotTimes()
	if err != nil {
		return err
	}
	kind, _, err := incident.Kind()
	if err != nil {
		return err
	}
	siteLost := kind == IncidentKindSite
	plan := controller.PlanAbuseRestore(abuseRecords, dumpTime, now())

	if !run.Apply {
		// the plan, read before anything changes (QD-078)
		if _, err := apply(ctx, plan, false, run); err != nil {
			return err
		}
		if _, err := replaceHolds(ctx, legalRecords.Records, dumpTime, plan.Now, siteLost, false, run); err != nil {
			return err
		}
		return fmt.Errorf("%w: read it, then run the abuse step again with --apply", ErrStepPlanOnly)
	}

	for _, refusal := range abuseRecords.Refused {
		if err := appendOnce(incident, &ListRow{Kind: ListKindAbuseRefused, Table: abuseMailboxListName, RowId: fmt.Sprint(refusal.Index), Detail: refusal.Err.Error()}); err != nil {
			return err
		}
	}
	for _, refusal := range legalRecords.Refused {
		if err := appendOnce(incident, &ListRow{Kind: ListKindAbuseRefused, Table: legalMailboxListName, RowId: fmt.Sprint(refusal.Index), Detail: refusal.Err.Error()}); err != nil {
			return err
		}
	}
	result, err := apply(ctx, plan, true, run)
	if err != nil {
		return err
	}
	if len(result.Outcomes) != len(plan.Items) {
		return fmt.Errorf("the abuse restore answered %d outcomes for %d records", len(result.Outcomes), len(plan.Items))
	}
	for index, item := range plan.Items {
		row := &ListRow{
			Kind:   ListKindAbuseRestored,
			Table:  m5AbuseTable,
			RowId:  item.Record.ActionId.String(),
			Detail: fmt.Sprintf("%s %s", item.Record.Kind, result.Outcomes[index]),
		}
		if err := appendOnce(incident, row); err != nil {
			return err
		}
	}
	lines, err := replaceHolds(ctx, legalRecords.Records, dumpTime, plan.Now, siteLost, true, run)
	if err != nil {
		return err
	}
	for _, line := range lines {
		detail := fmt.Sprintf("network %s %s %s", line.NetworkId, line.Kind, line.Outcome)
		if line.Void {
			detail += " void: the site took what it kept"
		}
		if err := appendOnce(incident, &ListRow{Kind: ListKindLegalHoldRestored, Table: "network_legal_hold", RowId: line.CaseId.String(), Detail: detail}); err != nil {
			return err
		}
	}
	fmt.Fprintf(run.Out, "the abuse step is done: %d records re-applied in mail order, %d holds listed for counsel, %d messages refused\n",
		len(plan.Items), len(lines), len(abuseRecords.Refused)+len(legalRecords.Refused))
	return nil
}

// Both exports' verified records, opened with the offline seal key the run names and verified under the MAC key
// in this process's abuse-mailbox.yml (controller.LoadAbuseRestoreKeys). Neither key is printed or listed.
func loadAbuseStepRecords(run *StepRun) (*controller.AbuseMailboxRecords, *controller.LegalHoldMailboxRecords, error) {
	abusePath, err := run.RequireInput(StepInputAbuseMailbox)
	if err != nil {
		return nil, nil, err
	}
	legalPath, err := run.RequireInput(StepInputLegalMailbox)
	if err != nil {
		return nil, nil, err
	}
	keyPath, err := run.RequireInput(StepInputMailboxKey)
	if err != nil {
		return nil, nil, err
	}
	settings := controller.DefaultAbuseMailboxSettings()
	keys, err := controller.LoadAbuseRestoreKeys(keyPath, settings)
	if err != nil {
		return nil, nil, err
	}
	abuseData, err := os.ReadFile(abusePath)
	if err != nil {
		return nil, nil, fmt.Errorf("the abuse mailbox's export does not read: %w", err)
	}
	legalData, err := os.ReadFile(legalPath)
	if err != nil {
		return nil, nil, fmt.Errorf("the legal-request mailbox's export does not read: %w", err)
	}
	return controller.ParseAbuseRecords(abuseData, keys.PrivateKey, keys.MacKey, settings),
		controller.ParseLegalHoldRecords(legalData, keys.PrivateKey, keys.MacKey, settings),
		nil
}

// The restore step as the dns step runs it, which dnsservice.RestoreStep is.
type DnsRestoreRunner interface {
	// Runs the step for incidentId on root, answering where it stood even when it returns an error.
	Run(ctx context.Context, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error)
}

// What the dns step runs on. A nil field takes the real one.
type DnsStepDeps struct {
	// Whether the restored database holds table.
	TablePresent func(ctx context.Context, table string) (bool, error)
	// Whether this binary links MG5.
	Linked func() bool
	// The roots dns.yml serves and the restore step over the env's Postgres and Redis, with what releases it.
	Open func(ctx context.Context, run *StepRun) (roots []string, step DnsRestoreRunner, release func(), returnErr error)
	// Whether to check the database's restore marker: always, but for a test with no database.
	SkipMarker bool
}

// The dns step (see the file header) under run's incident: each root's restore step to its end.
func RunDnsStep(ctx context.Context, deps *DnsStepDeps, run *StepRun) error {
	if deps == nil {
		deps = &DnsStepDeps{}
	}
	incident := run.Incident
	if !deps.SkipMarker {
		if err := CheckRestoreMarker(ctx, incident.IncidentId()); err != nil {
			return err
		}
	}
	if ok, err := m5StepHasTables(ctx, run, StepDns, m5DnsTable, deps.TablePresent, deps.Linked); !ok {
		return err
	}
	open := deps.Open
	if open == nil {
		open = openDnsStep
	}
	roots, step, release, err := open(ctx, run)
	if err != nil {
		return err
	}
	defer release()
	for _, root := range roots {
		progress, err := step.Run(ctx, root, sidecar.Id(incident.IncidentId()))
		if progress != nil {
			fmt.Fprintf(run.Out, "%s: restore serial %d, %d NS stopped, %d waiting, %d diffs, done %t\n",
				root, progress.RestoreSerial, len(progress.StoppedNs), len(progress.WaitingNs), progress.DiffCount, progress.Done)
		}
		if err != nil {
			return fmt.Errorf("the restore step for %s has not ended, and the next run goes on from where it stood: %w", root, err)
		}
		if progress == nil || !progress.Done {
			return fmt.Errorf("the restore step for %s answered no end, and the next run goes on from where it stood", root)
		}
		detail := fmt.Sprintf("restore serial %d, %d NS stopped, %d diffs", progress.RestoreSerial, len(progress.StoppedNs), progress.DiffCount)
		if err := appendOnce(incident, &ListRow{Kind: ListKindDnsRestored, Table: m5DnsTable, RowId: root, Detail: detail}); err != nil {
			return err
		}
	}
	fmt.Fprintf(run.Out, "the dns step is done for %d roots: restoring is clear, and later dns writes commit above each restore serial\n", len(roots))
	return nil
}

// dns.yml's roots and §12.6's restore step on the env's Postgres and Redis, as `sidecarctl dns restore` opens them.
func openDnsStep(ctx context.Context, run *StepRun) ([]string, DnsRestoreRunner, func(), error) {
	settings, err := loadDnsStepSettings()
	if err != nil {
		return nil, nil, nil, err
	}
	store, err := dnshost.NewStore(sidecarhost.NewRedis(nil))
	if err != nil {
		return nil, nil, nil, err
	}
	step, err := dnshost.NewRestoreStep(store, settings, nil, &stepLogger{run: run})
	if err != nil {
		store.Close()
		return nil, nil, nil, err
	}
	return settings.Roots, step, store.Close, nil
}

// dns.yml from config.
func loadDnsStepSettings() (*dnsservice.DnsSettings, error) {
	resource, err := server.Config.SimpleResource("dns.yml")
	if err != nil {
		return nil, fmt.Errorf("dns.yml: %w", err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("dns.yml: %w", err)
	}
	settings, err := dnsservice.LoadDnsSettings(data)
	if err != nil {
		return nil, fmt.Errorf("dns.yml: %w", err)
	}
	return settings, nil
}

// A step's log, written to its run's output a line at a time.
type stepLogger struct {
	run *StepRun
}

// See sidecar.Logger.
func (self *stepLogger) Info(args ...any) {
	fmt.Fprintln(self.run.Out, fmt.Sprint(args...))
}

// See sidecar.Logger.
func (self *stepLogger) Infof(format string, args ...any) {
	fmt.Fprintf(self.run.Out, format+"\n", args...)
}

// See sidecar.Logger.
func (self *stepLogger) Warningf(format string, args ...any) {
	fmt.Fprintf(self.run.Out, "warning: "+format+"\n", args...)
}

// See sidecar.Logger.
func (self *stepLogger) Errorf(format string, args ...any) {
	fmt.Fprintf(self.run.Out, "error: "+format+"\n", args...)
}

// No verbose level is on: the step's own lines are what a runbook reads.
func (self *stepLogger) V(level int32) sidecar.Verbose {
	return sidecar.NewNoopLogger().V(level)
}

// What the labels step runs on. A nil field takes the real one.
type LabelsStepDeps struct {
	// Whether the restored database holds table.
	TablePresent func(ctx context.Context, table string) (bool, error)
	// Whether this binary links MG5.
	Linked func() bool
	// The roots dns.yml serves.
	Roots func() ([]string, error)
	// Schedules one root's replay, once a root and incident: work.ScheduleDnsCtReplay in a transaction.
	Schedule func(ctx context.Context, args *work.DnsCtReplayArgs) error
	// Whether to check the database's restore marker: always, but for a test with no database.
	SkipMarker bool
}

// The labels step (see the file header) under run's incident.
func RunLabelsStep(ctx context.Context, deps *LabelsStepDeps, run *StepRun) error {
	if deps == nil {
		deps = &LabelsStepDeps{}
	}
	incident := run.Incident
	if !deps.SkipMarker {
		if err := CheckRestoreMarker(ctx, incident.IncidentId()); err != nil {
			return err
		}
	}
	if ok, err := m5StepHasTables(ctx, run, StepLabels, m5DnsTable, deps.TablePresent, deps.Linked); !ok {
		return err
	}
	rootsOf := deps.Roots
	if rootsOf == nil {
		rootsOf = func() ([]string, error) {
			settings, err := loadDnsStepSettings()
			if err != nil {
				return nil, err
			}
			return settings.Roots, nil
		}
	}
	schedule := deps.Schedule
	if schedule == nil {
		schedule = scheduleCtReplay
	}
	roots, err := rootsOf()
	if err != nil {
		return err
	}
	dumpTime, _, err := incident.SnapshotTimes()
	if err != nil {
		return err
	}
	for _, root := range roots {
		args := &work.DnsCtReplayArgs{Root: root, IncidentId: incident.IncidentId(), DumpTime: dumpTime}
		if err := schedule(ctx, args); err != nil {
			return fmt.Errorf("%s's CT replay: %w", root, err)
		}
		detail := "from the sidecar dump's " + dumpTime.UTC().Format(time.RFC3339Nano)
		if err := appendOnce(incident, &ListRow{Kind: ListKindCtReplayScheduled, Table: "dns_label", RowId: root, Detail: detail}); err != nil {
			return err
		}
	}
	fmt.Fprintf(run.Out, "the labels step is done: %d roots' CT replays scheduled; taskworker's dns_ct_replay retries each until every CT log answers\n", len(roots))
	return nil
}

// work.ScheduleDnsCtReplay in a transaction of its own, on a local session.
func scheduleCtReplay(ctx context.Context, args *work.DnsCtReplayArgs) (returnErr error) {
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleDnsCtReplay(clientSession, tx, args)
		})
	}); r != nil {
		return fmt.Errorf("the replay could not be scheduled: %v", r)
	}
	return nil
}

// The three steps as the process's frame runs them, each on the env WARP_ENV names.
func init() {
	RegisterStep(StepAbuse, func(ctx context.Context, run *StepRun) error {
		return RunAbuseStep(ctx, nil, run)
	})
	RegisterStep(StepDns, func(ctx context.Context, run *StepRun) error {
		return RunDnsStep(ctx, nil, run)
	})
	RegisterStep(StepLabels, func(ctx context.Context, run *StepRun) error {
		return RunLabelsStep(ctx, nil, run)
	})
}
