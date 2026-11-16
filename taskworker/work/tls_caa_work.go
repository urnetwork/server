package work

// certs' CAA checks chain (TLS-M6-04; §12.8 step 3, §13.8; D-DNS-9, D-TLS-11,
// FS-32): one run-once chain, every run one pass of certsservice's
// RunCaaChecks from the cursor the last pass left in its args. A pass walks
// and probes each pending strong-mode custom host, the due ones at their
// newest chain's half-life and those an earlier pass left with no verdict, and
// applies each verdict through model.DnsCustomHostResultInTx: at setup a pass
// makes the host active and a failure refuses it with
// custom_host_caa_operator_alias; later a failure clears
// effective_caa_chain_ok and alarms the network's admins with §2.8 EX1's
// exception, the host staying mapped.
//
// What a pass runs on is installed per process (SetTlsCaaChecks), beside the
// certs chains' engine, by taskworker's certs install while sidecar.yml's
// certs switch is on and certs.yml names the dns api (tls_certs_install.go):
// the engine, certs' DNS client for the canary routes, the resolver on the
// system's, the Postgres store and the operator zones, certs.yml's root and
// caa_operator_zones. A process with nothing installed runs an empty pass and
// schedules the next, its cursor kept, so no host is judged and every strong-
// mode host stays pending and unmapped until certs is installed (the closed
// direction). A binary that does not link what custom hosts read neither
// schedules the chain nor touches anything.
//
// A probe takes ProbeWindow at least, 10 minutes, so a pass's task max time
// sits above the probe's timeout. Nothing here names a host in a log (I-13).

import (
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// What a pass of the checks runs on.
type TlsCaaCheckDeps struct {
	// The engine, whose clock and DNS retry settings the probe uses. Required.
	Service *certsservice.CertsService
	// The resolver, the canary routes, the store and the checks' settings.
	// Required.
	Caa *certsservice.CertsCaaDeps
}

var tlsCaaCheckDeps atomic.Pointer[TlsCaaCheckDeps]

// Installs what this process's CAA checks run on; nil, or deps missing a
// part, removes it, and the chain runs empty passes.
func SetTlsCaaChecks(deps *TlsCaaCheckDeps) {
	setTlsCaaChecks(deps)
}

// SetTlsCaaChecks, answering what it installed, nil when it removed: the
// install that put it there removes only that.
func setTlsCaaChecks(deps *TlsCaaCheckDeps) *TlsCaaCheckDeps {
	if deps == nil || deps.Service == nil || deps.Caa == nil {
		tlsCaaCheckDeps.Store(nil)
		return nil
	}
	installed := *deps
	tlsCaaCheckDeps.Store(&installed)
	return &installed
}

// The chain's cadence.
type TlsCaaChecksSettings struct {
	// The next pass runs this long after one, MoreInterval after one whose
	// batch came back full, and RetryInterval after one that failed.
	RunInterval   time.Duration
	MoreInterval  time.Duration
	RetryInterval time.Duration
	// A pass's task max time: past the probe's timeout, which bounds every
	// probe of the pass, since the pass runs its owners side by side.
	MaxTime time.Duration
}

// Every 10 minutes, a pass's max time past the probe's timeout.
func DefaultTlsCaaChecksSettings() *TlsCaaChecksSettings {
	return &TlsCaaChecksSettings{
		RunInterval:   10 * time.Minute,
		MoreInterval:  1 * time.Second,
		RetryInterval: 1 * time.Minute,
		MaxTime:       certsservice.DefaultCertsCaaSettings().ProbeTimeout + 15*time.Minute,
	}
}

type TlsCaaChecksArgs struct {
	// Where the next pass starts. nil: the start.
	Cursor *certsservice.CertsCaaCursor `json:"cursor,omitempty"`
}

type TlsCaaChecksResult struct {
	// Nothing is installed.
	Idle              bool `json:"idle"`
	ActivatedCount    int  `json:"activated_count"`
	RefusedCount      int  `json:"refused_count"`
	CheckedCount      int  `json:"checked_count"`
	ExposedCount      int  `json:"exposed_count"`
	GoneCount         int  `json:"gone_count"`
	InconclusiveCount int  `json:"inconclusive_count"`
	DroppedCount      int  `json:"dropped_count"`
	// The cursor the next pass starts from.
	Next   *certsservice.CertsCaaCursor `json:"next,omitempty"`
	More   bool                         `json:"more"`
	Failed bool                         `json:"failed"`
}

// Whether this binary links what the checks read: custom hosts and certs'
// rows.
func tlsCaaChecksLinked() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg6Linked()
}

// Schedules the chain's first run. A binary that does not link what custom
// hosts read schedules nothing.
func ScheduleTlsCaaChecks(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsCaaChecks(clientSession, tx, &TlsCaaChecksArgs{}, server.NowUtc().Add(DefaultTlsCaaChecksSettings().RunInterval))
}

func scheduleTlsCaaChecks(clientSession *session.ClientSession, tx server.PgTx, args *TlsCaaChecksArgs, runAt time.Time) {
	if !tlsCaaChecksLinked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsCaaChecks,
		args,
		clientSession,
		task.RunOnce("tls_caa_checks"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsCaaChecksSettings().MaxTime),
	)
}

// One pass of the checks.
func TlsCaaChecks(
	args *TlsCaaChecksArgs,
	clientSession *session.ClientSession,
) (*TlsCaaChecksResult, error) {
	result := &TlsCaaChecksResult{Idle: true, Next: args.Cursor}
	deps := tlsCaaCheckDeps.Load()
	if !tlsCaaChecksLinked() || deps == nil {
		return result, nil
	}
	result.Idle = false
	report, err := deps.Service.RunCaaChecks(clientSession.Ctx, deps.Caa, args.Cursor)
	if err != nil {
		glog.Warningf("[tls]caa checks: %v\n", err)
		result.Failed = true
	}
	if report == nil {
		return result, nil
	}
	for _, verdict := range report.Verdicts {
		switch verdict.Outcome {
		case certsservice.CertsCaaOutcomeActivated:
			result.ActivatedCount += 1
		case certsservice.CertsCaaOutcomeRefused:
			result.RefusedCount += 1
		case certsservice.CertsCaaOutcomeChecked:
			result.CheckedCount += 1
		case certsservice.CertsCaaOutcomeExposed:
			result.ExposedCount += 1
		default:
			result.GoneCount += 1
		}
	}
	result.InconclusiveCount = report.InconclusiveCount
	result.DroppedCount = report.DroppedCount
	result.Next = report.Next
	result.More = report.More
	if 0 < len(report.Verdicts)+report.InconclusiveCount {
		glog.Infof(
			"[tls]caa checks: activated %d, refused %d, checked %d, exposed %d, gone %d, no verdict %d, dropped %d\n",
			result.ActivatedCount,
			result.RefusedCount,
			result.CheckedCount,
			result.ExposedCount,
			result.GoneCount,
			result.InconclusiveCount,
			result.DroppedCount,
		)
	}
	return result, nil
}

// Schedules the next pass from the cursor this one left.
func TlsCaaChecksPost(
	_ *TlsCaaChecksArgs,
	result *TlsCaaChecksResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsCaaChecksSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	switch {
	case result.Failed:
		runAt = server.NowUtc().Add(settings.RetryInterval)
	case result.More:
		runAt = server.NowUtc().Add(settings.MoreInterval)
	}
	scheduleTlsCaaChecks(clientSession, tx, &TlsCaaChecksArgs{Cursor: result.Next}, runAt)
	return nil
}

// What the certs install builds the checks from.
type tlsCaaChecksBuild struct {
	service *certsservice.CertsService
	dns     *server.CertsDnsClient
	// nil builds the resolver on the system's
	resolver      certsservice.CertsCaaResolver
	store         certsservice.CertsCaaStore
	operatorZones []string
}

// Builds and installs the checks; the returned call removes them. A part
// that cannot be built leaves the checks uninstalled, said once in the log,
// and the certs chains run on.
func installTlsCaaChecks(build *tlsCaaChecksBuild) (uninstall func()) {
	resolver := build.resolver
	if resolver == nil {
		systemResolver, err := server.NewCertsCaaResolver(nil, nil)
		if err != nil {
			glog.Warningf("[certs]the caa checks are not installed: the resolver: %v\n", err)
			return func() {}
		}
		resolver = systemResolver
	}
	settings := certsservice.DefaultCertsCaaSettings()
	settings.OperatorZones = build.operatorZones
	if err := settings.Validate(); err != nil {
		glog.Warningf("[certs]the caa checks are not installed: %v\n", err)
		return func() {}
	}
	installed := setTlsCaaChecks(&TlsCaaCheckDeps{
		Service: build.service,
		Caa: &certsservice.CertsCaaDeps{
			Resolver: resolver,
			Dns:      build.dns,
			Store:    build.store,
			Settings: settings,
		},
	})
	return func() {
		// only this install's deps: a later install's stay
		tlsCaaCheckDeps.CompareAndSwap(installed, nil)
	}
}
