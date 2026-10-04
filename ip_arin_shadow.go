package server

// Optional shadow lookup instrumentation. It never changes serving facts,
// stores an address, queries a database, or installs itself at startup.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

const (
	// The legacy observer copies each complete resource into the Go heap.
	arinShadowSnapshotFileLimit int64 = 512 << 20
	// Global origin provenance increased the fully built artifact to
	// 559,752,379 bytes. Capture hashes bounded streams and maps the exact
	// immutable descriptors, so it has a separate finite file budget.
	arinShadowCaptureFileLimit int64 = 1 << 30
)

type ArinShadowActiveFacts struct {
	Epoch                      int64
	At                         time.Time
	Risk, NonQuality, Verified bool
}

// False is a measured fact, not the default for a missing source field.
func (f *ArinShadowActiveFacts) UnmarshalJSON(data []byte) error {
	var input struct {
		Epoch                      *int64
		At                         *time.Time
		Risk, NonQuality, Verified *bool
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Epoch == nil || input.At == nil || input.Risk == nil || input.NonQuality == nil || input.Verified == nil {
		return ErrArinShadowInput
	}
	*f = ArinShadowActiveFacts{Epoch: *input.Epoch, At: *input.At, Risk: *input.Risk, NonQuality: *input.NonQuality, Verified: *input.Verified}
	return nil
}

type arinShadowFacts struct {
	state                     string
	risk, proxyRisk, verified bool
	registration              ArinShadowRegistration
	origin                    ArinShadowOrigin
}
type arinShadowConnection struct {
	actual            ArinShadowActiveFacts
	active, candidate arinShadowFacts
}
type ArinShadowRecorder struct {
	mu                        sync.RWMutex
	active, candidate         *mmdb.Reader
	activeHash, candidateHash string
	connections               map[string]arinShadowConnection
	start                     time.Time
	capacity                  int
	dropped, failed           int64
	closed                    bool
	clock                     func() time.Time
}

var arinShadowObserver atomic.Pointer[ArinShadowRecorder]

// Both paths and hashes must come from independently attested immutable
// resources. An enabled observer still cannot establish census completeness.
func OpenArinShadowRecorder(activePath, activeHash, candidatePath, candidateHash string, start time.Time, capacity int) (*ArinShadowRecorder, error) {
	return openArinShadowRecorder(activePath, activeHash, candidatePath, candidateHash, start, capacity, false)
}

// Capture resources must be independently pinned immutable files. Linux maps
// the same open descriptor which was hashed, avoiding one whole-file Go-heap
// copy per process. Mapping never changes the serving reader or resource.
func OpenArinShadowCaptureRecorder(activePath, activeHash, candidatePath, candidateHash string, start time.Time, capacity int) (*ArinShadowRecorder, error) {
	return openArinShadowRecorder(activePath, activeHash, candidatePath, candidateHash, start, capacity, true)
}

func openArinShadowRecorder(activePath, activeHash, candidatePath, candidateHash string, start time.Time, capacity int, mapped bool) (*ArinShadowRecorder, error) {
	if start.IsZero() || capacity < 1 || capacity > 2000000 {
		return nil, ErrArinShadowInput
	}
	fileLimit := arinShadowSnapshotFileLimit
	if mapped {
		fileLimit = arinShadowCaptureFileLimit
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
		if err != nil || !metadata.Mode().IsRegular() || metadata.Size() > fileLimit {
			return nil, ErrArinShadowInput
		}
		if mapped {
			h := sha256.New()
			n, err := io.Copy(h, io.LimitReader(file, fileLimit+1))
			if err != nil || n != metadata.Size() || !slices.Equal(h.Sum(nil), digest) {
				return nil, ErrArinShadowInput
			}
			db, err := openMappedArinShadowFile(file)
			after, statErr := file.Stat()
			if err != nil {
				return nil, ErrArinShadowInput
			}
			// This hash must name the exact artifact already fully verified
			// by the offline build/readback gate. Repeating Verify traverses
			// the whole database per Connect process and defeats a bounded
			// capture; per-address decode failures remain explicit unknowns.
			if statErr != nil || !os.SameFile(metadata, after) || metadata.Size() != after.Size() || !metadata.ModTime().Equal(after.ModTime()) || db.Metadata.DatabaseType != string(schemaTypeArinDb) || db.Metadata.BuildTime().Unix() <= 0 {
				db.Close()
				return nil, ErrArinShadowInput
			}
			return db, nil
		}
		data, err := io.ReadAll(io.LimitReader(file, arinShadowSnapshotFileLimit+1))
		if err != nil || int64(len(data)) > arinShadowSnapshotFileLimit {
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
	return &ArinShadowRecorder{active: active, candidate: candidate, activeHash: activeHash, candidateHash: candidateHash, connections: map[string]arinShadowConnection{}, start: start, capacity: capacity, clock: NowUtc}, nil
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
		OrgHandle               string   `maxminddb:"org_handle"`
		NetHandle               string   `maxminddb:"net_handle"`
		ClassificationOrgHandle string   `maxminddb:"classification_org_handle"`
		ClassificationRule      string   `maxminddb:"classification_rule"`
		MultipleOwners          bool     `maxminddb:"multiple_registration_owners"`
		OriginASNs              []uint32 `maxminddb:"origin_asns"`
		OriginUseState          string   `maxminddb:"origin_use_state"`
		Evidence                []struct {
			Category string `maxminddb:"category"`
		} `maxminddb:"network_risk_evidence"`
	}
	if err := db.Lookup(address).Decode(&extra); err != nil {
		return arinShadowFacts{}, nil, ErrArinShadowInput
	}
	// Only a single public registration can be attributed without inventing
	// an owner for a multi-owner record. The classification still retains all
	// original multi-owner/ambiguity semantics; missing attribution is counted.
	if !extra.MultipleOwners {
		facts.registration = newArinShadowRegistration(extra.OrgHandle, extra.NetHandle, extra.ClassificationOrgHandle, extra.ClassificationRule)
	}
	facts.origin = newArinShadowOrigin(extra.OriginASNs, extra.OriginUseState)
	for _, e := range extra.Evidence {
		if slices.Contains([]string{"proxy", "residential_proxy", "virtual_isp", "vpn", "tor"}, e.Category) {
			facts.proxyRisk = true
		}
	}
	if facts.proxyRisk && !facts.risk {
		return arinShadowFacts{}, nil, ErrArinShadowInput
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
	r.connections[connection] = arinShadowConnection{actual: actual, active: active, candidate: candidate}
	return true
}

// Census identities must come from the same source-owned live public-provider
// snapshot. They are input only; no identifier is included in the result.
type ArinShadowProvider struct {
	Token       string   `json:"token"`
	Connections []string `json:"connections"`
	// These are the current durable facts from the same live census, not a
	// copy made by the observer. A reused identity alone cannot join evidence.
	Lookups       map[string]ArinShadowActiveFacts `json:"lookups"`
	Buckets       []string                         `json:"buckets"`
	BaseQuality   bool                             `json:"base_quality"`
	BaseSpeed     bool                             `json:"base_speed"`
	ActiveQuality bool                             `json:"active_quality"`
	ActiveSpeed   bool                             `json:"active_speed"`
}
type ArinShadowBucket struct {
	Bucket               string `json:"bucket"`
	Providers            int64  `json:"providers"`
	CompleteLookups      int64  `json:"complete_lookup_providers"`
	MissingOrStale       int64  `json:"missing_or_stale_providers"`
	VerifiedSubscriber   int64  `json:"verified_subscriber"`
	Excluded             int64  `json:"excluded"`
	Unknown              int64  `json:"unknown"`
	Ambiguous            int64  `json:"ambiguous"`
	Risk                 int64  `json:"risk"`
	ProxyRisk            int64  `json:"proxy_risk"`
	ActiveQuality        int64  `json:"active_quality_supply"`
	CandidateQuality     int64  `json:"candidate_quality_supply"`
	QualityAdded         int64  `json:"quality_added"`
	QualityRemoved       int64  `json:"quality_removed"`
	QualityIndeterminate int64  `json:"quality_indeterminate"`
	ActiveSpeed          int64  `json:"active_speed_supply"`
	CandidateSpeed       int64  `json:"candidate_speed_supply"`
	SpeedIndeterminate   int64  `json:"speed_indeterminate"`
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
	// Public country codes and the aggregate scope are sufficient for this
	// matrix. Never echo caller-supplied provider/operator identifiers as labels.
	return value == "all" || len(value) == 2 && value[0] >= 'a' && value[0] <= 'z' && value[1] >= 'a' && value[1] <= 'z'
}

func sameArinShadowFacts(a, b ArinShadowActiveFacts) bool {
	return a.Epoch == b.Epoch && a.At.Equal(b.At) && a.Risk == b.Risk && a.NonQuality == b.NonQuality && a.Verified == b.Verified
}

// A complete required-bucket matrix includes empty buckets. Classification
// counts partition providers (ambiguous > excluded > unknown > subscriber);
// risk is an independent overlapping count. Missing connections are unknown.
func (r *ArinShadowRecorder) Snapshot(census []ArinShadowProvider, required []string, complete bool, maxAge time.Duration) (ArinShadowReport, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	report := ArinShadowReport{At: r.clock(), CensusComplete: complete, DroppedCapacity: r.dropped, FailedObservations: r.failed, Buckets: []ArinShadowBucket{}}
	if r.closed || report.At.Before(r.start) || len(required) == 0 || len(required) > 10000 || len(census) > r.capacity || maxAge <= 0 || maxAge > 90*time.Second {
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
		if p.Token == "" || len(p.Token) > 128 || providers[p.Token] || len(p.Connections) == 0 || len(p.Connections) > r.capacity-len(connections) || len(p.Buckets) == 0 || p.ActiveQuality && !p.BaseQuality || p.ActiveSpeed && !p.BaseSpeed {
			return report, ErrArinShadowInput
		}
		providers[p.Token] = true
		providerConnections := make(map[string]bool, len(p.Connections))
		for _, connection := range p.Connections {
			providerConnections[connection] = true
		}
		for connection := range p.Lookups {
			if !providerConnections[connection] {
				return report, ErrArinShadowInput
			}
		}
		providerComplete := true
		verified := true
		risk, proxy, excluded, unknown, ambiguous := false, false, false, false, false
		for _, connection := range p.Connections {
			if connection == "" || len(connection) > 128 || connections[connection] || len(connections) >= r.capacity {
				return report, ErrArinShadowInput
			}
			connections[connection] = true
			observed, ok := r.connections[connection]
			expected, joined := p.Lookups[connection]
			if !ok || !joined || !sameArinShadowFacts(observed.actual, expected) || observed.actual.At.Before(r.start) || observed.actual.At.Before(report.At.Add(-maxAge)) || observed.actual.At.After(report.At) {
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
			if !providerComplete {
				row.QualityIndeterminate++
				row.SpeedIndeterminate++
			}
			if providerComplete && p.ActiveQuality && !candidateQuality {
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
