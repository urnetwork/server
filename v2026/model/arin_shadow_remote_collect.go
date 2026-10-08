package model

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/urnetwork/server/v2026"
)

// Bounded look-ahead groups a full source stream by owning process. Sending a
// 256-row mixed-provider page to every process would multiply round trips.
// This cap holds 16 source pages; each RPC/fact query still contains <=256 keys.
const arinShadowPrefetchConnections = 4096

type ArinShadowRemoteFleet struct {
	Handlers          map[server.Id]*server.ArinShadowRPCClient
	Native            *server.ArinShadowRPCClient
	InventoryComplete bool
}

// The fleet inventory is separately attested by the operator. Missing owners
// are explicit unknowns; no input member disappears from the source cohort.
func CollectArinShadowRemotePublic(ctx context.Context, recorder *server.ArinShadowRecorder, fleet ArinShadowRemoteFleet, required []string) (server.ArinShadowCaptureReport, error) {
	if ctx == nil || ctx.Err() != nil || recorder == nil || fleet.Native == nil || fleet.Native.Identity().Role != "native" || !fleet.InventoryComplete || len(fleet.Handlers) > 2048 {
		return server.ArinShadowCaptureReport{}, server.ErrArinShadowInput
	}
	bounded, cancel := context.WithTimeout(ctx, server.ArinShadowCaptureMaxAge)
	defer cancel()
	var lease ArinShadowNativeLeaseInfo
	if fleet.Native.Call(bounded, "native_acquire", struct{}{}, &lease) != nil || lease.Token == (server.Id{}) || !lease.ExpiresAt.After(server.NowUtc()) || !lease.SourceCompletedAt.Before(lease.ExpiresAt) {
		return server.ArinShadowCaptureReport{}, server.ErrArinShadowInput
	}
	boundedLease, stop := context.WithDeadline(bounded, lease.ExpiresAt)
	defer stop()
	defer func() {
		// Best-effort within the original owner. Expiry independently drops
		// the retained generation even if the transport/owner has failed.
		var empty struct{}
		_ = fleet.Native.Call(bounded, "native_release", struct {
			Token server.Id `json:"token"`
		}{lease.Token}, &empty)
	}()
	var collector *server.ArinShadowCaptureCollector
	var source server.ArinShadowCaptureCohort
	var lastProvider server.Id
	var sequence uint64
	var pending []ArinShadowPublicConnection
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		clients := []server.Id{}
		seenClients := map[server.Id]bool{}
		for _, row := range pending {
			if row.ClientId != lastProvider && !seenClients[row.ClientId] {
				seenClients[row.ClientId] = true
				clients = append(clients, row.ClientId)
			}
		}
		members := map[server.Id]ArinShadowNativeMember{}
		for start := 0; start < len(clients); start += server.ArinShadowCaptureBatchLimit {
			ids := clients[start:min(start+server.ArinShadowCaptureBatchLimit, len(clients))]
			var result []ArinShadowNativeMember
			if fleet.Native.Call(boundedLease, "native_members", ArinShadowNativeMemberRequest{lease.Token, ids}, &result) != nil || len(result) != len(ids) {
				return server.ErrArinShadowInput
			}
			for i, member := range result {
				if member.ClientId != ids[i] {
					return server.ErrArinShadowInput
				}
				members[member.ClientId] = member
			}
		}
		groups := map[*server.ArinShadowRPCClient][]server.Id{}
		for _, row := range pending {
			client := fleet.Handlers[row.HandlerId]
			groups[client] = append(groups[client], row.ConnectionId)
		}
		jobs := []arinShadowCaptureJob{}
		for client, ids := range groups {
			for start := 0; start < len(ids); start += server.ArinShadowCaptureBatchLimit {
				jobs = append(jobs, arinShadowCaptureJob{client, ids[start:min(start+server.ArinShadowCaptureBatchLimit, len(ids))]})
			}
		}
		if len(jobs) > 256 {
			return server.ErrArinShadowInput
		}
		// Keep all batches for one host adjacent. The operator's two-slot
		// SSH pool multiplexes its processes and evicts only idle transports.
		slices.SortStableFunc(jobs, func(a, b arinShadowCaptureJob) int {
			if a.client == nil {
				if b.client == nil {
					return 0
				}
				return -1
			}
			if b.client == nil {
				return 1
			}
			return cmp.Compare(a.client.Identity().Host, b.client.Identity().Host)
		})
		batches := make([]server.ArinShadowCapturedBatch, len(jobs))
		runArinShadowCaptureJobs(jobs, func(i int) {
			job := jobs[i]
			var err error
			if job.client != nil {
				batches[i], err = recorder.CallCapture(boundedLease, job.client, job.ids)
			} else {
				err = server.ErrArinShadowInput
			}
			if err != nil {
				batches[i], _ = recorder.UnavailableCaptureBatch(job.ids)
			}
		})
		if boundedLease.Err() != nil {
			return server.ErrArinShadowInput
		}
		for start := 0; start < len(pending); start += server.ArinShadowCaptureBatchLimit {
			rows := pending[start:min(start+server.ArinShadowCaptureBatchLimit, len(pending))]
			page := server.ArinShadowCapturePage{GenerationSHA256: lease.GenerationSHA256, Sequence: sequence, Records: make([]server.ArinShadowCaptureRecord, len(rows))}
			ids := make([]server.Id, len(rows))
			for i, row := range rows {
				record := server.ArinShadowCaptureRecord{ConnectionId: row.ConnectionId, HandlerId: row.HandlerId}
				ids[i] = row.ConnectionId
				if row.ClientId != lastProvider {
					member, ok := members[row.ClientId]
					if !ok {
						return server.ErrArinShadowInput
					}
					record.Provider = &server.ArinShadowCaptureProvider{ClientId: row.ClientId, ExpectedConnections: row.ProviderConnections, Buckets: slices.Clone(member.Buckets), BaseQuality: member.BaseQuality, BaseSpeed: member.BaseSpeed, ActiveQuality: member.ActiveQuality, ActiveSpeed: member.ActiveSpeed, MembershipUnavailable: member.Unavailable}
					lastProvider = row.ClientId
				}
				page.Records[i] = record
			}
			batch, err := server.SplitArinShadowCapturedBatch(ids, batches...)
			if err != nil {
				return err
			}
			if err = collector.ConsumeCaptured(page, batch); err != nil {
				return err
			}
			sequence++
		}
		pending = nil
		return nil
	}
	cohort, err := StreamArinShadowPublicCohort(boundedLease, func(public ArinShadowPublicCohort) error {
		source = server.ArinShadowCaptureCohort{GenerationSHA256: lease.GenerationSHA256, ObservedAt: public.ObservedAt, Providers: public.Providers, Connections: public.Connections}
		var err error
		collector, err = recorder.NewCurrentCaptureCollector(boundedLease, source, required)
		return err
	}, func(page []ArinShadowPublicConnection) error {
		pending = append(pending, page...)
		if len(pending) > arinShadowPrefetchConnections {
			return server.ErrArinShadowInput
		}
		if len(pending) == arinShadowPrefetchConnections {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if collector == nil {
		return server.ArinShadowCaptureReport{}, server.ErrArinShadowInput
	}
	var end ArinShadowNativeEnd
	endErr := fleet.Native.Call(boundedLease, "native_end", struct {
		Token server.Id `json:"token"`
	}{lease.Token}, &end)
	complete := err == nil && cohort.Complete && endErr == nil && end.GenerationSHA256 != "" && boundedLease.Err() == nil
	report, finishErr := collector.Finish(source, complete)
	report.NativeSourceStartedAt = lease.SourceStartedAt
	report.NativeSourceCompletedAt = lease.SourceCompletedAt
	report.NativePublishedAt = lease.PublishedAt
	report.NativeGenerationAtEnd = end.GenerationSHA256
	report.NativeGenerationChanged = end.GenerationSHA256 != "" && end.GenerationSHA256 != lease.GenerationSHA256
	if !complete || finishErr != nil {
		return report, server.ErrArinShadowInput
	}
	return report, nil
}

type arinShadowCaptureJob struct {
	client *server.ArinShadowRPCClient
	ids    []server.Id
}

// A host's multiplexed transport serves one request at a time. Schedule one
// worker per host, in pairs, rather than occupying all workers with requests
// waiting behind the first host. This keeps both existing transport permits
// usable without adding sessions, growing the prefetch or extending the lease.
// The caller supplies jobs sorted by host and joins every pair before moving on.
func runArinShadowCaptureJobs(jobs []arinShadowCaptureJob, call func(int)) {
	type span struct{ start, end int }
	groups := []span{}
	host := func(job arinShadowCaptureJob) string {
		if job.client == nil {
			return ""
		}
		return job.client.Identity().Host
	}
	for i := 0; i < len(jobs); {
		end := i + 1
		for end < len(jobs) && host(jobs[end]) == host(jobs[i]) {
			end++
		}
		groups = append(groups, span{i, end})
		i = end
	}
	for next := 0; next < len(groups); next += 2 {
		var wg sync.WaitGroup
		for _, group := range groups[next:min(next+2, len(groups))] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := group.start; i < group.end; i++ {
					call(i)
				}
			}()
		}
		wg.Wait()
	}
}
