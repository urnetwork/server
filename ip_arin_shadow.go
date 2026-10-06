package server

// Optional shadow lookup instrumentation. It never changes serving facts,
// stores an address, queries a database, or installs itself at startup.
import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"io"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

var ErrArinShadowInput = errors.New("ARIN shadow input is unavailable or inconsistent")

type ArinShadowActiveFacts struct {
	Epoch                      int64
	At                         time.Time
	Risk, NonQuality, Verified bool
}
type arinShadowFacts struct {
	state                     string
	risk, proxyRisk, verified bool
}
type arinShadowConnection struct {
	at                time.Time
	active, candidate arinShadowFacts
}
type ArinShadowRecorder struct {
	mu                sync.RWMutex
	active, candidate *mmdb.Reader
	connections       map[string]arinShadowConnection
	start             time.Time
	capacity          int
	dropped, failed   int64
	closed            bool
	clock             func() time.Time
}

var arinShadowObserver atomic.Pointer[ArinShadowRecorder]

// Both paths and hashes must come from independently attested immutable
// resources. An enabled observer still cannot establish census completeness.
func OpenArinShadowRecorder(activePath, activeHash, candidatePath, candidateHash string, start time.Time, capacity int) (*ArinShadowRecorder, error) {
	if start.IsZero() || capacity < 1 || capacity > 2000000 {
		return nil, ErrArinShadowInput
	}
	open := func(path, pin string) (*mmdb.Reader, error) {
		digest, err := hex.DecodeString(pin)
		if err != nil || len(digest) != sha256.Size {
			return nil, ErrArinShadowInput
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, ErrArinShadowInput
		}
		defer file.Close()
		metadata, err := file.Stat()
		if err != nil || !metadata.Mode().IsRegular() || metadata.Size() > 512<<20 {
			return nil, ErrArinShadowInput
		}
		data, err := io.ReadAll(io.LimitReader(file, (512<<20)+1))
		if err != nil || len(data) > 512<<20 {
			return nil, ErrArinShadowInput
		}
		actual := sha256.Sum256(data)
		if !slices.Equal(actual[:], digest) {
			return nil, ErrArinShadowInput
		}
		db, err := mmdb.OpenBytes(data)
		if err != nil {
			return nil, ErrArinShadowInput
		}
		if db.Metadata.DatabaseType != string(schemaTypeArinDb) || db.Metadata.BuildTime().Unix() <= 0 || db.Verify() != nil {
			db.Close()
			return nil, ErrArinShadowInput
		}
		return db, nil
	}
	active, err := open(activePath, activeHash)
	if err != nil {
		return nil, err
	}
	candidate, err := open(candidatePath, candidateHash)
	if err != nil {
		active.Close()
		return nil, err
	}
	return &ArinShadowRecorder{active: active, candidate: candidate, connections: map[string]arinShadowConnection{}, start: start, capacity: capacity, clock: NowUtc}, nil
}

// Installation is explicit and default-off. Call only after source/resource
// review; neither this library nor the CLI publishes configuration or a resource.
func InstallArinShadowRecorder(recorder *ArinShadowRecorder) { arinShadowObserver.Store(recorder) }
func ObserveArinShadowConnection(connection string, address string, actual ArinShadowActiveFacts) {
	if r := arinShadowObserver.Load(); r != nil {
		r.Observe(connection, address, actual)
	}
}
func (r *ArinShadowRecorder) Close() {
	arinShadowObserver.CompareAndSwap(r, nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	r.active.Close()
	r.candidate.Close()
	r.connections = nil
}
func shadowFacts(db *mmdb.Reader, address netip.Addr) (arinShadowFacts, *ArinInfo, error) {
	info, err := getArinInfoFromDatabase(db, schemaTypeArinDb, address)
	if err != nil {
		return arinShadowFacts{}, nil, ErrArinShadowInput
	}
	state := info.QualityState
	if state == "" {
		state = "unknown"
	}
	facts := arinShadowFacts{state: state, risk: info.Risk, verified: info.ClassifierVersion == 1 && info.QualityPolicyVersion == 2 && state == "subscriber" && !info.NonQuality}
	var extra struct {
		Evidence []struct {
			Category string `maxminddb:"category"`
		} `maxminddb:"network_risk_evidence"`
	}
	if err := db.Lookup(address).Decode(&extra); err != nil {
		return arinShadowFacts{}, nil, ErrArinShadowInput
	}
	for _, e := range extra.Evidence {
		if slices.Contains([]string{"proxy", "residential_proxy", "virtual_isp", "vpn", "tor"}, e.Category) {
			facts.proxyRisk = true
		}
	}
	return facts, info, nil
}

// Only opaque connection keys and classification facts survive the call. A
// replaced connection key replaces its facts instead of duplicating supply.
func (r *ArinShadowRecorder) Observe(connection, address string, actual ArinShadowActiveFacts) bool {
	addr, err := netip.ParseAddr(address)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	fail := func() bool { delete(r.connections, connection); r.failed++; return false }
	if err != nil || connection == "" || len(connection) > 128 || actual.Epoch <= 0 {
		return fail()
	}
	now := r.clock()
	if now.Before(r.start) || actual.At.IsZero() || actual.At.Before(r.start) || actual.At.After(now) || now.Sub(actual.At) > 90*time.Second {
		return fail()
	}
	active, info, err := shadowFacts(r.active, addr.Unmap())
	if err != nil {
		return fail()
	}
	if info.DatabaseBuildEpoch != actual.Epoch || info.Risk != actual.Risk || info.NonQuality != actual.NonQuality || info.QualityVerified() != actual.Verified {
		return fail()
	}
	candidate, candidateInfo, err := shadowFacts(r.candidate, addr.Unmap())
	if err != nil {
		return fail()
	}
	// An evaluated no-record result is unknown, never a subscriber. A candidate
	// hit must explicitly carry policy two; unrelated policy-one bytes fail closed.
	if candidateInfo.QualityPolicyVersion != 2 {
		return fail()
	}
	if _, exists := r.connections[connection]; !exists && len(r.connections) >= r.capacity {
		r.dropped++
		return false
	}
	r.connections[connection] = arinShadowConnection{at: now, active: active, candidate: candidate}
	return true
}

// Census identities must come from the same source-owned live public-provider
// snapshot. They are input only; no identifier is included in the result.
type ArinShadowProvider struct {
	Token         string   `json:"token"`
	Connections   []string `json:"connections"`
	Buckets       []string `json:"buckets"`
	BaseQuality   bool     `json:"base_quality"`
	BaseSpeed     bool     `json:"base_speed"`
	ActiveQuality bool     `json:"active_quality"`
	ActiveSpeed   bool     `json:"active_speed"`
}
type ArinShadowBucket struct {
	Bucket             string `json:"bucket"`
	Providers          int64  `json:"providers"`
	CompleteLookups    int64  `json:"complete_lookup_providers"`
	MissingOrStale     int64  `json:"missing_or_stale_providers"`
	VerifiedSubscriber int64  `json:"verified_subscriber"`
	Excluded           int64  `json:"excluded"`
	Unknown            int64  `json:"unknown"`
	Ambiguous          int64  `json:"ambiguous"`
	Risk               int64  `json:"risk"`
	ProxyRisk          int64  `json:"proxy_risk"`
	ActiveQuality      int64  `json:"active_quality_supply"`
	CandidateQuality   int64  `json:"candidate_quality_supply"`
	QualityAdded       int64  `json:"quality_added"`
	QualityRemoved     int64  `json:"quality_removed"`
	ActiveSpeed        int64  `json:"active_speed_supply"`
	CandidateSpeed     int64  `json:"candidate_speed_supply"`
}
type ArinShadowReport struct {
	At                  time.Time          `json:"at"`
	ActualMainCoverage  bool               `json:"actual_main_coverage"`
	CensusComplete      bool               `json:"census_complete"`
	ObservationComplete bool               `json:"observation_complete"`
	DroppedCapacity     int64              `json:"dropped_capacity"`
	FailedObservations  int64              `json:"failed_observations"`
	Buckets             []ArinShadowBucket `json:"buckets"`
}

func validShadowBucket(value string) bool {
	if len(value) == 0 || len(value) > 80 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == ':') {
			return false
		}
	}
	return true
}

// A complete required-bucket matrix includes empty buckets. Classification
// counts partition providers (ambiguous > excluded > unknown > subscriber);
// risk is an independent overlapping count. Missing connections are unknown.
func (r *ArinShadowRecorder) Snapshot(census []ArinShadowProvider, required []string, complete bool, maxAge time.Duration) (ArinShadowReport, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	report := ArinShadowReport{At: r.clock(), CensusComplete: complete, DroppedCapacity: r.dropped, FailedObservations: r.failed, Buckets: []ArinShadowBucket{}}
	if r.closed || len(required) == 0 || len(required) > 10000 || len(census) > r.capacity || maxAge <= 0 || maxAge > 90*time.Second {
		return report, ErrArinShadowInput
	}
	rows := map[string]*ArinShadowBucket{}
	for _, bucket := range required {
		if !validShadowBucket(bucket) || rows[bucket] != nil {
			return report, ErrArinShadowInput
		}
		rows[bucket] = &ArinShadowBucket{Bucket: bucket}
	}
	providers := map[string]bool{}
	connections := map[string]bool{}
	allComplete := true
	for _, p := range census {
		if p.Token == "" || providers[p.Token] || len(p.Connections) == 0 || len(p.Buckets) == 0 {
			return report, ErrArinShadowInput
		}
		providers[p.Token] = true
		providerComplete := true
		verified := true
		risk, proxy, excluded, unknown, ambiguous := false, false, false, false, false
		for _, connection := range p.Connections {
			if connection == "" || connections[connection] {
				return report, ErrArinShadowInput
			}
			connections[connection] = true
			observed, ok := r.connections[connection]
			if !ok || observed.at.Before(r.start) || observed.at.Before(report.At.Add(-maxAge)) || observed.at.After(report.At) {
				providerComplete = false
				unknown = true
				verified = false
				continue
			}
			f := observed.candidate
			verified = verified && f.verified
			risk = risk || f.risk
			proxy = proxy || f.proxyRisk
			excluded = excluded || f.state == "excluded"
			unknown = unknown || f.state == "unknown"
			ambiguous = ambiguous || f.state == "ambiguous"
		}
		if !providerComplete {
			allComplete = false
		}
		candidateQuality := providerComplete && p.BaseQuality && verified && !risk
		candidateSpeed := providerComplete && p.BaseSpeed && !risk
		seen := map[string]bool{}
		for _, bucket := range p.Buckets {
			row := rows[bucket]
			if row == nil || seen[bucket] {
				return report, ErrArinShadowInput
			}
			seen[bucket] = true
			row.Providers++
			if providerComplete {
				row.CompleteLookups++
			} else {
				row.MissingOrStale++
			}
			if ambiguous {
				row.Ambiguous++
			} else if excluded {
				row.Excluded++
			} else if unknown || !verified {
				row.Unknown++
			} else {
				row.VerifiedSubscriber++
			}
			if risk {
				row.Risk++
			}
			if proxy {
				row.ProxyRisk++
			}
			if p.ActiveQuality {
				row.ActiveQuality++
			}
			if candidateQuality {
				row.CandidateQuality++
			}
			if p.ActiveQuality && !candidateQuality {
				row.QualityRemoved++
			}
			if !p.ActiveQuality && candidateQuality {
				row.QualityAdded++
			}
			if p.ActiveSpeed {
				row.ActiveSpeed++
			}
			if candidateSpeed {
				row.CandidateSpeed++
			}
		}
	}
	keys := slices.Clone(required)
	slices.Sort(keys)
	for _, key := range keys {
		report.Buckets = append(report.Buckets, *rows[key])
	}
	report.ObservationComplete = complete && allComplete && r.dropped == 0
	// Only an external reviewed source receipt can qualify Main identity. A
	// local snapshot alone never makes that claim, including synthetic input.
	return report, nil
}
