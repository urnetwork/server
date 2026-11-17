package perfvar

import "testing"

// Owners counts public provider/peer/companion/stream keys, not internal send-sequence owners.
type privateSdkDefaultContractWitnessResult struct {
	Owners    int
	Dropped   int
	Overflows int
	Status    string
}

// A signed public opening and actual subsequent credit usage are observable.
// Same-sequence identity, pending debt, exact insufficiency and unexpired switching are not.
func privateSdkDefaultContractWitness(rows []privateSdkDefaultContractRow, dropped, overflows int) privateSdkDefaultContractWitnessResult {
	result := privateSdkDefaultContractWitnessResult{Dropped: dropped, Overflows: overflows, Status: "UNEXERCISED_PUBLIC_PRODUCTION_OPENING_AND_USAGE"}
	if dropped != 0 || overflows != 0 {
		result.Status = "PUBLIC_CONTRACT_OBSERVATION_INCOMPLETE"
		return result
	}
	type key struct {
		provider, peer    int
		companion, stream bool
	}
	grants := map[int]privateSdkDefaultContractRow{}
	first := map[key]int{}
	usage := map[int]privateSdkDefaultContractRow{}
	for _, row := range rows {
		if row.Stage == "signed_grant" && row.Contract > 0 && row.Peer > 0 && row.SignaturePresent && !row.CallbackError && row.Granted > 0 {
			if prior, exists := grants[row.Contract]; exists {
				if prior.Provider != row.Provider || prior.Peer != row.Peer || prior.Granted != row.Granted || prior.ForceStream != row.ForceStream {
					result.Status = "PUBLIC_CONTRACT_OBSERVATION_INCONSISTENT"
					return result
				}
				continue
			}
			grants[row.Contract] = row
		}
		if row.Stage == "public_contract_usage" || row.Stage == "final_contract_usage" {
			if prior, exists := usage[row.Contract]; !exists || prior.StatsSequence < row.StatsSequence {
				usage[row.Contract] = row
			}
		}
	}
	// Observation order describes first public grant, never first internal contract take.
	for _, row := range rows {
		grant, granted := grants[row.Contract]
		used, observed := usage[row.Contract]
		if row.Stage != "signed_grant" || !granted || !observed || used.StatsSequence == 0 || used.Provider != grant.Provider || used.Peer != grant.Peer || used.Granted != grant.Granted || used.ForceStream != grant.ForceStream {
			continue
		}
		k := key{provider: used.Provider, peer: used.Peer, companion: used.Companion, stream: used.ForceStream}
		if _, exists := first[k]; !exists {
			first[k] = row.Contract
		}
	}
	for k, openingId := range first {
		opening, openingUsage := grants[openingId], usage[openingId]
		if opening.Granted != 1024*1024 || openingUsage.Used <= 0 {
			continue
		}
		var total int64
		usedContracts := 0
		for id, used := range usage {
			grant, ok := grants[id]
			if !ok || used.StatsSequence == 0 || used.Used <= 0 || used.Granted != grant.Granted || used.Provider != grant.Provider || used.Peer != grant.Peer || used.ForceStream != grant.ForceStream {
				continue
			}
			if (key{provider: used.Provider, peer: used.Peer, companion: used.Companion, stream: used.ForceStream}) == k {
				total += used.Used
				usedContracts++
			}
		}
		if usedContracts >= 2 && total > opening.Granted {
			result.Owners++
		}
	}
	if result.Owners > 0 {
		result.Status = "EXERCISED_PUBLIC_PRODUCTION_OPENING_AND_USAGE"
	}
	return result
}

func privateSdkPublicOpeningRows() []privateSdkDefaultContractRow {
	const credit = 1024 * 1024
	return []privateSdkDefaultContractRow{
		{Stage: "signed_grant", Provider: 0, Peer: 1, Contract: 1, Granted: credit, SignaturePresent: true},
		{Stage: "signed_grant", Provider: 0, Peer: 1, Contract: 2, Granted: credit, SignaturePresent: true},
		{Stage: "final_contract_usage", Provider: 0, Peer: 1, Contract: 1, Granted: credit, Used: 800000, StatsSequence: 3},
		{Stage: "public_contract_usage", Provider: 0, Peer: 1, Contract: 2, Granted: credit, Used: 700000, StatsSequence: 2},
	}
}

func TestPrivateSdkPublicOpeningRequiresActualUsageBeyondOpening(t *testing.T) {
	for _, kind := range []string{"valid", "prefetch_only", "opening_unused", "below_opening", "foreign_peer", "foreign_provider", "wrong_companion", "wrong_stream", "unsigned", "failed_callback", "not1MiB", "unsequenced_usage"} {
		rows := privateSdkPublicOpeningRows()
		switch kind {
		case "prefetch_only":
			rows[3].Used = 0
		case "opening_unused":
			rows[2].Used = 0
		case "below_opening":
			rows[3].Used = 1
		case "foreign_peer":
			rows[3].Peer = 2
		case "foreign_provider":
			rows[3].Provider = 2
		case "wrong_companion":
			rows[3].Companion = true
		case "wrong_stream":
			rows[3].ForceStream = true
		case "unsigned":
			rows[0].SignaturePresent = false
		case "failed_callback":
			rows[0].CallbackError = true
		case "not1MiB":
			rows[0].Granted = 2 * 1024 * 1024
			rows[2].Granted = 2 * 1024 * 1024
		case "unsequenced_usage":
			rows[3].StatsSequence = 0
		}
		witness := privateSdkDefaultContractWitness(rows, 0, 0)
		want := 0
		if kind == "valid" {
			want = 1
		}
		if witness.Owners != want {
			t.Fatalf("%s public opening keys=%d want%d status=%s", kind, witness.Owners, want, witness.Status)
		}
	}
}

func TestPrivateSdkPublicOpeningDeduplicatesCumulativeUsage(t *testing.T) {
	rows := privateSdkPublicOpeningRows()
	rows[2].Used, rows[3].Used = 300000, 400000
	rows = append(rows, rows[2], rows[3])
	if witness := privateSdkDefaultContractWitness(rows, 0, 0); witness.Owners != 0 {
		t.Fatal("duplicate cumulative snapshots manufactured opening usage", witness)
	}
	rows = privateSdkPublicOpeningRows()
	stale := rows[3]
	stale.StatsSequence = 1
	stale.Used = 0
	rows = append(rows, stale)
	if witness := privateSdkDefaultContractWitness(rows, 0, 0); witness.Owners != 1 {
		t.Fatal("stale public stats regressed the latest cumulative usage", witness)
	}
}

func TestPrivateSdkPublicOpeningRejectsObservationLoss(t *testing.T) {
	for _, dropped := range []int{0, 1} {
		witness := privateSdkDefaultContractWitness(privateSdkPublicOpeningRows(), dropped, 1-dropped)
		if witness.Owners != 0 || witness.Status != "PUBLIC_CONTRACT_OBSERVATION_INCOMPLETE" {
			t.Fatal("incomplete public evidence was accepted", witness)
		}
	}
}
