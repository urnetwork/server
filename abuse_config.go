package server

import (
	"errors"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"
)

// §21's routing, config/<env>/abuse.yml (§21.16). Optional, reread every 60 s
// as sidecar.yml is, and holding no secret: the Web Risk key and the mailboxes'
// seal and MAC keys sit in vault.
//
//	holds_enabled: false               # §23.7: true only once counsel confirms
//	business_hours: {mode: business, zone: UTC, days: [mon, tue, wed, thu, fri], open: "09:00", close: "18:00", holidays: []}
//	pager: {<field>: <value>}          # the pager adapter's routing (plan-OPS-pager-service)
//	abuse_mailbox: <address>           # §21.10's off-site record of every action and lift
//	legal_request_mailbox: <address>   # one record per hold placed, renewed or ended
//	counsel_contact: <contact>         # §21.8: production orders out of hours
//	feeds: [{name: <name>, url: https://...}]
//	web_risk: {enabled: false}
//	notice_senders: {<domain>: registrar | registry | law_enforcement}
//	appeal_window: 6                   # months, DSA Art. 20's floor
//	appeal_url: https://...
//	form_url: https://...
//	protected_names: {roots: [...], platform: [...]}
//
// business_hours is plan-ABUSE-small-readings #2's spelling of the plan's
// review block, mode included; legal_request_mailbox is #12's; protected_names
// is abuse-core-protected-names'; notice_senders is plan-ABUSE-notice-intake's.
// §21.16's list picks each up at the next records pass.
//
// Absent, the file is inert: the scanners idle, holds refuse, notices page with
// no routing detail and no mail pages as a notice. A file that is there and
// cannot be read or parsed answers the same on a process's first read, and
// after a good read the good read stands, as a removal does, until the process
// restarts: a broken push never turns holds on or drops a route. Within a file
// that parses, one bad entry costs only itself.

// The file's name under config/<env>.
const abuseConfigResourceName = "abuse.yml"

// DSA Art. 20's floor on how long an appeal may be filed, in months: a statute,
// not a setting, so no file goes below it.
const AbuseAppealWindowMinMonths = 6

// Settings of the reader.
type AbuseConfigSettings struct {
	// How long one read answers before the next. §19.11's 60 s, as sidecar.yml,
	// so a change moves everywhere within a minute of a config push.
	// Non-positive rereads on every call.
	RereadInterval time.Duration
}

// §19.11's interval.
func DefaultAbuseConfigSettings() *AbuseConfigSettings {
	return &AbuseConfigSettings{
		RereadInterval: 60 * time.Second,
	}
}

// The review calendar as the file spells it. model.AbuseReviewHoursOf lays it
// over model.DefaultAbuseReviewHours, so a key left out keeps its default.
type AbuseBusinessHours struct {
	// business or always (M6's 24/7); empty keeps the default
	Mode string `yaml:"mode"`
	// an IANA zone name; empty keeps the default
	Zone string `yaml:"zone"`
	// open weekdays; empty keeps the default
	Days []string `yaml:"days"`
	// local HH:MM; empty keeps the default
	Open  string `yaml:"open"`
	Close string `yaml:"close"`
	// YYYY-MM-DD dates on which no one reviews. The calendar does not read them
	// yet (model/abuse_clock.go), so a deadline can still land on one.
	Holidays []string `yaml:"holidays"`
}

// One URL feed (plan-ABUSE-small-readings #14): a plain list of URLs or hosts.
type AbuseFeed struct {
	Name string `yaml:"name"`
	Url  string `yaml:"url"`
}

// The keys this reader reads.
type abuseConfigYaml struct {
	HoldsEnabled        bool               `yaml:"holds_enabled"`
	BusinessHours       AbuseBusinessHours `yaml:"business_hours"`
	Pager               map[string]string  `yaml:"pager"`
	AbuseMailbox        string             `yaml:"abuse_mailbox"`
	LegalRequestMailbox string             `yaml:"legal_request_mailbox"`
	CounselContact      string             `yaml:"counsel_contact"`
	Feeds               []AbuseFeed        `yaml:"feeds"`
	WebRisk             struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"web_risk"`
	NoticeSenders  map[string]string `yaml:"notice_senders"`
	AppealWindow   int               `yaml:"appeal_window"`
	AppealUrl      string            `yaml:"appeal_url"`
	FormUrl        string            `yaml:"form_url"`
	ProtectedNames struct {
		Roots    []string `yaml:"roots"`
		Platform []string `yaml:"platform"`
	} `yaml:"protected_names"`
}

// A notice sender's class (plan-ABUSE-notice-intake): the reporter classes
// model.AbuseReporterClass names for these, whose DKIM-verified mail pages.
const (
	AbuseNoticeSenderRegistrar      = "registrar"
	AbuseNoticeSenderRegistry       = "registry"
	AbuseNoticeSenderLawEnforcement = "law_enforcement"
)

// One read of abuse.yml. Immutable once read, so safe for concurrent use; the
// getters copy what they return. The zero value is what an absent file reads.
type AbuseConfig struct {
	holdsEnabled        bool
	businessHours       AbuseBusinessHours
	pager               map[string]string
	abuseMailbox        string
	legalRequestMailbox string
	counselContact      string
	feeds               []AbuseFeed
	webRiskEnabled      bool
	noticeSenders       map[string]string
	appealWindowMonths  int
	appealUrl           string
	formUrl             string
	rootNames           []string
	platformNames       []string
}

// Whether a hold may be placed (§23.7, plan-ABUSE-small-readings #20): false
// until the file says true, and false while the file is absent or broken.
// Readers of hold/ need no gate: no object, no effect.
func (self *AbuseConfig) HoldsEnabled() bool {
	return self.holdsEnabled
}

// The review calendar's keys as the file gave them.
func (self *AbuseConfig) BusinessHours() AbuseBusinessHours {
	hours := self.businessHours
	hours.Days = slices.Clone(hours.Days)
	hours.Holidays = slices.Clone(hours.Holidays)
	return hours
}

// The pager adapter's routing fields for §21.8's notices, empty when none: a
// notice then pages with no routing detail.
func (self *AbuseConfig) PagerRouting() map[string]string {
	return maps.Clone(self.pager)
}

// Where §21.10's record of each action and lift goes, "" when none: the record
// then stays owed and the action pass retries it.
func (self *AbuseConfig) AbuseMailbox() string {
	return self.abuseMailbox
}

// Where each hold's record goes (plan-ABUSE-small-readings #12), "" when none.
func (self *AbuseConfig) LegalRequestMailbox() string {
	return self.legalRequestMailbox
}

// Counsel's out-of-hours contact for production orders and data requests, ""
// when none.
func (self *AbuseConfig) CounselContact() string {
	return self.counselContact
}

// The URL feeds the abuse worker fetches, each https. Empty idles the feed scan.
func (self *AbuseConfig) Feeds() []AbuseFeed {
	return slices.Clone(self.feeds)
}

// Whether the Web Risk scan runs. It also needs vault's key.
func (self *AbuseConfig) WebRiskEnabled() bool {
	return self.webRiskEnabled
}

// The class of a DKIM-verified sender domain whose mail to abuse@ or postmaster@
// pages, "" for an unlisted one.
func (self *AbuseConfig) NoticeSenderClass(domain string) string {
	return self.noticeSenders[abuseConfigDomain(domain)]
}

// How long after its notice an appeal may be filed, in calendar months
// (plan-ABUSE-small-readings #9): the file's, never under
// AbuseAppealWindowMinMonths.
func (self *AbuseConfig) AppealWindowMonths() int {
	return max(self.appealWindowMonths, AbuseAppealWindowMinMonths)
}

// The appeal link a notice carries, "" when none.
func (self *AbuseConfig) AppealUrl() string {
	return self.appealUrl
}

// The report form, which the role-mail listener's refusal points to, "" when
// none.
func (self *AbuseConfig) FormUrl() string {
	return self.formUrl
}

// The roots, such as <root>, which no action targets (abuse-core-protected-names).
// Empty refuses every target.
func (self *AbuseConfig) RootNames() []string {
	return slices.Clone(self.rootNames)
}

// Platform names and domains, which no action targets, nor any name under one.
// Empty refuses every target.
func (self *AbuseConfig) PlatformNames() []string {
	return slices.Clone(self.platformNames)
}

// A domain as a sender list compares it: case and a trailing dot are no
// difference.
func abuseConfigDomain(domain string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// An https URL with a host, or "" with a log line naming the key only.
func abuseConfigHttpsUrl(key string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		glog.Errorf("[abuse]%s sets a %s that is not an https url, so it reads none\n", abuseConfigResourceName, key)
		return ""
	}
	return value
}

// Names of a list, trimmed, empty ones skipped.
func abuseConfigNames(names []string) []string {
	kept := []string{}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			kept = append(kept, name)
		}
	}
	return kept
}

// One bad entry costs only that entry.
func newAbuseConfig(y *abuseConfigYaml) *AbuseConfig {
	config := &AbuseConfig{
		holdsEnabled:        y.HoldsEnabled,
		businessHours:       y.BusinessHours,
		pager:               map[string]string{},
		abuseMailbox:        strings.TrimSpace(y.AbuseMailbox),
		legalRequestMailbox: strings.TrimSpace(y.LegalRequestMailbox),
		counselContact:      strings.TrimSpace(y.CounselContact),
		webRiskEnabled:      y.WebRisk.Enabled,
		noticeSenders:       map[string]string{},
		appealUrl:           abuseConfigHttpsUrl("appeal_url", y.AppealUrl),
		formUrl:             abuseConfigHttpsUrl("form_url", y.FormUrl),
		rootNames:           abuseConfigNames(y.ProtectedNames.Roots),
		platformNames:       abuseConfigNames(y.ProtectedNames.Platform),
	}
	config.businessHours.Days = slices.Clone(y.BusinessHours.Days)
	config.businessHours.Holidays = slices.Clone(y.BusinessHours.Holidays)
	for field, value := range y.Pager {
		if field != "" && value != "" {
			config.pager[field] = value
		}
	}
	for _, feed := range y.Feeds {
		feedUrl := abuseConfigHttpsUrl("feed url", feed.Url)
		if feedUrl == "" {
			continue
		}
		config.feeds = append(config.feeds, AbuseFeed{Name: strings.TrimSpace(feed.Name), Url: feedUrl})
	}
	for domain, class := range y.NoticeSenders {
		switch class {
		case AbuseNoticeSenderRegistrar, AbuseNoticeSenderRegistry, AbuseNoticeSenderLawEnforcement:
		default:
			glog.Errorf("[abuse]%s lists a notice sender of an unknown class, so it pages nothing\n", abuseConfigResourceName)
			continue
		}
		if domain = abuseConfigDomain(domain); domain != "" {
			config.noticeSenders[domain] = class
		}
	}
	switch {
	case y.AppealWindow == 0:
		config.appealWindowMonths = AbuseAppealWindowMinMonths
	case y.AppealWindow < AbuseAppealWindowMinMonths:
		glog.Errorf("[abuse]%s sets an appeal_window under DSA Art. 20's %d months, so it reads %d\n", abuseConfigResourceName, AbuseAppealWindowMinMonths, AbuseAppealWindowMinMonths)
		config.appealWindowMonths = AbuseAppealWindowMinMonths
	default:
		config.appealWindowMonths = y.AppealWindow
	}
	return config
}

// Parses the text of an abuse.yml as a read of the file does.
func parseAbuseConfig(data []byte) (*AbuseConfig, error) {
	y := &abuseConfigYaml{}
	if err := yaml.Unmarshal(data, y); err != nil {
		return nil, err
	}
	return newAbuseConfig(y), nil
}

// Reads the file, keeping previous when the read fails or the file is gone.
// See the file's header for what a failure answers.
func loadAbuseConfig(previous *AbuseConfig) (config *AbuseConfig) {
	failed := func(err any) *AbuseConfig {
		glog.Errorf("[abuse]could not read %s: %v\n", abuseConfigResourceName, err)
		if previous != nil {
			return previous
		}
		// what an absent file reads: holds refuse and nothing routes
		return &AbuseConfig{}
	}
	if r := HandleError(func() {
		resource, err := Config.SimpleResource(abuseConfigResourceName)
		if err != nil {
			if !errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrResourceUnavailable) {
				config = failed(err)
				return
			}
			config = previous
			if config == nil {
				config = &AbuseConfig{}
			}
			return
		}
		y := &abuseConfigYaml{}
		if err := resource.UnmarshalYamlE(y); err != nil {
			config = failed(err)
			return
		}
		config = newAbuseConfig(y)
	}); r != nil {
		config = failed(r)
	}
	return
}

// A read and when it was taken.
type abuseConfigRead struct {
	config   *AbuseConfig
	readTime time.Time
}

// Answers from the latest read and rereads once it is older than the settings
// allow, one caller rereading while the rest keep the latest read. Safe for
// concurrent use.
type abuseConfigReader struct {
	settings  *AbuseConfigSettings
	now       func() time.Time
	load      func(previous *AbuseConfig) *AbuseConfig
	rereading atomic.Bool
	latest    atomic.Pointer[abuseConfigRead]
}

// A reader of the file on the wall clock.
func newAbuseConfigReader(settings *AbuseConfigSettings) *abuseConfigReader {
	return &abuseConfigReader{
		settings: settings,
		now:      time.Now,
		load:     loadAbuseConfig,
	}
}

// The latest read, taken again first when it is stale and no other caller is
// taking it.
func (self *abuseConfigReader) get() *AbuseConfig {
	now := self.now()
	read := self.latest.Load()
	if read != nil && now.Sub(read.readTime) < self.settings.RereadInterval {
		return read.config
	}
	if self.rereading.CompareAndSwap(false, true) {
		defer self.rereading.Store(false)
	} else if read != nil {
		return read.config
	}
	var previous *AbuseConfig
	if read != nil {
		previous = read.config
	}
	config := self.load(previous)
	self.latest.Store(&abuseConfigRead{config: config, readTime: now})
	return config
}

// The process's reader. The file is the process's, so it has one owner per
// process.
var defaultAbuseConfigReader = newAbuseConfigReader(DefaultAbuseConfigSettings())

// The current abuse.yml.
func GetAbuseConfig() *AbuseConfig {
	return defaultAbuseConfigReader.get()
}

// Stands in for abuse.yml until the returned pop. The next read answers with it
// at once, and the pop forgets it as quickly.
func Testing_PushAbuseConfig(yaml string) func() {
	pop := Config.PushSimpleResource(abuseConfigResourceName, []byte(yaml))
	defaultAbuseConfigReader.latest.Store(nil)
	return func() {
		pop()
		defaultAbuseConfigReader.latest.Store(nil)
	}
}
