package model

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// What one network may hold of an extender sample the operator draws for
// clients (ExtenderSampleSettings). On 2026-11-16 two networks held 86% of the
// active extenders, so every sample was mostly theirs. Every address is an RFC
// 5737 or RFC 3849 documentation address and every id is generated.

// A network's quota of a sample: the size times the share, rounded up, at
// least one and at most the size; a non-positive share is no cap.
func TestExtenderSampleNetworkQuota(t *testing.T) {
	for _, c := range []struct {
		sampleCount     int
		maxNetworkShare float64
		want            int
	}{
		{sampleCount: 8, maxNetworkShare: 0.25, want: 2},
		{sampleCount: 3, maxNetworkShare: 0.25, want: 1},
		{sampleCount: 4, maxNetworkShare: 0.25, want: 1},
		{sampleCount: 5, maxNetworkShare: 0.25, want: 2},
		{sampleCount: 1, maxNetworkShare: 0.25, want: 1},
		{sampleCount: 8, maxNetworkShare: 2, want: 8},
		{sampleCount: 8, maxNetworkShare: 0, want: 8},
		{sampleCount: 8, maxNetworkShare: -1, want: 8},
		{sampleCount: 0, maxNetworkShare: 0.25, want: 0},
	} {
		if got := ExtenderSampleNetworkQuota(c.sampleCount, c.maxNetworkShare); got != c.want {
			t.Errorf("a sample of %d at a share of %v: quota %d, want %d", c.sampleCount, c.maxNetworkShare, got, c.want)
		}
	}
	if got := DefaultExtenderSampleSettings().NetworkQuota(8); got != 2 {
		t.Errorf("the default quota of a sample of 8 is %d, want 2", got)
	}
}

// A sampler admits a network up to its quota and no further while the sample
// has room, takes a pinned or passed-over entry over the quota, and takes
// nothing once full.
func TestExtenderNetworkSamplerAdmitsUpToTheQuota(t *testing.T) {
	fleetNetworkId := server.NewId()
	otherNetworkId := server.NewId()
	sampler := NewExtenderNetworkSampler(3, 1)
	if !sampler.Admits(fleetNetworkId) {
		t.Fatal("an empty sample refused the fleet")
	}
	sampler.Take(fleetNetworkId)
	if sampler.Admits(fleetNetworkId) {
		t.Fatal("the fleet was admitted past its quota of one")
	}
	if !sampler.Admits(otherNetworkId) {
		t.Fatal("another network was refused under its quota")
	}
	sampler.Take(otherNetworkId)
	if !sampler.TakeOver(fleetNetworkId) {
		t.Fatal("a passed-over entry was refused while the sample had room")
	}
	if !sampler.Full() || sampler.NetworkCount(fleetNetworkId) != 2 {
		t.Fatalf("full %t, the fleet holds %d, want full with 2", sampler.Full(), sampler.NetworkCount(fleetNetworkId))
	}
	if sampler.Admits(otherNetworkId) || sampler.TakeOver(otherNetworkId) {
		t.Fatal("a full sample took one more")
	}
}

// count active extenders of networkId, each with one active tcp address of a
// family on documentation addresses unique across the fixture's networks.
func testSampleNetworkExtenders(ctx context.Context, name string, networkId server.Id, count int) []server.Id {
	extenderIds := []server.Id{}
	nameHash := sha256.Sum256([]byte(name))
	for i := range count {
		ip := fmt.Sprintf("2001:db8:%x:%x::%x", nameHash[0], nameHash[1], i+1)
		extender := &NetworkExtender{
			ExtenderId:  server.NewId(),
			NetworkId:   networkId,
			ClientId:    server.NewId(),
			PublicKey:   []byte(fmt.Sprintf("extender-sample-%s-%d", name, i)),
			CreateTime:  server.NowUtc(),
			TcpPort:     443,
			UdpPort:     443,
			DnsPort:     53,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: "us",
			Active:      true,
		}
		Testing_CreateNetworkExtender(ctx, extender, []*NetworkExtenderAddress{testExtenderCacheAddress(ip, true)})
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	return extenderIds
}

// How many extenders of each network a bootstrap holds.
func testSampleNetworkCounts(sample []*NetworkExtenderWithAddresses) map[server.Id]int {
	counts := map[server.Id]int{}
	for _, entry := range sample {
		counts[entry.Extender.NetworkId] += 1
	}
	return counts
}

// An activation's bootstrap holds a fleet that has most of the directory to its
// quota, two of eight, whenever the other networks can fill the rest, in every
// draw. Before, a bootstrap was eight extenders drawn at random from the whole
// directory, so a fleet of 40 beside 6 others held about seven of them.
func TestActivationBootstrapHoldsEachNetworkToItsQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const bootstrapCount = 8
		quota := DefaultExtenderSampleSettings().NetworkQuota(bootstrapCount)

		fleetNetworkId := server.NewId()
		testSampleNetworkExtenders(ctx, "fleet", fleetNetworkId, 40)
		otherNetworkIds := []server.Id{}
		for i := range 6 {
			otherNetworkId := server.NewId()
			otherNetworkIds = append(otherNetworkIds, otherNetworkId)
			testSampleNetworkExtenders(ctx, fmt.Sprintf("other%d", i), otherNetworkId, 1)
		}

		for draw := range 20 {
			sample := GetRandomActiveNetworkExtendersUnderNetworkQuota(ctx, bootstrapCount, quota, server.Id{})
			counts := testSampleNetworkCounts(sample)
			if len(sample) != bootstrapCount || counts[fleetNetworkId] != quota {
				t.Fatalf("draw %d: %d extenders, the fleet holds %d, want %d with the fleet at its quota of %d", draw, len(sample), counts[fleetNetworkId], bootstrapCount, quota)
			}
			for _, otherNetworkId := range otherNetworkIds {
				if counts[otherNetworkId] != 1 {
					t.Fatalf("draw %d: a network of one extender holds %d", draw, counts[otherNetworkId])
				}
			}
		}

		// a quota of the whole sample is no cap: the draw is the whole
		// directory's, as GetRandomActiveNetworkExtenders draws
		if sample := GetRandomActiveNetworkExtendersUnderNetworkQuota(ctx, 46, 46, server.Id{}); len(sample) != 46 {
			t.Fatalf("an uncapped draw of the whole directory holds %d, want 46", len(sample))
		}
	})
}

// Where too few other networks have candidates to fill the bootstrap under the
// quota, the fleet fills the rest: the bootstrap is as full as it was without
// the quota, and every other network's extender is in it.
func TestActivationBootstrapFillsFromAFleetWhenTooFewOtherNetworksExist(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const bootstrapCount = 8
		quota := DefaultExtenderSampleSettings().NetworkQuota(bootstrapCount)

		fleetNetworkId := server.NewId()
		testSampleNetworkExtenders(ctx, "fleet", fleetNetworkId, 20)
		otherNetworkId := server.NewId()
		testSampleNetworkExtenders(ctx, "other", otherNetworkId, 1)

		for draw := range 10 {
			sample := GetRandomActiveNetworkExtendersUnderNetworkQuota(ctx, bootstrapCount, quota, server.Id{})
			counts := testSampleNetworkCounts(sample)
			if len(sample) != bootstrapCount || counts[otherNetworkId] != 1 || counts[fleetNetworkId] != bootstrapCount-1 {
				t.Fatalf("draw %d: %d extenders, the other network %d, the fleet %d; want 8 with the other network's one and the fleet's 7", draw, len(sample), counts[otherNetworkId], counts[fleetNetworkId])
			}
		}
	})
}
