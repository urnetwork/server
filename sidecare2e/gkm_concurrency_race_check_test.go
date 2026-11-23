//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The gkm concurrency case's exact-race check (gkmConcurrencyExactRace) over rounds built from requests as the
// recorder at gkm's origin keeps them, with no world: each shape a round of the growth phase's race can take, and the
// verdict §10.7's KF6 gives it. The shapes come from the case's runs: the staging a claim overtook
// (e2e-headers-final's, fixed by sidecar's TestGkmRotateTakesTheWinnerWhenItsStagingMeetsTheClaim) and the rotation
// that began once its key client held the round's generation (e2e-all-rerun's, the case's own timing).

package sidecare2e

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// One request of a round as the recorder keeps it: a rotator's STAGED or LATE delivery of a generation, or its claim
// of one, gkm taking the claim or answering 412.
type gkmConcurrencyRaceRequest struct {
	uploader   int
	generation uint64
	kind       sidecar.GkmDeliveryKind
	claim      bool
	taken      bool
	// a request that reached gkm's origin before the release
	early bool
}

// A shape of one round from generation 2 and its verdict.
type gkmConcurrencyRaceShape struct {
	name string
	// the round's start: gkm's newest generation, which every rotator held
	from     uint64
	requests []gkmConcurrencyRaceRequest
	// what each rotator returned, 0 for none
	returned []uint64
	// how many generations the round took, how many late rotations it holds, and a fragment each problem must hold
	// in turn, none for a round that passes
	took      int
	lateCount int
	problems  []string
}

func TestE2eGkmConcurrencyExactRaceHoldsEachRotatorToWhatItRacedFor(t *testing.T) {
	groupId := sidecar.NewId()
	repoId := sidecar.NewId()
	rotators := []*gkmConcurrencyDevice{}
	for _, name := range []string{"a-1", "a-2", "a-3", "a-4"} {
		rotators = append(rotators, &gkmConcurrencyDevice{name: name, sidecarId: sidecar.NewId()})
	}
	// a device that is no rotator of the round, uploader 4
	outsider := sidecar.NewId()
	uploaderId := func(uploader int) sidecar.Id {
		if uploader < len(rotators) {
			return rotators[uploader].sidecarId
		}
		return outsider
	}
	commitment := func(generation uint64) []byte {
		return bytes.Repeat([]byte{byte(generation)}, 32)
	}
	rotate := func(generation uint64) *protocol.GenRotate {
		return &protocol.GenRotate{
			GroupId: groupId.Bytes(), RepoId: repoId.Bytes(), Generation: generation,
			Commitment: commitment(generation), ParentCommitment: commitment(generation - 1),
		}
	}
	staged := func(uploader int, generation uint64) gkmConcurrencyRaceRequest {
		return gkmConcurrencyRaceRequest{uploader: uploader, generation: generation, kind: sidecar.GkmDeliveryStaged}
	}
	late := func(uploader int, generation uint64) gkmConcurrencyRaceRequest {
		return gkmConcurrencyRaceRequest{uploader: uploader, generation: generation, kind: sidecar.GkmDeliveryLate}
	}
	claim := func(uploader int, generation uint64, taken bool) gkmConcurrencyRaceRequest {
		return gkmConcurrencyRaceRequest{uploader: uploader, generation: generation, claim: true, taken: taken}
	}

	shapes := []gkmConcurrencyRaceShape{
		{
			// a-1 wins; a-2 loses at the claim; a-3's staging meets the claim and takes the winner; a-4's first pass
			// finds the winner standing
			name: "one race", from: 2,
			requests: []gkmConcurrencyRaceRequest{
				staged(0, 3), staged(1, 3), claim(0, 3, true), claim(1, 3, false), staged(2, 3),
			},
			returned: []uint64{3, 3, 3, 3}, took: 1,
		},
		{
			// e2e-all-rerun's round: a-4 delivers generation 2 and then 3 late to a joiner, its rotation waiting on that
			// flow, which took 3, so it drafts and claims 4 from it
			name: "a rotation begun from the round's generation", from: 2,
			requests: []gkmConcurrencyRaceRequest{
				staged(0, 3), staged(1, 3), late(3, 2), claim(0, 3, true), staged(2, 3), claim(1, 3, false),
				late(3, 3), staged(3, 4), claim(3, 4, true),
			},
			returned: []uint64{3, 3, 3, 4}, took: 2, lateCount: 1,
		},
		{
			// two rotations that began once their clients held 3 race on 4 in turn
			name: "two rotations begun from the round's generation", from: 2,
			requests: []gkmConcurrencyRaceRequest{
				staged(0, 3), claim(0, 3, true), staged(1, 3), claim(1, 3, false), staged(2, 4), staged(3, 4),
				claim(2, 4, true), claim(3, 4, false),
			},
			returned: []uint64{3, 3, 4, 4}, took: 2, lateCount: 1,
		},
		{
			// a loser whose staging of 3 met the claim and that rotated on to 4
			name: "a loser rotating on", from: 2,
			requests: []gkmConcurrencyRaceRequest{
				staged(0, 3), staged(1, 3), claim(0, 3, true), claim(1, 3, false), staged(3, 3), staged(3, 4),
				claim(3, 4, true),
			},
			returned: []uint64{3, 3, 3, 4}, took: 2,
			problems: []string{"which raced for generation 3 first", "a-4 raced for shared's generation 3 and returned 4", "a-4 raced for 3 and returned 4"},
		},
		{
			// e2e-all-rerun's next round, before every rotator held the newest generation at the release: a-3 and a-4
			// still held 3, found 4 standing and took it
			name: "a rotator taking a generation from before the round", from: 4,
			requests: []gkmConcurrencyRaceRequest{
				staged(0, 5), staged(1, 5), claim(0, 5, true), claim(1, 5, false),
			},
			returned: []uint64{5, 5, 4, 4}, took: 1,
			problems: []string{"a-3 raced for nothing and returned shared's generation 4", "a-4 raced for nothing and returned shared's generation 4", "a-3 raced for nothing and returned 4"},
		},
		{
			// a claim of no rotator in the round's window: an admission or a grant rotating
			name: "a generation no rotator claimed", from: 2,
			requests: []gkmConcurrencyRaceRequest{
				staged(0, 3), claim(0, 3, true), staged(1, 3), claim(1, 3, false), claim(4, 4, true),
			},
			returned: []uint64{3, 3, 3, 3}, took: 2,
			problems: []string{"gkm took generation 4 from sidecar", "a-1 raced for 3 and returned 3"},
		},
		{
			// the round's generation taken before the release
			name: "a claim before the release", from: 2,
			requests: []gkmConcurrencyRaceRequest{
				{uploader: 0, generation: 3, claim: true, taken: true, early: true}, staged(1, 3), claim(1, 3, false),
			},
			returned: []uint64{3, 3, 3, 3}, took: 1,
			problems: []string{"gkm took generation 3 from a-1", "a-1 raced for nothing and returned 3"},
		},
		{
			// no rotator's claim taken: the round took nothing
			name: "no generation taken", from: 2,
			requests: []gkmConcurrencyRaceRequest{
				staged(0, 3), claim(0, 3, false),
			},
			returned: []uint64{0, 0, 0, 0}, took: 0,
			problems: []string{"took no generation", "a-1 raced for 3 and returned none"},
		},
	}
	for _, shape := range shapes {
		released := time.Date(2026, 11, 23, 5, 3, 1, 0, time.UTC)
		at := released
		// every generation through the round's start stands, each claimed before the release
		claims := []*gkmConcurrencyClaim{}
		for generation := uint64(1); generation <= shape.from; generation += 1 {
			claims = append(claims, &gkmConcurrencyClaim{
				time: released.Add(-time.Duration(shape.from-generation+1) * time.Second), uploader: outsider, groupId: groupId,
				repoId: repoId, rotate: rotate(generation), signerId: outsider, status: 201,
			})
		}
		deliveries := []*gkmConcurrencyDelivery{}
		for _, request := range shape.requests {
			at = at.Add(10 * time.Millisecond)
			requestTime := at
			if request.early {
				requestTime = released.Add(-500 * time.Millisecond)
			}
			if request.claim {
				status := 412
				if request.taken {
					status = 201
				}
				claims = append(claims, &gkmConcurrencyClaim{
					time: requestTime, uploader: uploaderId(request.uploader), groupId: groupId, repoId: repoId,
					rotate: rotate(request.generation), signerId: uploaderId(request.uploader), status: status,
				})
				continue
			}
			deliveries = append(deliveries, &gkmConcurrencyDelivery{
				time: requestTime, uploader: uploaderId(request.uploader), groupId: groupId, generation: request.generation,
				body: &protocol.KeyDelivery{DeliveryKind: uint32(request.kind)}, signerId: uploaderId(request.uploader),
			})
		}
		generations, forks := gkmConcurrencyGenerations(claims, nil)
		if 0 < len(forks) {
			t.Fatalf("%s: the shape forks: %v", shape.name, forks)
		}
		round := &gkmConcurrencyExactRound{
			repoName: "shared", before: generations[groupId][shape.from], released: released, rotators: rotators,
			returnedGenerations: shape.returned, generations: generations[groupId], claims: claims, deliveries: deliveries,
		}
		for generation := range generations[groupId] {
			round.newest = max(round.newest, generation)
		}
		problems, took, lates := gkmConcurrencyExactRace(round)
		if took != shape.took {
			t.Errorf("%s: the round took %d generations, not %d", shape.name, took, shape.took)
		}
		if len(lates) != shape.lateCount {
			t.Errorf("%s: %d late rotations, not %d: %v", shape.name, len(lates), shape.lateCount, lates)
		}
		if len(shape.problems) == 0 && 0 < len(problems) {
			t.Errorf("%s: the round fails: %v", shape.name, problems)
		}
		if 0 < len(shape.problems) && len(problems) == 0 {
			t.Errorf("%s: the round passes", shape.name)
		}
		text := strings.Join(problems, "; ")
		for _, fragment := range shape.problems {
			if !strings.Contains(text, fragment) {
				t.Errorf("%s: no problem says %q: %s", shape.name, fragment, text)
			}
		}
	}
}
