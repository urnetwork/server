package main

// The platform listener's attribution and halt ladder (SMTP-M9-15, SMTP-M9-16; §14.8, D-SMTP-8, D-SMTP-9,
// plan-SMTP-report-intake-home): the report intake's Join, smtpedgeservice.SmtpAttribution over the send log and
// URnetwork's DKIM keys on model's store, as smtpedge reads them, verifying each X-UR-Ref under the keys smtpedge
// issues them with, from the block's vault (model.SmtpXUrRefKeyFileName, smtpedge's own file, which warp's
// mxedge-platform-x-ur-ref.yml mounts on the listener from M9; question blocks-api-url-platform-x-ur-ref-key); and its
// Halts, smtpedgeservice.SmtpHaltLadder, which runs each stored loop complaint through §14.8's rows and writes each
// halt to smtp_halt. Every smtpedge holds a halt from its next reload of ActiveHalts, and §21's mail pass in taskworker
// records each network halt as a case and an action. An attributed role mail opens a case through §21's sink, and a
// fraud complaint asks it for review.
//
// What the ladder needs in this process, as smtpedge's block gives its own: the store's halt reads, §21's sink
// (model.NewAbuseSmtpSink, whose suspension request kicks the mail pass, the pass running each minute besides, so a
// kick this block cannot make loses no suspension), and a host with Metrics and a Quota, whose Limits gives each
// network's tier. The platform listener holds the platform's Postgres role (pg.yml), so its Quota is
// quotahost.NewLocalQuota over model directly, as api's, connect's and taskworker's are, and needs no route of api's
// internal gate, which the listener's own class, mxedge-platform, reaches only for role mail's notices (QD-1121). The
// ladder holds no SmtpQuota, as smtpedgeservice has the intake's never do: the edges hold halts by reloading them. As
// in smtpedge's block, the host has no Push, so no SmtpStatus hint leaves here, and no Redis, which only an edge's
// hard bounces count on.
//
// A binary that does not link MG9, whose smtp_send, smtp_halt and smtp_dkim_key a join and the ladder read, gets
// neither: each report it would join would fail its store call and be answered 451 until the reporter gave up, where
// without a join an ARF report is dropped and role mail answered 550, as before M9. A vault with no X-UR-Ref key file,
// or one smtpedge.yml's settings refuse, gets neither either: without a key a join drops every report that carries
// the edge's reference, which nearly every report of an edge's send does. The block says which at start, and its
// intake serves on as it did.

import (
	"context"
	"errors"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
)

// The store a join and the ladder read and write: the send log, DKIM keys, complaints and halts. model's
// SmtpEdgeStore in the block, sidecartest's fake in tests.
type platformJoinStore interface {
	smtpedgeservice.SmtpEdgeStore
	smtpedgeservice.SmtpReportLookups
	smtpedgeservice.SmtpHaltLookups
}

// What attributeThrough takes beside the intake's own store and settings.
type platformJoinArgs struct {
	// Lays the X-UR-Ref keys and the issuing key over a copy of smtpedge.yml's settings: the block's vault file
	// (model.LoadSmtpXUrRefKeyFile).
	XUrRefKeys func(settings *smtpedgeservice.SmtpEdgeSettings) error
	// Each network's tier, which the ladder's rows read.
	Quota sidecar.Quota
	// §21's case flow, which the intake's attributed role mail and the ladder's reviews and suspensions reach.
	Sink smtpedgeservice.SmtpAbuseSink
	// The ladder's and the join's clock. nil is real time.
	Clock sidecar.Clock
}

// Attributes the intake's ARF reports and role mail through SMTP-M9-15's join over the intake's store, and runs each
// attributed loop complaint through SMTP-M9-16's ladder, from now on: the intake is made again with both, its other
// seams as they were, the sink args gives in place of its own. An error leaves the intake as it was. Called before the
// listener has the intake, as fileNoticesThrough is.
func (self *platformReportIntake) attributeThrough(args *platformJoinArgs) error {
	store, ok := self.store.(platformJoinStore)
	if !ok {
		return errors.New("report intake: its store reads no send log, keys or halts")
	}
	if args == nil || args.XUrRefKeys == nil || args.Quota == nil || args.Sink == nil {
		return errors.New("report intake: a join needs the X-UR-Ref keys, a Quota and §21's sink")
	}
	settings := *self.settings
	if err := args.XUrRefKeys(&settings); err != nil {
		// the error names the file and the part, never a key
		return err
	}
	if len(settings.XUrRefKeys) == 0 {
		return errors.New("report intake: " + model.SmtpXUrRefKeyFileName + " holds no key, so a join would drop every report carrying an X-UR-Ref")
	}
	clock := args.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	join, err := smtpedgeservice.NewSmtpAttribution(store, store, &settings, clock)
	if err != nil {
		return err
	}
	host := &sidecar.ServiceHost{
		Metrics: sidecarhost.NewMetrics(),
		Quota:   args.Quota,
		Clock:   clock,
	}
	ladder, err := smtpedgeservice.NewSmtpHaltLadder(host, &smtpedgeservice.SmtpHaltLadderDeps{
		Store:   store,
		Lookups: store,
		Halts:   store,
		Sink:    args.Sink,
	}, &settings)
	if err != nil {
		return err
	}
	deps := *self.deps
	deps.Join = join
	deps.Halts = ladder
	deps.Sink = args.Sink
	intake, err := smtpedgeservice.NewSmtpReportIntake(self.host, self.store, &deps, self.settings)
	if err != nil {
		return err
	}
	self.intake = intake
	self.deps = &deps
	return nil
}

// The pending group a join and the ladder read beside the intake's: MG9's send log, halts and keys.
func reportJoinGroups() []*sidecarhost.RunGroup {
	return []*sidecarhost.RunGroup{
		{Name: "MG9", Linked: server.SidecarMg9Linked},
	}
}

// The block's join and ladder: over model's store, the vault's X-UR-Ref key file, a Quota over the block's own
// Postgres on ctx and §21's real sink. An error says why the block attributes nothing, never a key, and leaves the
// intake as it was. The returned call drains the Quota.
func attributeBlockReports(ctx context.Context, intake *platformReportIntake) (func(), error) {
	if !server.SidecarMg9Linked() {
		return func() {}, errors.New("this binary does not link MG9, whose send log, halts and keys a join reads")
	}
	quota, err := quotahost.NewLocalQuota(ctx, nil, nil, nil)
	if err != nil {
		return func() {}, err
	}
	end := func() {
		quota.Drain(context.Background())
	}
	err = intake.attributeThrough(&platformJoinArgs{
		XUrRefKeys: model.LoadSmtpXUrRefKeyFile,
		Quota:      quota,
		Sink:       model.NewAbuseSmtpSink(nil),
	})
	if err != nil {
		end()
		return func() {}, err
	}
	return end, nil
}
