package controller

// The census rules of capture the flag (sn CAPTURETHEFLAG.md §4.2, §4.3) as a
// pure function of what the census step gathered: the epoch's presence, the
// end-boundary facts of every miner the samples found, the wallets, the fleet
// bindings and the operator's own networks. ctf_census.go gathers them.
//
// Every miner the samples found in a role during the epoch is a member, and
// every rule is evaluated for it, so a member records each rule it fails: the
// census counts it under the lowest-numbered one (sn ctf.Reason), and a near
// miss is a member that failed only presence or a change of country.
//
//	providers                                   extenders
//	P1  2 offline at the end, 3 not Public,     X1  1 revoked or inactive at the end
//	    4 no valid location
//	P2  10 presence                             X2  10 presence
//	P3  9 country changed                       X3  9 country changed, an activation
//	                                                elsewhere in the epoch
//	P4  8 duplicate address                     X4  8 duplicate (E3)
//	P5  7 no wallet, 6 head-bound               X5  7 no wallet, 6 head-bound (E4)
//	P6  5 own network                           X6  5 own network (E5)
//
// An extender's X1 and X4–X6 are the extender pool's E1 and E3–E5 (§4.2),
// judged once for both pools by the shared eligibility rules
// (model/extender_eligibility.go, extender_eligibility.go): its member is
// judged by its provider client's judged record, and E3 decides its
// duplicates from every active record of the epoch, not from this census's
// candidates.
//
// A member without a unit has no prefix to enter a draw with, which the
// census counts as no valid location (4): a valid location needs an address
// hash per family (§7).

import (
	"cmp"
	"maps"
	"slices"

	"github.com/urfoundation/sn/v2026/ctf"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A provider's facts at the end boundary (P1, P3–P6).
type ctfProviderEnd struct {
	NetworkId *server.Id
	// network_client.active, top-level (no source client), and
	// network_client_location_reliability.connected and .valid
	Active        bool
	TopLevel      bool
	Connected     bool
	ValidLocation bool
	// in the score export's Online set: ClientScore.Online
	Online bool
	// holds a Public provide key
	Public bool
	// the country it is published under, normalized
	CountryCode string
	Egress      *model.CtfEgress
}

// An extender's facts at the end boundary (X1, X3–X6): its provider client's
// judged record under the shared eligibility rules.
type ctfExtenderEnd struct {
	ExtenderId server.Id
	NetworkId  *server.Id
	// E1: active and unrevoked with at least one active address
	Active bool
	// E3: another active record of its component counts
	Duplicate bool
	// E4: an active fleet binding at either boundary, and the payout
	// coldkey, zero without one
	HeadBound bool
	Coldkey   [32]byte
	// E5: gated, a canary, or on another network the operator controls
	OwnNetwork bool
	// the record's country: its derived location's, else its activation's
	CountryCode string
	// a country of an activation of the record in the epoch other than
	// CountryCode
	ActivatedElsewhere bool
	Egress             *model.CtfEgress
}

// Everything the census of one epoch is decided from.
type ctfCensusFacts struct {
	Excused  model.CtfSampleSet
	Required int
	// the epoch's presence, any order
	Presences []*model.CtfPresence
	// by client id, for the provider members; a member without facts failed
	// every P1 condition
	Providers map[server.Id]*ctfProviderEnd
	// by provider client id, for the extender members; a member without facts
	// is inactive
	Extenders map[server.Id]*ctfExtenderEnd
	// the operator's own networks (P6); an extender's are in its facts (X6)
	OwnNetworks map[server.Id]bool
	// clients with an active fleet binding at either boundary (P5); an
	// extender's is in its facts (X5)
	HeadBound map[server.Id]bool
	// payout coldkeys under the provider pool's wallet selection (P5); an
	// extender's is in its facts (X5)
	Coldkeys map[server.Id][32]byte
}

// A census as the rules decide it, before it is encoded.
type ctfCensusDecision struct {
	// every member, by role and client id
	Members []*model.CtfCensusMember
	// all 498 country and role counts, by country and role
	Pots []*model.CtfCensusPot
	// the exclusion rows, by country, role and reason
	Exclusions []*model.CtfCensusExclusion
}

// The candidates of a decision, by role and client id.
func (self *ctfCensusDecision) candidates() []*model.CtfCensusMember {
	candidates := []*model.CtfCensusMember{}
	for _, member := range self.Members {
		if member.Candidate {
			candidates = append(candidates, member)
		}
	}
	return candidates
}

func ctfFailed(failed uint32, reason int) uint32 {
	return failed | 1<<reason
}

// The lowest-numbered reason of a set of failed rules, 0 for none.
func ctfLowestReason(failed uint32) int {
	for reason := 1; reason <= model.CtfNumReasons; reason++ {
		if failed&(1<<reason) != 0 {
			return reason
		}
	}
	return 0
}

// A member's presence in each country.
type ctfMemberPresence struct {
	// country code -> samples
	countries map[string]*model.CtfSampleSet
}

// The country a member was present in most, ties to the lowest code, and how
// many samples not excused found it there.
func (self *ctfMemberPresence) best(excused *model.CtfSampleSet) (string, int) {
	countryCode, samples := "", 0
	for _, code := range slices.Sorted(maps.Keys(self.countries)) {
		if n := self.countries[code].CountExcept(excused); countryCode == "" || samples < n {
			countryCode, samples = code, n
		}
	}
	return countryCode, samples
}

// The samples not excused that found the member present in a country.
func (self *ctfMemberPresence) in(countryCode string, excused *model.CtfSampleSet) int {
	if samples, ok := self.countries[countryCode]; ok {
		return samples.CountExcept(excused)
	}
	return 0
}

// ctfCountryInL reports whether a normalized country code is one of the 249
// (sn ctf, the list L).
func ctfCountryInL(countryCode string) bool {
	country, ok := ctf.ParseCountry(countryCode)
	return ok && country.String() == countryCode
}

// The 249 codes of L, in code order.
func ctfCountryCodes() []string {
	countries := ctf.Countries()
	codes := make([]string, len(countries))
	for i, country := range countries {
		codes[i] = country.String()
	}
	return codes
}

// The presence rules (P2, P3, X2, X3) on a member's end country: present in it
// for the required samples, else a change of country when another country had
// them, else too little presence.
func ctfPresenceFailures(presence *ctfMemberPresence, countryCode string, facts *ctfCensusFacts) (failed uint32, present int) {
	present = presence.in(countryCode, &facts.Excused)
	if present >= facts.Required {
		return 0, present
	}
	if bestCountry, bestSamples := presence.best(&facts.Excused); bestSamples >= facts.Required && bestCountry != countryCode {
		return 1 << model.CtfReasonCountryChanged, present
	}
	return 1 << model.CtfReasonPresence, present
}

// decideCtfCensus applies the census rules to an epoch's facts.
func decideCtfCensus(facts *ctfCensusFacts) *ctfCensusDecision {
	type memberKey struct {
		role     int
		clientId server.Id
	}
	presences := map[memberKey]*ctfMemberPresence{}
	for _, presence := range facts.Presences {
		key := memberKey{role: presence.Role, clientId: presence.ClientId}
		member := presences[key]
		if member == nil {
			member = &ctfMemberPresence{countries: map[string]*model.CtfSampleSet{}}
			presences[key] = member
		}
		samples := presence.Samples
		if existing := member.countries[presence.CountryCode]; existing != nil {
			for i := range samples {
				samples[i] |= existing[i]
			}
		}
		member.countries[presence.CountryCode] = &samples
	}
	keys := slices.SortedFunc(maps.Keys(presences), func(a, b memberKey) int {
		return cmp.Or(cmp.Compare(a.role, b.role), a.clientId.Cmp(b.clientId))
	})

	// every rule but the duplicate address, for every member, by role and
	// client id
	decision := &ctfCensusDecision{}
	addresses := map[*model.CtfCensusMember][][32]byte{}
	for _, key := range keys {
		member := &model.CtfCensusMember{Role: key.role, ClientId: key.clientId}
		var failed uint32
		var egress *model.CtfEgress
		headBound, coldkey := facts.HeadBound[key.clientId], facts.Coldkeys[key.clientId]
		switch key.role {
		case model.CtfRoleProvider:
			end := facts.Providers[key.clientId]
			if end == nil {
				end = &ctfProviderEnd{}
			}
			member.NetworkId = end.NetworkId
			member.CountryCode = model.CtfNormalizeCountryCode(end.CountryCode)
			egress = end.Egress
			if !(end.Active && end.TopLevel && end.Connected && end.Online) {
				failed = ctfFailed(failed, model.CtfReasonOffline)
			}
			if !end.Public {
				failed = ctfFailed(failed, model.CtfReasonNotPublic)
			}
			if !end.ValidLocation || !ctfCountryInL(member.CountryCode) {
				failed = ctfFailed(failed, model.CtfReasonNoLocation)
			}
			if end.NetworkId != nil && facts.OwnNetworks[*end.NetworkId] {
				failed = ctfFailed(failed, model.CtfReasonOwnNetwork)
			}
		case model.CtfRoleExtender:
			end := facts.Extenders[key.clientId]
			if end == nil {
				end = &ctfExtenderEnd{}
			} else {
				extenderId := end.ExtenderId
				member.ExtenderId = &extenderId
			}
			member.NetworkId = end.NetworkId
			member.CountryCode = model.CtfNormalizeCountryCode(end.CountryCode)
			egress = end.Egress
			if !end.Active {
				failed = ctfFailed(failed, model.CtfReasonRevoked)
			}
			if !ctfCountryInL(member.CountryCode) {
				failed = ctfFailed(failed, model.CtfReasonNoLocation)
			}
			if end.OwnNetwork {
				failed = ctfFailed(failed, model.CtfReasonOwnNetwork)
			}
			if end.ActivatedElsewhere {
				failed = ctfFailed(failed, model.CtfReasonCountryChanged)
			}
			// X4 is E3, decided from every active record of the epoch
			if end.Active && end.Duplicate {
				failed = ctfFailed(failed, model.CtfReasonDuplicate)
			}
			headBound, coldkey = end.HeadBound, end.Coldkey
		}
		if egress == nil || egress.Unit == nil || (egress.UnitFamily != 4 && egress.UnitFamily != 6) {
			failed = ctfFailed(failed, model.CtfReasonNoLocation)
		} else {
			unit := *egress.Unit
			member.Unit = &unit
			member.UnitFamily = egress.UnitFamily
			addresses[member] = egress.Addresses
		}
		if headBound {
			failed = ctfFailed(failed, model.CtfReasonHeadBound)
		}
		if coldkey != ([32]byte{}) {
			member.Coldkey = &coldkey
		} else {
			failed = ctfFailed(failed, model.CtfReasonNoWallet)
		}
		presenceFailed, present := ctfPresenceFailures(presences[key], member.CountryCode, facts)
		member.Failed = failed | presenceFailed
		member.PresentSamples = present
		// a country outside the 249 is none, as the census counts it
		if !ctfCountryInL(member.CountryCode) {
			member.CountryCode = model.CtfCountryUnknown
		}
		decision.Members = append(decision.Members, member)
	}

	// A unit hash is one prefix's, so it has one family in a census, in either
	// role. The lowest member that passes the rules so far fixes it; another
	// that names it with the other family has no valid unit. (A keyed hash of
	// a 4-byte prefix never equals one of a 16-byte prefix: this only guards
	// against a misread family.)
	families := map[[32]byte]int{}
	for _, member := range decision.Members {
		if member.Failed != 0 {
			continue
		}
		if family, ok := families[*member.Unit]; ok && family != member.UnitFamily {
			member.Failed = ctfFailed(member.Failed, model.CtfReasonNoLocation)
			continue
		}
		families[*member.Unit] = member.UnitFamily
	}

	// P4: one provider candidate per keyed IPv4 address or IPv6 /64, the
	// lowest client id. A member sharing an address with a lower candidate is
	// a duplicate whatever else it fails; one that fails nothing else and
	// shares none is a candidate and holds its addresses. (An extender's X4 is
	// E3, above.)
	held := map[int]map[[32]byte]bool{model.CtfRoleProvider: {}}
	for _, member := range decision.Members {
		if member.Role != model.CtfRoleProvider {
			continue
		}
		for _, address := range addresses[member] {
			if held[member.Role][address] {
				member.Failed = ctfFailed(member.Failed, model.CtfReasonDuplicate)
				break
			}
		}
		if member.Failed == 0 {
			for _, address := range addresses[member] {
				held[member.Role][address] = true
			}
		}
	}

	type potKey struct {
		countryCode string
		role        int
	}
	pots := map[potKey]*model.CtfCensusPot{}
	for _, countryCode := range ctfCountryCodes() {
		for _, role := range []int{model.CtfRoleProvider, model.CtfRoleExtender} {
			pot := &model.CtfCensusPot{CountryCode: countryCode, Role: role}
			pots[potKey{countryCode: countryCode, role: role}] = pot
			decision.Pots = append(decision.Pots, pot)
		}
	}
	// A unit enters one draw per role: its lowest candidate client id's
	// country (§4.2), the first candidate of the unit in client id order.
	type unitKey struct {
		role int
		unit [32]byte
	}
	draws := map[unitKey]string{}
	type exclusionKey struct {
		countryCode string
		role        int
		reason      int
	}
	exclusions := map[exclusionKey]int{}
	for _, member := range decision.Members {
		member.Reason = ctfLowestReason(member.Failed)
		member.Candidate = member.Failed == 0
		if member.Candidate {
			key := unitKey{role: member.Role, unit: *member.Unit}
			drawCountryCode, ok := draws[key]
			if !ok {
				drawCountryCode = member.CountryCode
				draws[key] = drawCountryCode
				pots[potKey{countryCode: drawCountryCode, role: member.Role}].Units += 1
			}
			member.DrawCountryCode = drawCountryCode
			pots[potKey{countryCode: drawCountryCode, role: member.Role}].Candidates += 1
			continue
		}
		if pot := pots[potKey{countryCode: member.CountryCode, role: member.Role}]; pot != nil && member.NearMiss() {
			pot.NearMisses += 1
		}
		countryCode := member.CountryCode
		if !ctfCountryInL(countryCode) {
			countryCode = model.CtfCountryUnknown
		}
		exclusions[exclusionKey{countryCode: countryCode, role: member.Role, reason: member.Reason}] += 1
	}
	for key, count := range exclusions {
		decision.Exclusions = append(decision.Exclusions, &model.CtfCensusExclusion{CountryCode: key.countryCode, Role: key.role, Reason: key.reason, Count: count})
	}
	slices.SortFunc(decision.Exclusions, func(a, b *model.CtfCensusExclusion) int {
		return cmp.Or(cmp.Compare(a.CountryCode, b.CountryCode), cmp.Compare(a.Role, b.Role), cmp.Compare(a.Reason, b.Reason))
	})
	return decision
}
