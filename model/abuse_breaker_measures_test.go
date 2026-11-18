package model

import (
	"context"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// The breakers' measures (abuse_breaker_measures.go; ABUSE-M9-03): the sample gathered from fake sources, each
// unknown where its source is, and the complaint rate from smtp_complaint and smtp_send rows in a build that links
// MG5 and MG9. Addresses are RFC 5737's.

// A Postmaster source answering from a map of days, and recording what it was asked.
type testPostmasterSource struct {
	dayRates map[string]int64
	asked    []string
}

func (self *testPostmasterSource) SpamRate(ctx context.Context, domain string, day time.Time) (int64, bool) {
	self.asked = append(self.asked, domain+" "+day.Format("2006-01-02"))
	partsPerMillion, ok := self.dayRates[day.Format("2006-01-02")]
	return partsPerMillion, ok
}

// A listing or SNDS source answering a fixed list, and recording the addresses it was asked about.
type testAddressSource struct {
	answer []netip.Addr
	known  bool
	asked  []netip.Addr
}

func (self *testAddressSource) ListedAddresses(ctx context.Context, addresses []netip.Addr, now time.Time) ([]netip.Addr, bool) {
	self.asked = append(self.asked, addresses...)
	return self.answer, self.known
}

func (self *testAddressSource) RedAddresses(ctx context.Context, addresses []netip.Addr) ([]netip.Addr, bool) {
	self.asked = append(self.asked, addresses...)
	return self.answer, self.known
}

var (
	testMeasuresNow      = time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	testMeasuresAddress  = netip.MustParseAddr("192.0.2.10")
	testMeasuresAddress2 = netip.MustParseAddr("192.0.2.11")
)

// Postmaster Tools reports a day late: the latest reported day inside the lag leads, the days before it follow for
// the run, a missing day in the run is unknown, and no reported day inside the lag is no rate at all.
func TestGatherMailBreakerMeasuresPostmasterDays(t *testing.T) {
	ctx := context.Background()
	settings := testMailBreakerSettings()
	gather := func(dayRates map[string]int64) ([]MailRate, *testPostmasterSource) {
		source := &testPostmasterSource{dayRates: dayRates}
		measures := gatherMailSourceMeasures(ctx, &MailBreakerSources{Root: "root.example", Postmaster: source}, testMeasuresNow, settings)
		return measures.PostmasterRates, source
	}

	rates, source := gather(map[string]int64{"2026-11-03": 1200, "2026-11-02": 900})
	if !slices.Equal(rates, []MailRate{KnownMailRate(1200), KnownMailRate(900)}) {
		t.Errorf("a day late: %v", rates)
	}
	if !slices.Equal(source.asked, []string{"root.example 2026-11-04", "root.example 2026-11-03", "root.example 2026-11-02"}) {
		t.Errorf("asked %v", source.asked)
	}
	// two days late, and the day before the latest missing
	rates, _ = gather(map[string]int64{"2026-11-02": 1500, "2026-10-31": 1500})
	if !slices.Equal(rates, []MailRate{KnownMailRate(1500), {}}) {
		t.Errorf("a missing day in the run: %v", rates)
	}
	// a day at zero is a rate, not a missing day
	rates, _ = gather(map[string]int64{"2026-11-04": 0, "2026-11-03": 0})
	if !slices.Equal(rates, []MailRate{KnownMailRate(0), KnownMailRate(0)}) {
		t.Errorf("days at zero: %v", rates)
	}
	// nothing inside the lag: an older day is not the latest
	rates, source = gather(map[string]int64{"2026-10-31": 100})
	if len(rates) != 0 || len(source.asked) != settings.PostmasterMaxLagDays+1 {
		t.Errorf("past the lag: %v after %d asks", rates, len(source.asked))
	}
	// no source, and no root: nothing is asked
	if measures := gatherMailSourceMeasures(ctx, &MailBreakerSources{Root: "root.example"}, testMeasuresNow, settings); len(measures.PostmasterRates) != 0 {
		t.Errorf("no source: %v", measures.PostmasterRates)
	}
	source = &testPostmasterSource{dayRates: map[string]int64{"2026-11-03": 1}}
	if measures := gatherMailSourceMeasures(ctx, &MailBreakerSources{Postmaster: source}, testMeasuresNow, settings); len(measures.PostmasterRates) != 0 || len(source.asked) != 0 {
		t.Errorf("no root: %v after %d asks", measures.PostmasterRates, len(source.asked))
	}
	// the machine reads a missing day as holding
	state := NextMailBreakerState(MailBreakerState{}, &MailBreakerMeasures{PostmasterRates: []MailRate{KnownMailRate(1500), {}}, ComplaintRate: KnownMailRate(0), ListingKnown: true}, testMeasuresNow, settings)
	if state.Level != MailBreakerLevelNone {
		t.Errorf("a missing day made a run: level %d", state.Level)
	}
}

// The listing is known only when the sending addresses are and both sources answered; only the platform's own
// addresses are asked about, and only they come back.
func TestGatherMailBreakerMeasuresListing(t *testing.T) {
	ctx := context.Background()
	settings := testMailBreakerSettings()
	own := []netip.Addr{testMeasuresAddress, netip.MustParseAddr("::ffff:192.0.2.11")}
	stranger := netip.MustParseAddr("198.51.100.7")

	listing := &testAddressSource{answer: []netip.Addr{testMeasuresAddress2, stranger}, known: true}
	snds := &testAddressSource{answer: []netip.Addr{testMeasuresAddress}, known: true}
	measures := gatherMailSourceMeasures(ctx, &MailBreakerSources{SendingAddresses: own, Listing: listing, Snds: snds}, testMeasuresNow, settings)
	if !measures.ListingKnown || !slices.Equal(measures.ListedAddresses, []netip.Addr{testMeasuresAddress, testMeasuresAddress2}) {
		t.Errorf("both sources: %v, known %t", measures.ListedAddresses, measures.ListingKnown)
	}
	for _, source := range []*testAddressSource{listing, snds} {
		if !slices.Equal(source.asked, []netip.Addr{testMeasuresAddress, testMeasuresAddress2}) {
			t.Errorf("a source was asked about %v", source.asked)
		}
	}

	cases := []struct {
		name    string
		sources *MailBreakerSources
		listed  []netip.Addr
	}{
		{"SNDS down", &MailBreakerSources{SendingAddresses: own, Listing: &testAddressSource{answer: []netip.Addr{testMeasuresAddress2}, known: true}, Snds: &testAddressSource{}}, []netip.Addr{testMeasuresAddress2}},
		{"the mirror down", &MailBreakerSources{SendingAddresses: own, Listing: &testAddressSource{}, Snds: &testAddressSource{known: true}}, []netip.Addr{}},
		{"no SNDS", &MailBreakerSources{SendingAddresses: own, Listing: &testAddressSource{known: true}}, []netip.Addr{}},
		{"no mirror", &MailBreakerSources{SendingAddresses: own, Snds: &testAddressSource{known: true}}, []netip.Addr{}},
		{"no sending addresses", &MailBreakerSources{Listing: &testAddressSource{known: true}, Snds: &testAddressSource{known: true}}, []netip.Addr{}},
		{"no sources", nil, []netip.Addr{}},
	}
	for _, c := range cases {
		measures := gatherMailSourceMeasures(ctx, c.sources, testMeasuresNow, settings)
		if measures.ListingKnown || !slices.Equal(measures.ListedAddresses, c.listed) {
			t.Errorf("%s: %v, known %t", c.name, measures.ListedAddresses, measures.ListingKnown)
		}
	}
	// both clean and answered: known, with nothing listed
	measures = gatherMailSourceMeasures(ctx, &MailBreakerSources{SendingAddresses: own, Listing: &testAddressSource{known: true}, Snds: &testAddressSource{known: true}}, testMeasuresNow, settings)
	if !measures.ListingKnown || len(measures.ListedAddresses) != 0 {
		t.Errorf("both clean: %v, known %t", measures.ListedAddresses, measures.ListingKnown)
	}
}

// The figures are aggregate: no type a measure travels in has a field for a network, a client, a message or a
// recipient, so no value returned can name one.
func TestMeasuresAreAggregate(t *testing.T) {
	idType := reflect.TypeOf(server.Id{})
	sidecarIdType := reflect.TypeOf(sidecar.Id{})
	var walk func(path string, value reflect.Type, seen map[reflect.Type]bool)
	walk = func(path string, value reflect.Type, seen map[reflect.Type]bool) {
		if value == idType || value == sidecarIdType {
			t.Errorf("%s holds an id", path)
			return
		}
		if seen[value] {
			return
		}
		seen[value] = true
		switch value.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path, value.Elem(), seen)
		case reflect.Map:
			walk(path+" key", value.Key(), seen)
			walk(path, value.Elem(), seen)
		case reflect.Struct:
			if value == reflect.TypeOf(time.Time{}) || value == reflect.TypeOf(netip.Addr{}) {
				return
			}
			for i := range value.NumField() {
				field := value.Field(i)
				lower := strings.ToLower(field.Name)
				for _, word := range []string{"network", "client", "recipient", "messageid", "sendid", "label", "domain", "userauth"} {
					if strings.Contains(lower, word) {
						t.Errorf("%s.%s names a %s", path, field.Name, word)
					}
				}
				walk(path+"."+field.Name, field.Type, seen)
			}
		}
	}
	for _, value := range []any{MailBreakerMeasures{}, MailRate{}, MailBreakerState{}, map[netip.Addr]server.SndsColor{}} {
		walk(reflect.TypeOf(value).String(), reflect.TypeOf(value), map[reflect.Type]bool{})
	}
	// the sources' answers: a rate and a flag, or addresses and a flag
	for name, method := range map[string]reflect.Type{
		"PostmasterToolsClient.SpamRate":        reflect.TypeOf((*server.PostmasterToolsClient).SpamRate),
		"SndsClient.RedAddresses":               reflect.TypeOf((*server.SndsClient).RedAddresses),
		"SndsClient.Colors":                     reflect.TypeOf((*server.SndsClient).Colors),
		"SpamhausListingClient.ListedAddresses": reflect.TypeOf((*server.SpamhausListingClient).ListedAddresses),
		"MailComplaintRate":                     reflect.TypeOf(MailComplaintRate),
		"GatherMailBreakerMeasures":             reflect.TypeOf(GatherMailBreakerMeasures),
	} {
		for i := range method.NumOut() {
			walk(name, method.Out(i), map[reflect.Type]bool{})
		}
	}
	// and the clients are the sources the sample is gathered from
	var _ MailPostmasterSource = (*server.PostmasterToolsClient)(nil)
	var _ MailSndsSource = (*server.SndsClient)(nil)
	var _ MailListingSource = (*server.SpamhausListingClient)(nil)
}

// A count over a total in parts per million, rounded up, and unknown with no total.
func TestMailRateOf(t *testing.T) {
	cases := []struct {
		count int64
		total int64
		want  MailRate
	}{
		{0, 0, MailRate{}},
		{3, 0, MailRate{}},
		{0, 1000, KnownMailRate(0)},
		{1, 1000, KnownMailRate(1000)},
		{1, 1001, KnownMailRate(1000)},
		{2, 1000, KnownMailRate(2000)},
		{1, 3, KnownMailRate(333334)},
		{5, 3, KnownMailRate(1000 * 1000)},
		{-1, 10, MailRate{}},
	}
	for _, c := range cases {
		if got := mailRateOf(c.count, c.total); got != c.want {
			t.Errorf("%d over %d: %+v, want %+v", c.count, c.total, got, c.want)
		}
	}
}

func testMeasuresComplaint(networkId sidecar.Id, messageIdHash []byte, complaintType string, confidence smtpedgeservice.SmtpConfidence, arrival time.Time) *smtpedgeservice.SmtpComplaint {
	return &smtpedgeservice.SmtpComplaint{
		ComplaintId:   sidecar.NewId(),
		NetworkId:     networkId,
		Reporter:      "reporter.example",
		Type:          complaintType,
		ArrivalTime:   arrival,
		Domain:        "quietnet.root.example",
		MessageIdHash: messageIdHash,
		JoinResult:    string(confidence),
	}
}

// The complaint rate from rows: complaints at high or medium confidence of the types §14.8 counts, each message
// once, over recipients counted once a message, in the trailing 24 h; nothing sent is unknown, not zero.
func TestMailComplaintRateFromRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
			if rate := MailComplaintRate(context.Background(), server.NowUtc(), testMailBreakerSettings()); rate.Known {
				t.Fatalf("a binary without the tables answered %+v", rate)
			}
			return
		}
		ctx := context.Background()
		settings := testMailBreakerSettings()
		sendSettings := DefaultSmtpSendSettings()
		// a day well ahead, so only this test's rows are in the window
		now := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 3, 0)).Add(12 * time.Hour)

		// nothing sent: unknown, whatever was reported
		if rate := MailComplaintRate(ctx, now, settings); rate.Known {
			t.Fatalf("with nothing sent: %+v", rate)
		}

		first := sidecar.NewId()
		second := sidecar.NewId()
		sends := []*smtpedgeservice.SmtpSendRecord{}
		send := func(networkId sidecar.Id, start time.Time, recipientCount uint32) *smtpedgeservice.SmtpSendRecord {
			record := smtpTestSend(networkId, start)
			record.RecipientCount = recipientCount
			sends = append(sends, record)
			return record
		}
		reported := send(first, now.Add(-time.Hour), 400)
		retracted := send(first, now.Add(-2*time.Hour), 300)
		other := send(second, now.Add(-90*time.Minute), 250)
		// a message whose first try reached 20 of its recipients and whose retry reached 50: one message, 50
		tried := send(second, now.Add(-2*time.Hour), 20)
		retry := send(second, now.Add(-time.Hour), 50)
		retry.RetryOfSendId = tried.SendId
		result, err := insertSmtpSendsAt(ctx, sends, now, sendSettings)
		if err != nil || result.StoredCount != len(sends) {
			t.Fatalf("the sends: %+v, %v", result, err)
		}
		// 400 + 300 + 250 + 50 recipients
		if rate := MailComplaintRate(ctx, now, settings); rate != KnownMailRate(0) {
			t.Fatalf("with no complaint: %+v", rate)
		}

		complaints := []*smtpedgeservice.SmtpComplaint{
			// one message reported by two operators' loops: one complaint
			testMeasuresComplaint(first, reported.MessageIdHash, smtpedgeservice.SmtpComplaintTypeAbuse, smtpedgeservice.SmtpConfidenceHigh, now.Add(-30*time.Minute)),
			testMeasuresComplaint(first, reported.MessageIdHash, smtpedgeservice.SmtpComplaintTypeAbuse, smtpedgeservice.SmtpConfidenceMedium, now.Add(-20*time.Minute)),
			// another network's, at medium confidence
			testMeasuresComplaint(second, other.MessageIdHash, smtpedgeservice.SmtpComplaintTypeFraud, smtpedgeservice.SmtpConfidenceMedium, now.Add(-10*time.Minute)),
			// uncounted: low confidence, no join, a not-spam, an aggregate, and one from before the window
			testMeasuresComplaint(first, smtpedgeservice.SmtpMessageIdHash("<low@quietnet.root.example>"), smtpedgeservice.SmtpComplaintTypeAbuse, "low", now.Add(-10*time.Minute)),
			testMeasuresComplaint(first, smtpedgeservice.SmtpMessageIdHash("<none@quietnet.root.example>"), smtpedgeservice.SmtpComplaintTypeAbuse, "", now.Add(-10*time.Minute)),
			testMeasuresComplaint(first, smtpedgeservice.SmtpMessageIdHash("<fine@quietnet.root.example>"), smtpedgeservice.SmtpComplaintTypeNotSpam, smtpedgeservice.SmtpConfidenceHigh, now.Add(-10*time.Minute)),
			testMeasuresComplaint(first, nil, smtpedgeservice.SmtpComplaintTypeRua, smtpedgeservice.SmtpConfidenceHigh, now.Add(-10*time.Minute)),
			testMeasuresComplaint(first, smtpedgeservice.SmtpMessageIdHash("<old@quietnet.root.example>"), smtpedgeservice.SmtpComplaintTypeAbuse, smtpedgeservice.SmtpConfidenceHigh, now.Add(-25*time.Hour)),
			// a complaint whose message a not-spam in the window names counts nothing, as §14.8's
			testMeasuresComplaint(first, retracted.MessageIdHash, smtpedgeservice.SmtpComplaintTypeAbuse, smtpedgeservice.SmtpConfidenceHigh, now.Add(-40*time.Minute)),
			testMeasuresComplaint(first, retracted.MessageIdHash, smtpedgeservice.SmtpComplaintTypeNotSpam, smtpedgeservice.SmtpConfidenceHigh, now.Add(-15*time.Minute)),
		}
		for _, complaint := range complaints {
			if err := InsertSmtpComplaint(ctx, complaint); err != nil {
				t.Fatal(err)
			}
		}
		// 2 complaints over 1,000 recipients: 0.20%
		if rate := MailComplaintRate(ctx, now, settings); rate != KnownMailRate(2000) {
			t.Fatalf("2 complaints over 1,000 recipients: %+v", rate)
		}
		// two complaints with no Message-ID hash are two complaints
		for range 2 {
			if err := InsertSmtpComplaint(ctx, testMeasuresComplaint(first, nil, smtpedgeservice.SmtpComplaintTypeOther, smtpedgeservice.SmtpConfidenceHigh, now.Add(-5*time.Minute))); err != nil {
				t.Fatal(err)
			}
		}
		if rate := MailComplaintRate(ctx, now, settings); rate != KnownMailRate(4000) {
			t.Fatalf("4 complaints over 1,000 recipients: %+v", rate)
		}
		// 21 hours on, the sends are still in the window, and the complaints too
		if rate := MailComplaintRate(ctx, now.Add(21*time.Hour), settings); rate != KnownMailRate(4000) {
			t.Fatalf("21 hours on: %+v", rate)
		}
		// past the window nothing was sent, so the rate is unknown, not zero
		if rate := MailComplaintRate(ctx, now.Add(27*time.Hour), settings); rate.Known {
			t.Fatalf("27 hours on: %+v", rate)
		}
		// the sample carries the rate
		measures := GatherMailBreakerMeasures(ctx, nil, now, settings)
		if measures.ComplaintRate != KnownMailRate(4000) || measures.ListingKnown || len(measures.PostmasterRates) != 0 {
			t.Fatalf("the sample: %+v", measures)
		}
	})
}
