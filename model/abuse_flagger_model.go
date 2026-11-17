package model

// §21.8's trusted flaggers (ABUSE-M8-01; D-ABUSE-3, D-ABUSE-5, D-ABUSE-1, I-13; QD-111): bodies whose report
// naming a url takes the machine's host kill, reviewed within 24 h, through the ordinary path.
//
//   - abuse.yml lists each flagger by id (trusted_flaggers: [{id}]); vault's abuse-flaggers.yml holds, per id, the
//     SHA-256 of its 256-bit key, never a key. POST /abuse/report takes the key in X-UR-Flagger-Key, and
//     AbuseFlaggerVerifier.Authenticate compares its digest with every listed entry's in constant time, with no early
//     exit. Only an id abuse.yml lists counts, so removing the line revokes the flagger at the file's next read.
//   - A match sets reporter class trusted_flagger, and the case keeps the flagger's id as its class,
//     trusted_flagger:<id> (AbuseFlaggerReporterClass): a key names a body, not a person, so no address is kept, and
//     a kill's review shows who flagged it.
//   - A wrong or absent key falls back to a plain report, and the route's answer does not say which. Past
//     KillsPerDay machine kills in a KillWindow, a flagger's reports take §21.8's report row too
//     (AbuseFlaggerReportClass), so a leaked key kills at most a day's cap before its line is removed.
//   - Only a report naming a url acts (AbuseSignalOfReport), host level first: a second strike escalates through
//     NextAbuseStep and never through flagger status, and a flagger's kill is appealable like any other.
//
// Nothing here logs a key, a digest or an id beside a network (I-13).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

// api's vault file of flagger key digests.
const AbuseFlaggerFileName = "abuse-flaggers.yml"

// The header POST /abuse/report takes a flagger's key in.
const AbuseFlaggerKeyHeader = "X-UR-Flagger-Key"

// What prefixes a flagger's id in its case's reporter class.
const abuseFlaggerReporterClassPrefix = string(AbuseReporterClassTrustedFlagger) + ":"

// Tunables of the flaggers.
type AbuseFlaggerSettings struct {
	// QD-111's flagger_kills_per_day: one flagger's machine kills in KillWindow, past which its reports take §21.8's
	// report row. 20; abuse.yml's flagger_kills_per_day replaces it, and 0 lets no flagger kill.
	KillsPerDay int
	// The window KillsPerDay counts over: a day.
	KillWindow time.Duration
	// How long one read of the vault file and abuse.yml's list answers before the next: 60 s, as abuse.yml's own
	// reread, so a removed line revokes within a minute.
	ReloadInterval time.Duration
}

// QD-111's numbers.
func DefaultAbuseFlaggerSettings() *AbuseFlaggerSettings {
	return &AbuseFlaggerSettings{
		KillsPerDay:    20,
		KillWindow:     24 * time.Hour,
		ReloadInterval: 60 * time.Second,
	}
}

// The settings with abuse.yml's flagger_kills_per_day laid over them.
func AbuseFlaggerSettingsOf(config *server.AbuseConfig) *AbuseFlaggerSettings {
	settings := DefaultAbuseFlaggerSettings()
	if killsPerDay, ok := config.FlaggerKillsPerDay(); ok {
		settings.KillsPerDay = killsPerDay
	}
	return settings
}

// A verify file that cannot be used. The text names the part, never a digest.
var ErrAbuseFlaggerFileRefused = errors.New("abuse flagger file refused")

// The vault file, as written:
//
//	flaggers:
//	  - id: <the id abuse.yml lists>
//	    sha256: <hex of the key's SHA-256>
type abuseFlaggerFileYaml struct {
	Flaggers []struct {
		Id     string `yaml:"id"`
		Sha256 string `yaml:"sha256"`
	} `yaml:"flaggers"`
}

// One flagger a verifier holds.
type abuseFlaggerEntry struct {
	id     string
	digest [sha256.Size]byte
}

// The flaggers whose keys a report may carry: the vault file's digests of the ids abuse.yml lists. Safe for
// concurrent use; no I/O.
type AbuseFlaggerVerifier struct {
	entries []abuseFlaggerEntry
}

// Reads the vault file, keeping only the entries listedIds names. A file that does not parse, or an entry that is
// malformed, refuses the file whole: a broken push is caught when it is pushed, and every key falls back meanwhile.
func NewAbuseFlaggerVerifier(data []byte, listedIds []string) (*AbuseFlaggerVerifier, error) {
	refuse := func(format string, arguments ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrAbuseFlaggerFileRefused, AbuseFlaggerFileName, fmt.Sprintf(format, arguments...))
	}
	file := &abuseFlaggerFileYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(file); err != nil {
		return nil, refuse("%v", err)
	}
	self := &AbuseFlaggerVerifier{}
	seen := map[string]bool{}
	for index, flagger := range file.Flaggers {
		id := strings.TrimSpace(flagger.Id)
		if !server.IsAbuseFlaggerId(id) || seen[id] {
			return nil, refuse("entry %d names no id, or one named twice", index)
		}
		seen[id] = true
		digest, err := hex.DecodeString(strings.TrimSpace(flagger.Sha256))
		if err != nil || len(digest) != sha256.Size {
			return nil, refuse("entry %d holds a digest that is not SHA-256 in hex", index)
		}
		if !slices.Contains(listedIds, id) {
			// a line removed from abuse.yml revokes the flagger
			continue
		}
		self.entries = append(self.entries, abuseFlaggerEntry{id: id, digest: [sha256.Size]byte(digest)})
	}
	return self, nil
}

// The flagger whose key this is: its digest compared with every entry's in constant time, with no early exit, so the
// time taken says nothing of which entry, if any, matched. false for an empty key and for no match.
func (self *AbuseFlaggerVerifier) Authenticate(key string) (flaggerId string, ok bool) {
	if self == nil || key == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(key))
	matchIndex := -1
	for index := range self.entries {
		if subtle.ConstantTimeCompare(digest[:], self.entries[index].digest[:]) == 1 {
			matchIndex = index
		}
	}
	if matchIndex < 0 {
		return "", false
	}
	return self.entries[matchIndex].id, true
}

// api's verifier from its vault and abuse.yml's list. An error names the part; the caller then takes every key as
// none.
func LoadAbuseFlaggerVerifier() (*AbuseFlaggerVerifier, error) {
	listedIds := server.GetAbuseConfig().TrustedFlaggerIds()
	if len(listedIds) == 0 {
		return &AbuseFlaggerVerifier{}, nil
	}
	resource, err := server.Vault.SimpleResource(AbuseFlaggerFileName)
	if err != nil {
		return nil, fmt.Errorf("%w: no %s: %v", ErrAbuseFlaggerFileRefused, AbuseFlaggerFileName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrAbuseFlaggerFileRefused, AbuseFlaggerFileName, err)
	}
	return NewAbuseFlaggerVerifier(data, listedIds)
}

// A verifier read at most once a ReloadInterval. Safe for concurrent use.
type AbuseFlaggerSource struct {
	load     func() (*AbuseFlaggerVerifier, error)
	settings *AbuseFlaggerSettings

	stateLock sync.Mutex
	verifier  *AbuseFlaggerVerifier
	loadTime  time.Time
	loaded    bool
}

// A source over load, LoadAbuseFlaggerVerifier when nil.
func NewAbuseFlaggerSource(load func() (*AbuseFlaggerVerifier, error), settings *AbuseFlaggerSettings) *AbuseFlaggerSource {
	if load == nil {
		load = LoadAbuseFlaggerVerifier
	}
	if settings == nil {
		settings = DefaultAbuseFlaggerSettings()
	}
	return &AbuseFlaggerSource{load: load, settings: settings}
}

// The verifier in force at now: a fresh read once the last is ReloadInterval old. A read that fails answers a
// verifier of no flagger, so every key falls back to a plain report, the closed direction.
func (self *AbuseFlaggerSource) Verifier(now time.Time) *AbuseFlaggerVerifier {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.loaded && now.Sub(self.loadTime) < self.settings.ReloadInterval {
		return self.verifier
	}
	verifier, err := self.load()
	if err != nil {
		verifier = &AbuseFlaggerVerifier{}
	}
	self.verifier, self.loadTime, self.loaded = verifier, now, true
	return verifier
}

// The reporter class a flagger's case keeps: trusted_flagger:<id>.
func AbuseFlaggerReporterClass(flaggerId string) AbuseReporterClass {
	return AbuseReporterClass(abuseFlaggerReporterClassPrefix + flaggerId)
}

// The flagger a case's class names, ok false for any other class.
func AbuseFlaggerIdOf(class AbuseReporterClass) (string, bool) {
	flaggerId, found := strings.CutPrefix(string(class), abuseFlaggerReporterClassPrefix)
	return flaggerId, found && server.IsAbuseFlaggerId(flaggerId)
}

// The flagger's machine kills since since: host kills a machine recorded on cases its reports opened.
func AbuseFlaggerKillsSince(ctx context.Context, flaggerId string, since time.Time) (count int) {
	if !server.SidecarMg5Linked() {
		return 0
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*)
				FROM abuse_action
				INNER JOIN abuse_case ON abuse_case.case_id = abuse_action.case_id
				WHERE
					abuse_case.reporter_class = $1 AND
					abuse_action.kind = $2 AND
					abuse_action.by_machine AND
					$3 <= abuse_action.create_time
			`,
			string(AbuseFlaggerReporterClass(flaggerId)),
			string(AbuseActionKindHostKill),
			since.UTC(),
		).Scan(&count))
	})
	return
}

// The class a flagger's report takes at now: trusted_flagger while its kills in KillWindow stay under KillsPerDay,
// else public, §21.8's report row, the flagger dropped from the report.
func AbuseFlaggerReportClass(ctx context.Context, flaggerId string, now time.Time, settings *AbuseFlaggerSettings) (AbuseReporterClass, string) {
	if settings.KillsPerDay <= 0 || settings.KillsPerDay <= AbuseFlaggerKillsSince(ctx, flaggerId, now.Add(-settings.KillWindow)) {
		return AbuseReporterClassPublic, ""
	}
	return AbuseReporterClassTrustedFlagger, flaggerId
}
