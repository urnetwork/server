package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestArinCurrentCollectorFullPopulationUsesCrossProviderBatches(t *testing.T) {
	r, owner, fact := captureFixture(t)
	const population = 100001
	const perProvider = 5
	cohort := ArinShadowCaptureCohort{GenerationSHA256: strings.Repeat("ab", 32), ObservedAt: r.clock(), Providers: (population + perProvider - 1) / perProvider, Connections: population}
	c, err := r.NewCurrentCaptureCollector(context.Background(), cohort, []string{"all", "us", "ca"})
	if err != nil {
		t.Fatal(err)
	}
	calls, maxBatch := 0, 0
	for start := 0; start < population; start += ArinShadowCaptureBatchLimit {
		page := ArinShadowCapturePage{GenerationSHA256: cohort.GenerationSHA256, Sequence: uint64(start / ArinShadowCaptureBatchLimit)}
		facts := map[Id]ArinShadowCaptureFacts{}
		owners := map[Id]*shadowCaptureTestOwner{}
		for n := start; n < min(population, start+ArinShadowCaptureBatchLimit); n++ {
			id, client := captureNumberId(n+1), captureNumberId(n/perProvider+1)
			input := ArinShadowCaptureRecord{ConnectionId: id, HandlerId: fact.HandlerId}
			if n%perProvider == 0 {
				input.Provider = &ArinShadowCaptureProvider{ClientId: client, ExpectedConnections: min(perProvider, population-n), Buckets: []string{"all", "us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true}
			}
			page.Records = append(page.Records, input)
			f := fact
			f.ConnectionId = id
			f.ClientId = client
			facts[id] = f
			snapshot := owner.snapshot
			snapshot.ConnectionId = id
			snapshot.ClientId = client
			owners[id] = &shadowCaptureTestOwner{available: true, snapshot: snapshot}
		}
		err = c.Consume(page, func(ctx context.Context, ids []Id) ([]ArinShadowCaptureTarget, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("owner resolver unbounded")
			}
			out := make([]ArinShadowCaptureTarget, len(ids))
			for i, id := range ids {
				out[i] = ArinShadowCaptureTarget{id, owners[id]}
			}
			return out, nil
		}, func(ctx context.Context, ids []Id) ([]ArinShadowCaptureFacts, error) {
			calls++
			maxBatch = max(maxBatch, len(ids))
			out := make([]ArinShadowCaptureFacts, len(ids))
			for i, id := range ids {
				out[i] = facts[id]
			}
			return out, nil
		})
		if err != nil {
			t.Fatal("complete bounded page rejected", err)
		}
	}
	report, err := c.Finish(cohort, true)
	if err != nil || !report.CensusComplete || !report.ObservationComplete || !report.NativeMembershipComplete || report.ActualMainCoverage || report.CapturedConnections != population || report.Providers != cohort.Providers || calls != (population+255)/256 || maxBatch != 256 {
		t.Fatal("full source/accounting/batch boundary lost", err)
	}
	for _, row := range report.Buckets {
		if row.Bucket == "ca" {
			if row.Providers != 0 {
				t.Fatal("empty country not explicit")
			}
		} else if row.VerifiedSubscriber != cohort.Providers || row.CandidateQuality != cohort.Providers {
			t.Fatal("provider was lost or counted more than once")
		}
	}
	encoded, _ := json.Marshal(report)
	for _, private := range []string{fact.HandlerId.String(), owner.snapshot.Address.String(), "connection_id", "client_id"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private identity escaped report")
		}
	}
	t.Logf("%d connections / %d providers: %d exact fact batches, peak batch %d; no all-population consumer map", population, cohort.Providers, calls, maxBatch)
}

func TestArinCurrentCollectorRequiresWholeGenerationEndAndMembership(t *testing.T) {
	for _, which := range []string{"healthy", "page_gap", "changed_generation", "missing_end", "changed_end", "missing_owner", "changed_handler", "native_missing", "missing_connection", "duplicate_header"} {
		t.Run(which, func(t *testing.T) {
			r, owner, fact := captureFixture(t)
			cohort := ArinShadowCaptureCohort{GenerationSHA256: strings.Repeat("ab", 32), ObservedAt: r.clock(), Providers: 1, Connections: 1}
			c, err := r.NewCurrentCaptureCollector(context.Background(), cohort, []string{"all"})
			if err != nil {
				t.Fatal(err)
			}
			p := &ArinShadowCaptureProvider{ClientId: fact.ClientId, ExpectedConnections: 1, Buckets: []string{"all"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true}
			page := ArinShadowCapturePage{GenerationSHA256: cohort.GenerationSHA256, Records: []ArinShadowCaptureRecord{{Provider: p, ConnectionId: fact.ConnectionId, HandlerId: fact.HandlerId}}}
			switch which {
			case "page_gap":
				page.Sequence = 1
			case "changed_generation":
				page.GenerationSHA256 = strings.Repeat("cd", 32)
			case "missing_owner":
				owner.available = false
			case "changed_handler":
				page.Records[0].HandlerId = NewId()
			case "native_missing":
				p.MembershipUnavailable = true
			case "missing_connection":
				p.ExpectedConnections = 2
			}
			err = c.Consume(page, func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
				return []ArinShadowCaptureTarget{{fact.ConnectionId, owner}}, nil
			}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
				return []ArinShadowCaptureFacts{fact}, nil
			})
			if which == "duplicate_header" {
				page.Sequence++
				err = c.Consume(page, func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
					return []ArinShadowCaptureTarget{{fact.ConnectionId, owner}}, nil
				}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
					return []ArinShadowCaptureFacts{fact}, nil
				})
			}
			end := cohort
			if which == "changed_end" {
				end.Connections++
			}
			report, finishErr := c.Finish(end, which != "missing_end")
			if which == "healthy" {
				if err != nil || finishErr != nil || !report.ObservationComplete || !report.NativeMembershipComplete {
					t.Fatal("healthy source failed")
				}
				return
			}
			if which == "native_missing" {
				if !report.ObservationComplete || report.NativeMembershipComplete || report.NativeMembershipUnknown != 1 || report.Buckets[0].QualityRemoved != 0 || report.Buckets[0].QualityIndeterminate != 1 {
					t.Fatal("absent native join became a known policy change")
				}
				return
			}
			if report.ObservationComplete {
				t.Fatal("partial or changed source certified complete", which)
			}
		})
	}
}
