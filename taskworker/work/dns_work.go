package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §12's taskworker chain (DNS-M5-09, DNS-M5-13, DNS-M5-14, DNS-M5-15,
// DNS-M5-16, DNS-M6-01, DNS-M6-03, DNS-M6-05): each run purges
// dns_zone_journal's rows past their 7 days (§12.14), removes challenges and
// canaries past their hour through the journal (D-DNS-8), a strong-mode probe's
// CAA canary and its copy among them, lifts zone suspensions whose until has
// passed (§12.9), drops a restore's lists, posted diffs and CT replay records
// past their 400 days (dns-1), tombstones the ownerless retiring names a
// restore's replay left that nobody re-bound in 90 days, tombstones the aliases
// a release or a change retired 90 days ago, their rows and CAA leaving the
// root (D-DNS-10, I-14), and drops the custom-host map's versions past their 7
// days, the newest kept (§12.14). A binary that does not link MG5 neither
// schedules the chain nor, should an older row run, touches anything.

// Cadences of the chain.
type DnsMaintenanceSettings struct {
	// The chain runs again this long after a run, and RetryInterval after one
	// that left work behind or failed. A minute keeps an expired challenge
	// within a minute of its hour.
	RunInterval   time.Duration
	RetryInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
}

func DefaultDnsMaintenanceSettings() *DnsMaintenanceSettings {
	return &DnsMaintenanceSettings{
		RunInterval:   1 * time.Minute,
		RetryInterval: 10 * time.Second,
		MaxTime:       5 * time.Minute,
	}
}

type DnsMaintenanceArgs struct {
}

type DnsMaintenanceResult struct {
	PurgedJournalCount     int  `json:"purged_journal_count"`
	ExpiredCount           int  `json:"expired_count"`
	LiftedCount            int  `json:"lifted_count"`
	PurgedRestoreListCount int  `json:"purged_restore_list_count"`
	TombstonedLabelCount   int  `json:"tombstoned_label_count"`
	PurgedIncidentCount    int  `json:"purged_incident_count"`
	TombstonedAliasCount   int  `json:"tombstoned_alias_count"`
	PurgedMapVersionCount  int  `json:"purged_map_version_count"`
	More                   bool `json:"more"`
}

// Schedules the chain's first run. A binary that does not link MG5 schedules
// nothing.
func ScheduleDnsMaintenance(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleDnsMaintenance(clientSession, tx, server.NowUtc().Add(DefaultDnsMaintenanceSettings().RunInterval))
}

func scheduleDnsMaintenance(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		DnsMaintenance,
		&DnsMaintenanceArgs{},
		clientSession,
		task.RunOnce("dns_maintenance"),
		task.RunAt(runAt),
		task.MaxTime(DefaultDnsMaintenanceSettings().MaxTime),
	)
}

// One run of the chain.
func DnsMaintenance(
	_ *DnsMaintenanceArgs,
	clientSession *session.ClientSession,
) (*DnsMaintenanceResult, error) {
	if !server.SidecarMg5Linked() {
		return &DnsMaintenanceResult{}, nil
	}
	ctx := clientSession.Ctx
	now := server.NowUtc()
	result := &DnsMaintenanceResult{}
	purgedCount, err := model.PurgeDnsJournals(ctx, now)
	result.PurgedJournalCount = purgedCount
	if err != nil {
		glog.Warningf("[dns]journal purge: %v\n", err)
		result.More = true
	}
	expiredCount, more, err := model.RemoveExpiredDnsChallenges(ctx, now)
	result.ExpiredCount = expiredCount
	if err != nil {
		glog.Warningf("[dns]challenge expiry: %v\n", err)
	}
	result.More = result.More || more || err != nil
	liftedCount, err := model.LiftExpiredDnsSuspensions(ctx, now)
	result.LiftedCount = liftedCount
	if err != nil {
		glog.Warningf("[dns]suspension lift: %v\n", err)
		result.More = true
	}
	purgedListCount, err := model.PurgeDnsRestoreLists(ctx, now)
	result.PurgedRestoreListCount = purgedListCount
	if err != nil {
		glog.Warningf("[dns]restore list purge: %v\n", err)
		result.More = true
	}
	tombstonedCount, more, err := model.TombstoneUnclaimedDnsLabels(ctx, now)
	result.TombstonedLabelCount = tombstonedCount
	if err != nil {
		glog.Warningf("[dns]unclaimed label tombstones: %v\n", err)
	}
	result.More = result.More || more || err != nil
	purgedIncidentCount, more, err := model.PurgeDnsRestoreIncidents(ctx, now)
	result.PurgedIncidentCount = purgedIncidentCount
	if err != nil {
		glog.Warningf("[dns]restore incident purge: %v\n", err)
	}
	result.More = result.More || more || err != nil
	tombstonedAliasCount, more, err := model.TombstoneRetiredDnsAliases(ctx, now)
	result.TombstonedAliasCount = tombstonedAliasCount
	if err != nil {
		glog.Warningf("[dns]retired alias tombstones: %v\n", err)
	}
	result.More = result.More || more || err != nil
	purgedMapVersionCount, err := model.PurgeDnsCustomHostMapVersions(ctx, now)
	result.PurgedMapVersionCount = purgedMapVersionCount
	if err != nil {
		glog.Warningf("[dns]custom-host map purge: %v\n", err)
		result.More = true
	}
	return result, nil
}

// Schedules the next run, sooner when this one left work behind.
func DnsMaintenancePost(
	_ *DnsMaintenanceArgs,
	result *DnsMaintenanceResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultDnsMaintenanceSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleDnsMaintenance(clientSession, tx, runAt)
	return nil
}
