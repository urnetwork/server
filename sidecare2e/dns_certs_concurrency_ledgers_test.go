//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The ledgers of the dns and certificates case (dns_certs_concurrency_test.go) beyond dns_changes_per_hour, each
// reconciled exactly at the case's end:
//
//   - certs' budget at LE ({tcb_letsencrypt_<root>}, certsservice's budget.go): every member a first order of the case's,
//     none a renewal's or a resumed order's; each network's first certificate to issue spent a unit, since its set had
//     none issued; and another first order of the same set spent one only if it was admitted before that certificate
//     issued, as a new set, its member's score, its admission's time, earlier than the issue (a later one is a
//     repeated set, which spends none);
//   - each challenge name's journal rows against the certs worker's writes, as the recorder in front of its DNS client
//     saw them (dns_certs_concurrency_recorders_test.go): every answered write committed once, at its serial, with its
//     values; every commit at the name an answered write's or the one landing of a write whose outcome was unknown;
//     every answered removal a delete at its serial; and the dns API's count in dns_challenges_per_name_hour's window,
//     certs' read of it for a custom domain's target too (model.GetDnsAcmeChallengeWindowInTx), those commits, under the
//     cap;
//   - dns_challenges_per_name_hour itself at each network's challenge name: written as certs writes, on its bearer,
//     each write answered until the name's commits in the window reach the cap, where the next is refused 429; the
//     removal, which nothing counts or refuses, answered then, and the edge answering no TXT there;
//   - api's write budget (§11.13, evsservice's reports): every P0 byte api's process reported against the first metadata
//     cluster is an object it holds under §7.9's and §7.10's prefixes, priced as a create, by kind (log for proj/ and
//     projrows/, revocation for rev/), and it reported no P1 or P2; read where the objects listed before and after the
//     report agree, since a report is a level at its time;
//   - Pebble's nonce refusals (§22.9's 5%, at random): counted for each client, and no client sent a nonce again after
//     an answer to it, or a POST with none.

package sidecare2e

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server/model"
)

// certs' budget at LE, exactly (see the file header).
func dnsCertsCheckCertsBudget(t testing.TB, ctx context.Context, stack *dnsCertsStack, publishers []*dnsCertsPublisher) {
	t.Helper()
	members, err := dnsCertsBudgetMembers(ctx, stack.World, certsservice.CertsCaLetsEncrypt, strings.TrimSuffix(stack.Keys.root, "."))
	if err != nil {
		t.Fatal(err)
	}
	// each spend by its order, scored by its admission
	spent := map[sidecar.Id]time.Time{}
	for member, score := range members {
		if member == certsservice.CertsBudgetMarker {
			continue
		}
		certId, err := sidecar.ParseId(member)
		if err != nil {
			t.Errorf("the certs budget holds %q, which is no order's id", member)
			continue
		}
		spent[certId] = time.UnixMilli(score).UTC()
	}
	firstOrders := map[sidecar.Id]*dnsCertsPublisher{}
	networkPublishers := map[*dnsCertsNetwork][]*dnsCertsPublisher{}
	for _, publisher := range publishers {
		if publisher.firstCertId == (sidecar.Id{}) {
			continue
		}
		firstOrders[publisher.firstCertId] = publisher
		networkPublishers[publisher.Network] = append(networkPublishers[publisher.Network], publisher)
	}
	for certId := range spent {
		if firstOrders[certId] == nil {
			t.Errorf("the certs budget holds %s, which is no first order of the case's: a renewal, a resumed order or a custom row spent", certId)
		}
	}
	for network, group := range networkPublishers {
		slices.SortFunc(group, func(a *dnsCertsPublisher, b *dnsCertsPublisher) int { return a.firstIssuedAt.Compare(b.firstIssuedAt) })
		earliest := group[0]
		lines := []string{}
		if _, ok := spent[earliest.firstCertId]; !ok {
			t.Errorf("%s's first certificate to issue, %s's %s at %s, spent no unit of the budget, with none of its set issued before", network.Name, earliest.Device.Name, earliest.firstCertId, earliest.firstIssuedAt.Format(time.RFC3339Nano))
		}
		for _, publisher := range group {
			admitted, ok := spent[publisher.firstCertId]
			if !ok {
				lines = append(lines, fmt.Sprintf("%s's issued at %s spent none", publisher.Device.Name, publisher.firstIssuedAt.UTC().Format(time.StampMilli)))
				continue
			}
			lines = append(lines, fmt.Sprintf("%s's admitted at %s and issued at %s spent one", publisher.Device.Name, admitted.Format(time.StampMilli), publisher.firstIssuedAt.UTC().Format(time.StampMilli)))
			if publisher != earliest && !admitted.Before(earliest.firstIssuedAt) {
				t.Errorf("%s's first order %s spent a unit as a new set at its admission at %s, after %s's certificate of the same set issued at %s", publisher.Device.Name, publisher.firstCertId, admitted.Format(time.RFC3339Nano), earliest.Device.Name, earliest.firstIssuedAt.Format(time.RFC3339Nano))
			}
		}
		t.Logf("the certs budget for %s's set: %s", network.Name, strings.Join(lines, "; "))
	}
}

// One commit or removal of a challenge in the journal, and whether a write of certs' accounted for it.
type dnsCertsChallengeCommit struct {
	Serial uint64
	// sorted; none for a removal
	Values    []string
	accounted bool
}

// Each challenge name's journal rows against the certs worker's writes, and the dns API's count in the cap's window
// (see the file header). Run before anything but the certs worker writes a challenge.
func dnsCertsCheckChallengeCounts(t testing.TB, ctx context.Context, stack *dnsCertsStack, settings *dnsCertsSettings, records *dnsCertsRecords) {
	t.Helper()
	root := records.Root
	rows, err := dnsCertsJournal(ctx, root, 0, settings.JournalPageCount)
	if err != nil {
		t.Fatal(err)
	}
	isChallengeOwner := func(owner string) bool {
		return strings.HasPrefix(owner, model.DnsChallengeLabel+".") || dnsCertsIsAcmeOwner(owner, root)
	}
	commits := map[string][]*dnsCertsChallengeCommit{}
	removals := map[string][]*dnsCertsChallengeCommit{}
	for _, row := range rows {
		for _, upsert := range row.Upserts {
			if upsert.Kind != dnsservice.DnsRecordKindChallenge || upsert.Type != dns.TypeTXT {
				continue
			}
			values, err := dnsCertsRdataValues(upsert)
			if err != nil {
				t.Errorf("serial %d: %v", row.Serial, err)
				continue
			}
			owner := dns.CanonicalName(upsert.Owner)
			commits[owner] = append(commits[owner], &dnsCertsChallengeCommit{Serial: row.Serial, Values: values})
		}
		for _, deleted := range row.Deletes {
			owner := dns.CanonicalName(deleted.Owner)
			if deleted.Type == dns.TypeTXT && isChallengeOwner(owner) {
				removals[owner] = append(removals[owner], &dnsCertsChallengeCommit{Serial: row.Serial})
			}
		}
	}
	writes := map[string][]*dnsCertsChallengeWrite{}
	for _, write := range stack.Dns.Writes() {
		owner := dnsCertsChallengeOwner(write.Name, root)
		writes[owner] = append(writes[owner], write)
	}
	owners := map[string]bool{}
	for owner := range commits {
		owners[owner] = true
	}
	for owner := range removals {
		owners[owner] = true
	}
	for owner := range writes {
		owners[owner] = true
	}
	windowStart := time.Now().Add(-settings.ChallengeWindow)
	for _, owner := range slices.Sorted(maps.Keys(owners)) {
		ends := map[dnsCertsChallengeEnd]int{}
		unknownUpserts := []*dnsCertsChallengeWrite{}
		unknownRemovals := 0
		for _, write := range writes[owner] {
			ends[write.End] += 1
			switch {
			case write.End == dnsCertsChallengeUnknown && len(write.Values) == 0:
				unknownRemovals += 1
			case write.End == dnsCertsChallengeUnknown:
				unknownUpserts = append(unknownUpserts, write)
			case write.End != dnsCertsChallengeAnswered:
			case len(write.Values) == 0 && write.Serial == 0:
				// a removal of nothing
			case len(write.Values) == 0:
				removal := dnsCertsFindCommit(removals[owner], write.Serial)
				if removal == nil || removal.accounted {
					t.Errorf("certs' removal at %s was answered at serial %d, which the journal holds no delete of %s at, or another answer named", owner, write.Serial, owner)
					continue
				}
				removal.accounted = true
			default:
				commit := dnsCertsFindCommit(commits[owner], write.Serial)
				if commit == nil || commit.accounted || !slices.Equal(commit.Values, write.Values) {
					t.Errorf("certs' write of %v at %s was answered at serial %d, and the journal holds %+v there: an acknowledged challenge lost, moved or answered twice", write.Values, owner, write.Serial, commit)
					continue
				}
				commit.accounted = true
			}
		}
		for _, commit := range commits[owner] {
			if commit.accounted {
				continue
			}
			landed := slices.IndexFunc(unknownUpserts, func(write *dnsCertsChallengeWrite) bool { return slices.Equal(write.Values, commit.Values) })
			if landed < 0 {
				t.Errorf("the journal commits %v at %s at serial %d, which no write of certs' was answered at, nor sent with an unknown outcome: applied twice, or by a writer that is not certs", commit.Values, owner, commit.Serial)
				continue
			}
			unknownUpserts = slices.Delete(unknownUpserts, landed, landed+1)
			commit.accounted = true
		}
		for _, removal := range removals[owner] {
			if removal.accounted {
				continue
			}
			if unknownRemovals <= 0 {
				t.Errorf("the journal deletes %s at serial %d, which no removal of certs' was answered at, nor sent with an unknown outcome", owner, removal.Serial)
				continue
			}
			unknownRemovals -= 1
			removal.accounted = true
		}
		count, err := dnsCertsChallengeWindowCount(ctx, root, owner, windowStart)
		if err != nil {
			t.Fatal(err)
		}
		inWindow := len(commits[owner])
		switch {
		case count != inWindow:
			t.Errorf("the dns API counts %d writes of %s in dns_challenges_per_name_hour's window, for the %d the journal commits there since the case began", count, owner, inWindow)
		case settings.ChallengesPerNameHour < int64(count):
			t.Errorf("%s took %d writes in dns_challenges_per_name_hour's window, past its %d", owner, count, settings.ChallengesPerNameHour)
		}
		if dnsCertsIsAcmeOwner(owner, root) {
			acmeCount, err := dnsCertsAcmeChallengeWindow(ctx, root, owner, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if acmeCount != count {
				t.Errorf("certs reads %d writes of %s in the cap's window before it charges one, and the dns API counts %d", acmeCount, owner, count)
			}
		}
		t.Logf("the challenges at %s: %d commits and %d removals in the journal; certs' writes %v", owner, len(commits[owner]), len(removals[owner]), ends)
	}
}

// The commit at serial in commits, nil for none.
func dnsCertsFindCommit(commits []*dnsCertsChallengeCommit, serial uint64) *dnsCertsChallengeCommit {
	for _, commit := range commits {
		if commit.Serial == serial {
			return commit
		}
	}
	return nil
}

// dns_challenges_per_name_hour at each network's challenge name, exactly (see the file header): after every order has
// ended, so nothing of certs' writes there meanwhile.
func dnsCertsExhaustChallenges(t testing.TB, ctx context.Context, stack *dnsCertsStack, settings *dnsCertsSettings, networks []*dnsCertsNetwork, records *dnsCertsRecords) {
	t.Helper()
	for _, network := range networks {
		name := strings.TrimSuffix(network.Apex, ".")
		owner := dnsCertsChallengeOwner(name, records.Root)
		count, err := dnsCertsChallengeWindowCount(ctx, records.Root, owner, time.Now().Add(-settings.ChallengeWindow))
		if err != nil {
			t.Fatal(err)
		}
		start := count
		exhausted := false
		for sequence := range settings.ChallengeExhaustMaxCount {
			status, _, err := dnsCertsChallengePost(ctx, stack, &dnsservice.DnsChallengeRequest{Root: records.Root, Name: name, Values: []string{fmt.Sprintf("e2e-exhaust-%d", sequence)}})
			if err != nil {
				t.Errorf("a write at %s while exhausting its challenges: %v", owner, err)
				break
			}
			if status == http.StatusTooManyRequests {
				if int64(count) != settings.ChallengesPerNameHour {
					t.Errorf("a write at %s was refused for dns_challenges_per_name_hour with %d writes in its window, where the cap is %d", owner, count, settings.ChallengesPerNameHour)
				}
				exhausted = true
				break
			}
			if status != http.StatusOK {
				t.Errorf("a write at %s while exhausting its challenges answered %d", owner, status)
				break
			}
			count += 1
			if settings.ChallengesPerNameHour < int64(count) {
				t.Errorf("a write at %s was answered as its window's %d, past dns_challenges_per_name_hour's %d", owner, count, settings.ChallengesPerNameHour)
				break
			}
		}
		if !exhausted {
			t.Errorf("the writes at %s were never refused for dns_challenges_per_name_hour: %d in its window", owner, count)
		}
		// the removal: never counted, never refused
		status, answer, err := dnsCertsChallengePost(ctx, stack, &dnsservice.DnsChallengeRequest{Root: records.Root, Name: name})
		switch {
		case err != nil:
			t.Errorf("the removal at %s: %v", owner, err)
		case status != http.StatusOK || answer == nil || answer.Serial == 0:
			t.Errorf("the removal at %s past its cap answered %d, %+v, not a delete's serial", owner, status, answer)
		}
		after, err := dnsCertsChallengeWindowCount(ctx, records.Root, owner, time.Now().Add(-settings.ChallengeWindow))
		if err != nil {
			t.Fatal(err)
		}
		if after != count {
			t.Errorf("the removal at %s moved its window's count from %d to %d", owner, count, after)
		}
		set := dnsCertsSetKey{Owner: owner, Type: dns.TypeTXT}
		if answer, err := stack.World.AwaitDnsAnswer(ctx, owner, dns.TypeTXT, func(answer *dns.Msg) bool {
			return answer.Rcode == dns.RcodeSuccess && len(dnsCertsAnswerValues(answer, set)) == 0
		}, settings.Dns); err != nil {
			t.Errorf("the edge still answers %s after its removal: %v: %v", owner, err, answer)
		}
		t.Logf("dns_challenges_per_name_hour at %s: %d writes in its window before, refused at %d", owner, start, count)
	}
}

// api's write budget, exactly (see the file header).
func dnsCertsCheckApiBudget(t testing.TB, ctx context.Context, world *World, settings *dnsCertsSettings) {
	t.Helper()
	cluster, err := world.T3().Host().Blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	prices := evsservice.DefaultPriceTable4KiB()
	disagreements := []string{}
	for range settings.BudgetReadTryCount {
		before, err := dnsCertsApiObjects(ctx, world, settings.ObjectPageCount)
		if err != nil {
			t.Fatal(err)
		}
		listed := time.Now()
		ledgers, err := world.ReadLedgers(ctx, listed)
		if err != nil {
			t.Fatalf("the ledgers: %v", err)
		}
		after, err := dnsCertsApiObjects(ctx, world, settings.ObjectPageCount)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(before, after) {
			disagreements = append(disagreements, fmt.Sprintf("%d objects, then %d", len(before), len(after)))
			continue
		}
		want := map[sidecar.WriteP0Kind]int64{}
		objectCounts := map[sidecar.WriteP0Kind]int{}
		for key, byteCount := range after {
			for prefix, kind := range dnsCertsApiObjectKinds {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				price, err := prices.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: byteCount, Count: 1})
				if err != nil {
					t.Fatalf("the price of %s's %d bytes: %v", key, byteCount, err)
				}
				want[kind] += price
				objectCounts[kind] += 1
			}
		}
		var report *evsservice.WriteBudgetReport
		for _, candidate := range ledgers.BudgetReports[cluster.ClusterId()] {
			if candidate.ProcessId == ledgers.ApiProcessId {
				report = candidate
			}
		}
		if ledgers.ApiProcessId == "" || report == nil {
			t.Errorf("api's process %q holds no budget report of %s", ledgers.ApiProcessId, cluster.ClusterId())
			return
		}
		got := map[sidecar.WriteP0Kind]int64{}
		for kind, byteCount := range report.P0ByteCounts {
			if byteCount != 0 {
				got[kind] = byteCount
			}
		}
		if !maps.Equal(got, want) {
			t.Errorf("api's process reported P0 %v against %s, and the objects it holds there price at %v as created (%v objects by kind): a write issued and never made, or made and never issued", got, cluster.ClusterId(), want, objectCounts)
		}
		if report.P1ByteCount != 0 || report.UnfundedByteCount != 0 || report.FundedByteCount != 0 || report.CreditByteCount != 0 {
			t.Errorf("api's process reported P1 %d, P2 %d unfunded and %d funded, and credit %d, where all it writes is P0", report.P1ByteCount, report.UnfundedByteCount, report.FundedByteCount, report.CreditByteCount)
		}
		t.Logf("api's write budget: P0 %v against %s for %v objects", got, cluster.ClusterId(), objectCounts)
		return
	}
	t.Errorf("api's objects never stood still across a budget report: %v", disagreements)
}

// Pebble's nonce refusals, counted, and no replay (see the file header).
func dnsCertsCheckNonces(t testing.TB, stack *dnsCertsStack, settings *dnsCertsSettings) {
	t.Helper()
	totalPosts, totalRefused := 0, 0
	for _, recorder := range []*dnsCertsAcmeRecorder{stack.WorkerAcme, stack.DesktopAcme} {
		postCount, refusedCount, nonceless, resentCount, replays := recorder.Counts()
		totalPosts += postCount
		totalRefused += refusedCount
		t.Logf("%s sent Pebble %d POSTs: %d refused for their nonce, %d nonces sent again after no answer", recorder.name, postCount, refusedCount, resentCount)
		if nonceless != 0 {
			t.Errorf("%s sent Pebble %d POSTs with no nonce", recorder.name, nonceless)
		}
		if len(replays) != 0 {
			t.Errorf("%s sent %d nonces again after Pebble had answered them, a replay its retry hides: %v", recorder.name, len(replays), replays)
		}
	}
	share := 0.0
	if 0 < totalPosts {
		share = 100 * float64(totalRefused) / float64(totalPosts)
	}
	t.Logf("Pebble refused the nonces of %d of %d POSTs (%.1f%%, refusing %d%% at random)", totalRefused, totalPosts, share, settings.PebbleNonceRejectPercent)
}
