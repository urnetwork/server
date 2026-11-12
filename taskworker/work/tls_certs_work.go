package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// certs' purge chain (TLS-M5-17; §13.8, cert-1): each run deletes the rows of
// network_tls_cert whose certificate's validity ended more than the row
// retention ago, 30 days, in any state (CertsStore.PurgeEnded). A row with no
// validity, a failed order's, has no time here to count from and stays until
// the row keeps one (tls-m5-14-cert-row-end-time). Nothing here reaches a CA
// or a zone. A binary that does not link MG5 neither schedules the chain nor,
// should an older row run, touches anything.
//
// The order, renewal and revocation chains of TLS-M5-17 are not built yet.

// Cadences of the purge.
type TlsCertPurgeSettings struct {
	// The purge runs again this long after a run, and RetryInterval after one
	// that failed. The retention is in days, so an hour is exact enough.
	RunInterval   time.Duration
	RetryInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
	// A row stays this long past its certificate's end: the engine's
	// RowRetention, cert-1's 30 d.
	RowRetention time.Duration
}

func DefaultTlsCertPurgeSettings() *TlsCertPurgeSettings {
	return &TlsCertPurgeSettings{
		RunInterval:   1 * time.Hour,
		RetryInterval: 5 * time.Minute,
		MaxTime:       10 * time.Minute,
		RowRetention:  certsservice.DefaultCertsSettings().RowRetention,
	}
}

type TlsCertPurgeArgs struct {
}

type TlsCertPurgeResult struct {
	RemovedCount int  `json:"removed_count"`
	More         bool `json:"more"`
}

// Schedules the purge's first run. A binary that does not link MG5 schedules
// nothing.
func ScheduleTlsCertPurge(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsCertPurge(clientSession, tx, server.NowUtc().Add(DefaultTlsCertPurgeSettings().RunInterval))
}

func scheduleTlsCertPurge(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsCertPurge,
		&TlsCertPurgeArgs{},
		clientSession,
		task.RunOnce("tls_cert_purge"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsCertPurgeSettings().MaxTime),
	)
}

// One run of the purge.
func TlsCertPurge(
	_ *TlsCertPurgeArgs,
	clientSession *session.ClientSession,
) (*TlsCertPurgeResult, error) {
	return tlsCertPurgeAt(clientSession, server.NowUtc())
}

func tlsCertPurgeAt(clientSession *session.ClientSession, now time.Time) (*TlsCertPurgeResult, error) {
	if !server.SidecarMg5Linked() {
		return &TlsCertPurgeResult{}, nil
	}
	settings := DefaultTlsCertPurgeSettings()
	removedCount, err := model.NewCertsStore(nil).PurgeEnded(clientSession.Ctx, now.Add(-settings.RowRetention))
	if err != nil {
		glog.Warningf("[tls]cert purge: %v\n", err)
	}
	return &TlsCertPurgeResult{RemovedCount: removedCount, More: err != nil}, nil
}

// Schedules the next run, sooner when this one failed.
func TlsCertPurgePost(
	_ *TlsCertPurgeArgs,
	result *TlsCertPurgeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsCertPurgeSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleTlsCertPurge(clientSession, tx, runAt)
	return nil
}
