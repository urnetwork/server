package server

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// abuse.yml's reader (abuse_config.go). Pure: a config home in the test's own
// directory or an override stands in for the file, and a fake clock moves the
// rereads.

// Points Config at a new, empty config home for the rest of the test.
func newTestAbuseConfigHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_CONFIG_HOME", home)
	return home
}

// Every key an absent file answers: nothing on, nothing routed.
func requireInertAbuseConfig(t *testing.T, name string, config *AbuseConfig) {
	t.Helper()
	if config.HoldsEnabled() {
		t.Errorf("%s: holds are on", name)
	}
	if config.AbuseMailbox() != "" || config.LegalRequestMailbox() != "" || config.CounselContact() != "" {
		t.Errorf("%s: a mailbox or contact is set", name)
	}
	if len(config.Feeds()) != 0 || config.WebRiskEnabled() {
		t.Errorf("%s: a scanner runs", name)
	}
	if len(config.PagerRouting()) != 0 {
		t.Errorf("%s: notices page with routing", name)
	}
	if config.NoticeSenderClass("registrar.example") != "" {
		t.Errorf("%s: a sender pages", name)
	}
	if len(config.RootNames()) != 0 || len(config.PlatformNames()) != 0 {
		t.Errorf("%s: names are protected, so a ladder could act", name)
	}
	if config.AppealWindowMonths() != AbuseAppealWindowMinMonths {
		t.Errorf("%s: the appeal window is %d months", name, config.AppealWindowMonths())
	}
}

// Absent, the file is inert, and a home that is not there is absent too.
func TestAbuseConfigIsInertWhenAbsent(t *testing.T) {
	home := newTestAbuseConfigHome(t)
	requireInertAbuseConfig(t, "an absent file", loadAbuseConfig(nil))

	// the answer came from this home: a file put there is read
	if err := os.WriteFile(filepath.Join(home, abuseConfigResourceName), []byte("holds_enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !loadAbuseConfig(nil).HoldsEnabled() {
		t.Fatal("the test's config home is not the one read")
	}

	t.Setenv("WARP_CONFIG_HOME", filepath.Join(t.TempDir(), "absent"))
	requireInertAbuseConfig(t, "a home that is not there", loadAbuseConfig(nil))
}

// A file that does not parse refuses holds on a process's first read, and after
// a good read keeps it: a broken push turns nothing on and drops no route.
func TestAbuseConfigBadYamlOnFirstReadRefusesHolds(t *testing.T) {
	home := newTestAbuseConfigHome(t)
	path := filepath.Join(home, abuseConfigResourceName)
	if err := os.WriteFile(path, []byte("holds_enabled: [true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireInertAbuseConfig(t, "bad yaml first", loadAbuseConfig(nil))

	if err := os.WriteFile(path, []byte("holds_enabled: true\nabuse_mailbox: records@mailbox.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := loadAbuseConfig(nil)
	if !good.HoldsEnabled() || good.AbuseMailbox() != "records@mailbox.example" {
		t.Fatalf("the good read: holds %t, mailbox %q", good.HoldsEnabled(), good.AbuseMailbox())
	}

	if err := os.WriteFile(path, []byte("abuse_mailbox: {\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loadAbuseConfig(good) != good {
		t.Fatal("a bad read replaced the latest good read")
	}
	// a removal holds as a bad read does, until the process restarts
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if loadAbuseConfig(good) != good {
		t.Fatal("a removal replaced the latest good read")
	}
}

// A file that is there but cannot be read is unavailable, not absent.
func TestAbuseConfigUnavailableIsNotAbsent(t *testing.T) {
	home := newTestAbuseConfigHome(t)
	if err := os.Mkdir(filepath.Join(home, abuseConfigResourceName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Config.SimpleResource(abuseConfigResourceName); !errors.Is(err, ErrResourceUnavailable) {
		t.Fatalf("the resolver answers %v, want an unavailable resource", err)
	}
	requireInertAbuseConfig(t, "a directory", loadAbuseConfig(nil))
	previous := &AbuseConfig{holdsEnabled: true}
	if loadAbuseConfig(previous) != previous {
		t.Fatal("an unavailable file replaced the latest good read")
	}
}

// holds_enabled is false unless the file says true.
func TestAbuseConfigHoldsOffByDefault(t *testing.T) {
	config, err := parseAbuseConfig([]byte("abuse_mailbox: records@mailbox.example\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.HoldsEnabled() {
		t.Fatal("a file without holds_enabled turns holds on")
	}
}

// Every key, as §21.16 and the resolutions spell it.
func TestAbuseConfigReadsItsKeys(t *testing.T) {
	config, err := parseAbuseConfig([]byte(`
holds_enabled: true
business_hours: {mode: business, zone: Europe/Berlin, days: [mon, tue], open: "08:00", close: "17:00", holidays: ["2026-12-25"]}
pager: {routing_key: abuse-notices}
abuse_mailbox: " records@mailbox.example "
legal_request_mailbox: legal@mailbox.example
counsel_contact: counsel@counsel.example
feeds:
  - {name: feed-one, url: "https://feed.example/list.txt"}
web_risk: {enabled: true}
notice_senders: {"Registrar.Example.": registrar, registry.example: registry, police.example: law_enforcement}
appeal_window: 9
appeal_url: "https://appeal.example/abuse/appeal"
form_url: "https://form.example/abuse"
protected_names: {roots: [root.example], platform: [platform.example, " "]}
something_new: {ignored: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !config.HoldsEnabled() {
		t.Error("holds_enabled")
	}
	hours := config.BusinessHours()
	if hours.Mode != "business" || hours.Zone != "Europe/Berlin" || !slices.Equal(hours.Days, []string{"mon", "tue"}) ||
		hours.Open != "08:00" || hours.Close != "17:00" || !slices.Equal(hours.Holidays, []string{"2026-12-25"}) {
		t.Errorf("business_hours %+v", hours)
	}
	if config.PagerRouting()["routing_key"] != "abuse-notices" {
		t.Errorf("pager %v", config.PagerRouting())
	}
	if config.AbuseMailbox() != "records@mailbox.example" || config.LegalRequestMailbox() != "legal@mailbox.example" {
		t.Errorf("mailboxes %q, %q", config.AbuseMailbox(), config.LegalRequestMailbox())
	}
	if config.CounselContact() != "counsel@counsel.example" {
		t.Errorf("counsel_contact %q", config.CounselContact())
	}
	if feeds := config.Feeds(); len(feeds) != 1 || feeds[0].Name != "feed-one" || feeds[0].Url != "https://feed.example/list.txt" {
		t.Errorf("feeds %+v", feeds)
	}
	if !config.WebRiskEnabled() {
		t.Error("web_risk")
	}
	for domain, class := range map[string]string{
		"registrar.example":  AbuseNoticeSenderRegistrar,
		"REGISTRY.example.":  AbuseNoticeSenderRegistry,
		"police.example":     AbuseNoticeSenderLawEnforcement,
		"unlisted.example":   "",
		"sub.police.example": "",
	} {
		if got := config.NoticeSenderClass(domain); got != class {
			t.Errorf("notice sender %s: %q, want %q", domain, got, class)
		}
	}
	if config.AppealWindowMonths() != 9 {
		t.Errorf("appeal_window %d", config.AppealWindowMonths())
	}
	if config.AppealUrl() != "https://appeal.example/abuse/appeal" || config.FormUrl() != "https://form.example/abuse" {
		t.Errorf("urls %q, %q", config.AppealUrl(), config.FormUrl())
	}
	if !slices.Equal(config.RootNames(), []string{"root.example"}) || !slices.Equal(config.PlatformNames(), []string{"platform.example"}) {
		t.Errorf("protected names %q, %q", config.RootNames(), config.PlatformNames())
	}
}

// Within a file that parses, one bad entry costs only itself.
func TestAbuseConfigBadEntryCostsOnlyItself(t *testing.T) {
	config, err := parseAbuseConfig([]byte(`
feeds:
  - {name: plain, url: "http://feed.example/list.txt"}
  - {name: good, url: "https://feed.example/list.txt"}
  - {name: nohost, url: "https:///list.txt"}
notice_senders: {registrar.example: registrar, anyone.example: public}
appeal_window: 2
appeal_url: "javascript:alert(1)"
form_url: "https://form.example/abuse"
`))
	if err != nil {
		t.Fatal(err)
	}
	if feeds := config.Feeds(); len(feeds) != 1 || feeds[0].Name != "good" {
		t.Errorf("feeds %+v", feeds)
	}
	if config.NoticeSenderClass("anyone.example") != "" || config.NoticeSenderClass("registrar.example") != AbuseNoticeSenderRegistrar {
		t.Error("a sender of an unknown class pages, or cost its neighbour")
	}
	if config.AppealWindowMonths() != AbuseAppealWindowMinMonths {
		t.Errorf("an appeal window under DSA Art. 20 reads %d months", config.AppealWindowMonths())
	}
	if config.AppealUrl() != "" || config.FormUrl() != "https://form.example/abuse" {
		t.Errorf("urls %q, %q", config.AppealUrl(), config.FormUrl())
	}
}

// A getter's answer is a copy, so a caller cannot change a read others share.
func TestAbuseConfigGettersCopy(t *testing.T) {
	config, err := parseAbuseConfig([]byte("protected_names: {roots: [root.example]}\npager: {k: v}\n"))
	if err != nil {
		t.Fatal(err)
	}
	config.RootNames()[0] = "changed.example"
	config.PagerRouting()["k"] = "changed"
	if config.RootNames()[0] != "root.example" || config.PagerRouting()["k"] != "v" {
		t.Fatal("a getter handed out the read's own slice or map")
	}
}

// One read answers for 60 s, the next caller after that rereads, and a reread
// that fails keeps the read it had.
func TestAbuseConfigRereadsEverySixtySeconds(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	loads := 0
	answers := []*AbuseConfig{{holdsEnabled: false}, {holdsEnabled: true}}
	reader := newAbuseConfigReader(DefaultAbuseConfigSettings())
	reader.now = func() time.Time { return now }
	reader.load = func(previous *AbuseConfig) *AbuseConfig {
		answer := answers[min(loads, len(answers)-1)]
		loads += 1
		return answer
	}
	if reader.get().HoldsEnabled() || loads != 1 {
		t.Fatalf("the first read: %d loads", loads)
	}
	now = now.Add(60*time.Second - time.Nanosecond)
	if reader.get().HoldsEnabled() || loads != 1 {
		t.Fatalf("a read inside 60 s reloaded: %d loads", loads)
	}
	now = now.Add(time.Nanosecond)
	if !reader.get().HoldsEnabled() || loads != 2 {
		t.Fatalf("a read at 60 s did not reload: %d loads", loads)
	}
}

// The override the tests of other packages use reaches GetAbuseConfig at once,
// and its pop forgets it as quickly.
func TestAbuseConfigTestingPush(t *testing.T) {
	newTestAbuseConfigHome(t)
	pop := Testing_PushAbuseConfig("holds_enabled: true\n")
	if !GetAbuseConfig().HoldsEnabled() {
		pop()
		t.Fatal("the pushed file is not read")
	}
	pop()
	if GetAbuseConfig().HoldsEnabled() {
		t.Fatal("the popped file is still read")
	}
}

// config/local/abuse.yml, beside this checkout, parses and is dark.
func TestAbuseConfigLocalFileIsDark(t *testing.T) {
	data, err := os.ReadFile("../config/local/abuse.yml")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no config checkout beside the server")
	}
	if err != nil {
		t.Fatal(err)
	}
	config, err := parseAbuseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	requireInertAbuseConfig(t, "config/local/abuse.yml", config)
	if hours := config.BusinessHours(); hours.Mode != "business" || hours.Zone != "UTC" {
		t.Errorf("business_hours %+v", hours)
	}
}
