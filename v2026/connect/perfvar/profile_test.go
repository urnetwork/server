// This file defines the resolved, hashable userspace network profiles shared
// by PERFVAR's simulator, route fixtures, and measurement records.
package perfvar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

const (
	singleRegionMinimumRoundTrip = 500 * time.Millisecond
	singleRegionMaximumRoundTrip = time.Second
)

// A stable label for the loss decision applied to one link direction.
type lossModel string

const (
	lossModelNone        lossModel = "none"
	lossModelIndependent lossModel = "independent"
	lossModelEveryN      lossModel = "every-n"
	lossModelBurst       lossModel = "burst"
)

// The observable behavior when an outer packet exceeds the link MTU.
type oversizeMode string

const (
	oversizeModeDrop  oversizeMode = "drop"
	oversizeModeError oversizeMode = "error"
)

// The two-state good/bad loss process used for clustered mobile loss.
type burstLossProfile struct {
	GoodToBadProbability float64 `json:"good_to_bad_probability"`
	BadToGoodProbability float64 `json:"bad_to_good_probability"`
	GoodLossProbability  float64 `json:"good_loss_probability"`
	BadLossProbability   float64 `json:"bad_loss_probability"`
}

// One independently scheduled direction of a simulated network path.
type linkProfile struct {
	RateBitsPerSecond    int64             `json:"rate_bits_per_second"`
	BurstByteCount       int               `json:"burst_byte_count"`
	QueueByteCount       int               `json:"queue_byte_count"`
	QueuePacketCount     int               `json:"queue_packet_count"`
	BaseDelay            time.Duration     `json:"base_delay_nanoseconds"`
	Jitter               time.Duration     `json:"jitter_nanoseconds"`
	LossModel            lossModel         `json:"loss_model"`
	LossProbability      float64           `json:"loss_probability"`
	DropEveryPacketCount uint64            `json:"drop_every_packet_count"`
	BurstLoss            *burstLossProfile `json:"burst_loss,omitempty"`
	DuplicateProbability float64           `json:"duplicate_probability"`
	ReorderProbability   float64           `json:"reorder_probability"`
	OuterMtu             int               `json:"outer_mtu"`
	OversizeMode         oversizeMode      `json:"oversize_mode"`
	Blackhole            bool              `json:"blackhole"`
	ProcessingDelay      time.Duration     `json:"processing_delay_nanoseconds"`
	AllowQueueDrops      bool              `json:"allow_queue_drops"`
	AllowMtuDrops        bool              `json:"allow_mtu_drops"`
}

// A complete bidirectional scenario definition with a replay seed.
type networkProfile struct {
	Name       string      `json:"name"`
	Seed       int64       `json:"seed"`
	InnerMtu   int         `json:"inner_mtu"`
	Forward    linkProfile `json:"forward"`
	Reverse    linkProfile `json:"reverse"`
	SourceNote string      `json:"source_note"`
}

// A live event changes one or both directions after time or byte progress.
type profileEvent struct {
	Name                string
	After               time.Duration
	AfterDeliveredBytes uint64
	Forward             *linkProfile
	Reverse             *linkProfile
	Rebind              bool
	Kick                bool
}

// The queue target is expressed directly as a bounded bandwidth-delay product.
func bandwidthDelayQueue(rateBitsPerSecond int64, duration time.Duration) int {
	if rateBitsPerSecond <= 0 || duration <= 0 {
		return 0
	}
	byteCount := float64(rateBitsPerSecond) / 8 * duration.Seconds()
	return max(1500, int(math.Ceil(byteCount)))
}

// A common profile constructor keeps defaults explicit and consistent.
func newLinkProfile(
	rateBitsPerSecond int64,
	baseDelay time.Duration,
	jitter time.Duration,
	lossProbability float64,
	queueDuration time.Duration,
) linkProfile {
	profile := linkProfile{
		RateBitsPerSecond:    rateBitsPerSecond,
		BurstByteCount:       64 * 1024,
		QueueByteCount:       bandwidthDelayQueue(rateBitsPerSecond, queueDuration),
		QueuePacketCount:     4096,
		BaseDelay:            baseDelay,
		Jitter:               jitter,
		LossModel:            lossModelNone,
		LossProbability:      lossProbability,
		DuplicateProbability: 0,
		ReorderProbability:   0,
		OuterMtu:             1500,
		OversizeMode:         oversizeModeDrop,
		AllowQueueDrops:      true,
	}
	if 0 < lossProbability {
		profile.LossModel = lossModelIndependent
	}
	return profile
}

// Synthetic starting points match PERFVAR.md and are not field-network claims.
func initialNetworkProfiles(seed int64) map[string]networkProfile {
	cleanForward := newLinkProfile(1_000_000_000, time.Millisecond, 0, 0, 10*time.Millisecond)
	cleanForward.BurstByteCount = 256 * 1024
	// Pion's userspace token-bucket queue drops overflow silently. The clean
	// control must be large enough to absorb PERFVAR's default measured burst
	// so an unobservable simulator drop is never mistaken for carrier loss.
	cleanForward.QueueByteCount = 32 * 1024 * 1024
	cleanForward.QueuePacketCount = 64 * 1024
	cleanForward.AllowQueueDrops = false
	cleanReverse := cleanForward

	wifiForward := newLinkProfile(500_000_000, 10*time.Millisecond, 3*time.Millisecond, 0.0005, 50*time.Millisecond)
	wifiReverse := newLinkProfile(100_000_000, 10*time.Millisecond, 3*time.Millisecond, 0.0005, 50*time.Millisecond)

	lteForward := newLinkProfile(50_000_000, 30*time.Millisecond, 10*time.Millisecond, 0.005, 100*time.Millisecond)
	lteReverse := newLinkProfile(10_000_000, 30*time.Millisecond, 10*time.Millisecond, 0.005, 100*time.Millisecond)

	poorForward := newLinkProfile(10_000_000, 60*time.Millisecond, 25*time.Millisecond, 0, 200*time.Millisecond)
	poorReverse := newLinkProfile(2_000_000, 60*time.Millisecond, 25*time.Millisecond, 0, 200*time.Millisecond)
	for _, profile := range []*linkProfile{&poorForward, &poorReverse} {
		profile.LossModel = lossModelBurst
		profile.BurstLoss = &burstLossProfile{
			GoodToBadProbability: 0.01,
			BadToGoodProbability: 0.35,
			GoodLossProbability:  0.002,
			BadLossProbability:   0.65,
		}
	}

	wanForward := newLinkProfile(300_000_000, 50*time.Millisecond, 5*time.Millisecond, 0.001, 100*time.Millisecond)
	wanReverse := newLinkProfile(100_000_000, 50*time.Millisecond, 5*time.Millisecond, 0.001, 100*time.Millisecond)

	regional500Forward := newLinkProfile(
		100_000_000,
		singleRegionMinimumRoundTrip/2,
		0,
		0,
		singleRegionMinimumRoundTrip,
	)
	regional500Reverse := regional500Forward
	regional1000Forward := newLinkProfile(
		100_000_000,
		singleRegionMaximumRoundTrip/2,
		0,
		0,
		singleRegionMaximumRoundTrip,
	)
	regional1000Reverse := regional1000Forward

	profiles := map[string]networkProfile{
		"clean-lan": {
			Name:       "clean-lan",
			Seed:       seed,
			InnerMtu:   1440,
			Forward:    cleanForward,
			Reverse:    cleanReverse,
			SourceNote: "synthetic same-host clean LAN",
		},
		"wifi-good": {
			Name:       "wifi-good",
			Seed:       seed,
			InnerMtu:   1440,
			Forward:    wifiForward,
			Reverse:    wifiReverse,
			SourceNote: "synthetic good Wi-Fi",
		},
		"lte": {
			Name:       "lte",
			Seed:       seed,
			InnerMtu:   1400,
			Forward:    lteForward,
			Reverse:    lteReverse,
			SourceNote: "synthetic LTE",
		},
		"mobile-poor": {
			Name:       "mobile-poor",
			Seed:       seed,
			InnerMtu:   1280,
			Forward:    poorForward,
			Reverse:    poorReverse,
			SourceNote: "synthetic poor mobile path with clustered loss",
		},
		"wan": {
			Name:       "wan",
			Seed:       seed,
			InnerMtu:   1400,
			Forward:    wanForward,
			Reverse:    wanReverse,
			SourceNote: "synthetic asymmetric WAN",
		},
		"single-region-500ms-rtt": {
			Name:       "single-region-500ms-rtt",
			Seed:       seed,
			InnerMtu:   1400,
			Forward:    regional500Forward,
			Reverse:    regional500Reverse,
			SourceNote: "constant 250 ms each way; 500 ms application-user-to-connect RTT",
		},
		"single-region-1000ms-rtt": {
			Name:       "single-region-1000ms-rtt",
			Seed:       seed,
			InnerMtu:   1400,
			Forward:    regional1000Forward,
			Reverse:    regional1000Reverse,
			SourceNote: "constant 500 ms each way; 1 s application-user-to-connect RTT",
		},
	}
	dual500 := profiles["single-region-500ms-rtt"]
	dual500.Name = "dual-region-500ms-rtt"
	dual500.SourceNote = "constant 500 ms user-to-connect RTT on both endpoint access paths"
	profiles[dual500.Name] = dual500
	dual1000 := profiles["single-region-1000ms-rtt"]
	dual1000.Name = "dual-region-1000ms-rtt"
	dual1000.SourceNote = "constant 1 s user-to-connect RTT on both endpoint access paths"
	profiles[dual1000.Name] = dual1000
	return profiles
}

// The clean control can admit an entire default transfer by both byte and
// packet bounds, so simulator recovery cannot define its measured ceiling.
func TestCleanProfileQueueCoversDefaultBulkPayload(t *testing.T) {
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	const payloadByteCount = 32 * 1024 * 1024
	for _, direction := range []linkProfile{profile.Forward, profile.Reverse} {
		if direction.QueueByteCount < payloadByteCount {
			t.Fatalf("clean queue bytes=%d payload=%d", direction.QueueByteCount, payloadByteCount)
		}
		maximumPayloadPacketCount := (payloadByteCount + profile.InnerMtu - 1) / profile.InnerMtu
		if direction.QueuePacketCount < maximumPayloadPacketCount {
			t.Fatalf(
				"clean queue packets=%d payload packets=%d",
				direction.QueuePacketCount,
				maximumPayloadPacketCount,
			)
		}
	}
}

// Focused profiles change one primary axis so a regression can be attributed
// without taking a full Cartesian product of all network properties.
func allNetworkProfiles(seed int64) map[string]networkProfile {
	profiles := initialNetworkProfiles(seed)
	for _, roundTripMilliseconds := range []int{0, 10, 25, 50, 100, 150} {
		name := fmt.Sprintf("rtt-%dms", roundTripMilliseconds)
		profiles[name] = focusedNetworkProfile(name, seed, func(forward *linkProfile, reverse *linkProfile) {
			oneWay := time.Duration(roundTripMilliseconds) * time.Millisecond / 2
			forward.BaseDelay = oneWay
			reverse.BaseDelay = oneWay
		})
	}
	for _, lossBasisPoints := range []int{0, 1, 10, 50, 100, 200} {
		name := fmt.Sprintf("loss-%dbp", lossBasisPoints)
		profiles[name] = focusedNetworkProfile(name, seed, func(forward *linkProfile, reverse *linkProfile) {
			probability := float64(lossBasisPoints) / 10_000
			for _, link := range []*linkProfile{forward, reverse} {
				link.LossProbability = probability
				if probability == 0 {
					link.LossModel = lossModelNone
				} else {
					link.LossModel = lossModelIndependent
				}
			}
		})
	}
	for _, jitterMilliseconds := range []int{0, 1, 5, 25} {
		name := fmt.Sprintf("jitter-%dms", jitterMilliseconds)
		profiles[name] = focusedNetworkProfile(name, seed, func(forward *linkProfile, reverse *linkProfile) {
			jitter := time.Duration(jitterMilliseconds) * time.Millisecond
			forward.Jitter = jitter
			reverse.Jitter = jitter
		})
	}
	for _, reorderBasisPoints := range []int{0, 10, 100, 500} {
		name := fmt.Sprintf("reorder-%dbp", reorderBasisPoints)
		profiles[name] = focusedNetworkProfile(name, seed, func(forward *linkProfile, reverse *linkProfile) {
			probability := float64(reorderBasisPoints) / 10_000
			forward.ReorderProbability = probability
			reverse.ReorderProbability = probability
		})
	}
	for _, rateMegabits := range []int64{10, 50, 100, 300, 1000, 2500} {
		name := fmt.Sprintf("rate-%dmbps", rateMegabits)
		profiles[name] = focusedNetworkProfile(name, seed, func(forward *linkProfile, reverse *linkProfile) {
			rateBitsPerSecond := rateMegabits * 1_000_000
			for _, link := range []*linkProfile{forward, reverse} {
				link.RateBitsPerSecond = rateBitsPerSecond
			}
		})
	}
	for _, outerMtu := range []int{1280, 1400, 1500} {
		name := fmt.Sprintf("mtu-%d", outerMtu)
		profiles[name] = focusedNetworkProfile(name, seed, func(forward *linkProfile, reverse *linkProfile) {
			forward.OuterMtu = outerMtu
			reverse.OuterMtu = outerMtu
		})
		profile := profiles[name]
		profile.InnerMtu = min(profile.InnerMtu, outerMtu-80)
		profile.SourceNote = "synthetic focused MTU with an 80-byte tunnel allowance"
		profiles[name] = profile
	}
	profiles["mtu-blackhole-1280"] = focusedNetworkProfile(
		"mtu-blackhole-1280",
		seed,
		func(forward *linkProfile, reverse *linkProfile) {
			forward.OuterMtu = 1280
			reverse.OuterMtu = 1280
			forward.AllowMtuDrops = true
			reverse.AllowMtuDrops = true
		},
	)
	queueDurations := map[string]time.Duration{
		"queue-shallow": 5 * time.Millisecond,
		"queue-one-bdp": 50 * time.Millisecond,
		"queue-deep":    500 * time.Millisecond,
	}
	for name, queueDuration := range queueDurations {
		profiles[name] = focusedNetworkProfile(name, seed, func(forward *linkProfile, reverse *linkProfile) {
			forward.BaseDelay = 25 * time.Millisecond
			reverse.BaseDelay = 25 * time.Millisecond
			forward.QueueByteCount = bandwidthDelayQueue(forward.RateBitsPerSecond, queueDuration)
			reverse.QueueByteCount = bandwidthDelayQueue(reverse.RateBitsPerSecond, queueDuration)
			forward.AllowQueueDrops = true
			reverse.AllowQueueDrops = true
		})
	}
	profiles["direction-asymmetric"] = focusedNetworkProfile(
		"direction-asymmetric",
		seed,
		func(forward *linkProfile, reverse *linkProfile) {
			forward.RateBitsPerSecond = 500_000_000
			reverse.RateBitsPerSecond = 50_000_000
			forward.QueueByteCount = bandwidthDelayQueue(forward.RateBitsPerSecond, 50*time.Millisecond)
			reverse.QueueByteCount = bandwidthDelayQueue(reverse.RateBitsPerSecond, 50*time.Millisecond)
		},
	)
	return profiles
}

// Each jitter variation retains the focused clean control in every other field.
func TestFocusedJitterProfilesChangeOnlyJitter(t *testing.T) {
	const seed = 20260810
	profiles := allNetworkProfiles(seed)
	clean := initialNetworkProfiles(seed)["clean-lan"]
	for _, jitterMilliseconds := range []int{0, 1, 5, 25} {
		name := fmt.Sprintf("jitter-%dms", jitterMilliseconds)
		actual, ok := profiles[name]
		if !ok {
			t.Errorf("missing profile %q", name)
			continue
		}
		want := clean
		want.Name = name
		want.SourceNote = "synthetic focused variation"
		want.Forward.Jitter = time.Duration(jitterMilliseconds) * time.Millisecond
		want.Reverse.Jitter = time.Duration(jitterMilliseconds) * time.Millisecond
		if actual != want {
			t.Errorf("profile %q changed a field outside jitter: actual=%+v want=%+v", name, actual, want)
		}
	}
}

// Each reorder variation retains the focused clean control in every other field.
func TestFocusedReorderProfilesChangeOnlyReorder(t *testing.T) {
	const seed = 20260810
	profiles := allNetworkProfiles(seed)
	clean := initialNetworkProfiles(seed)["clean-lan"]
	for _, reorderBasisPoints := range []int{0, 10, 100, 500} {
		name := fmt.Sprintf("reorder-%dbp", reorderBasisPoints)
		actual, ok := profiles[name]
		if !ok {
			t.Errorf("missing profile %q", name)
			continue
		}
		want := clean
		want.Name = name
		want.SourceNote = "synthetic focused variation"
		want.Forward.ReorderProbability = float64(reorderBasisPoints) / 10_000
		want.Reverse.ReorderProbability = float64(reorderBasisPoints) / 10_000
		if actual != want {
			t.Errorf("profile %q changed a field outside reorder: actual=%+v want=%+v", name, actual, want)
		}
	}
}

// Each rate variation retains the non-limiting clean queue and every other
// control field, so observed loss cannot come from the separate queue axis.
func TestFocusedRateProfilesChangeOnlyRate(t *testing.T) {
	const seed = 20260810
	profiles := allNetworkProfiles(seed)
	clean := initialNetworkProfiles(seed)["clean-lan"]
	for _, rateMegabits := range []int64{10, 50, 100, 300, 1000, 2500} {
		name := fmt.Sprintf("rate-%dmbps", rateMegabits)
		actual, ok := profiles[name]
		if !ok {
			t.Errorf("missing profile %q", name)
			continue
		}
		want := clean
		want.Name = name
		want.SourceNote = "synthetic focused variation"
		want.Forward.RateBitsPerSecond = rateMegabits * 1_000_000
		want.Reverse.RateBitsPerSecond = rateMegabits * 1_000_000
		if actual != want {
			t.Errorf("profile %q changed a field outside rate: actual=%+v want=%+v", name, actual, want)
		}
	}
}

// A no-drop profile cannot advertise a token-bucket burst larger than the
// queue that must own those immediately conforming bytes.
func TestProfileValidationRejectsNoDropQueueBelowAdvertisedBurst(t *testing.T) {
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	profile.Forward.QueueByteCount = profile.Forward.BurstByteCount - 1
	err := profile.validate()
	want := fmt.Sprintf(
		"forward no-drop queue bytes %d cannot own advertised burst bytes %d",
		profile.Forward.QueueByteCount,
		profile.Forward.BurstByteCount,
	)
	if err == nil || err.Error() != want {
		t.Fatalf("no-drop burst validation err=%v want=%q", err, want)
	}
}

// Focused profile identities are deterministic and distinct across axis values.
func TestFocusedJitterAndReorderProfileHashes(t *testing.T) {
	profiles := allNetworkProfiles(20260810)
	profileNames := []string{
		"jitter-0ms",
		"jitter-1ms",
		"jitter-5ms",
		"jitter-25ms",
		"reorder-0bp",
		"reorder-10bp",
		"reorder-100bp",
		"reorder-500bp",
	}
	profileHashes := map[string]bool{}
	for _, profileName := range profileNames {
		profile := profiles[profileName]
		firstHash, err := profile.hash()
		if err != nil {
			t.Fatal(err)
		}
		secondHash, err := profile.hash()
		if err != nil {
			t.Fatal(err)
		}
		if firstHash == "" || firstHash != secondHash {
			t.Fatalf("profile=%s unstable hashes %q %q", profileName, firstHash, secondHash)
		}
		if profileHashes[firstHash] {
			t.Fatalf("profile=%s reused hash %q", profileName, firstHash)
		}
		profileHashes[firstHash] = true
	}
}

// A focused profile changes one axis while retaining the clean profile shape.
func focusedNetworkProfile(
	name string,
	seed int64,
	mutate func(*linkProfile, *linkProfile),
) networkProfile {
	profile := initialNetworkProfiles(seed)["clean-lan"]
	profile.Name = name
	profile.SourceNote = "synthetic focused variation"
	mutate(&profile.Forward, &profile.Reverse)
	return profile
}

// Validation rejects ambiguous filters and impossible simulator settings.
func (self networkProfile) validate() error {
	if self.Name == "" {
		return fmt.Errorf("profile name is empty")
	}
	if self.InnerMtu < 576 {
		return fmt.Errorf("inner MTU %d is below IPv4's supported minimum", self.InnerMtu)
	}
	validateLink := func(direction string, profile linkProfile) error {
		if profile.RateBitsPerSecond <= 0 {
			return fmt.Errorf("%s rate must be positive", direction)
		}
		if profile.BurstByteCount <= 0 || profile.QueueByteCount <= 0 || profile.QueuePacketCount <= 0 {
			return fmt.Errorf("%s queue and burst bounds must be positive", direction)
		}
		if !profile.AllowQueueDrops && profile.QueueByteCount < profile.BurstByteCount {
			return fmt.Errorf(
				"%s no-drop queue bytes %d cannot own advertised burst bytes %d",
				direction,
				profile.QueueByteCount,
				profile.BurstByteCount,
			)
		}
		if profile.BaseDelay < 0 || profile.Jitter < 0 || profile.ProcessingDelay < 0 {
			return fmt.Errorf("%s delay values must not be negative", direction)
		}
		probabilities := []float64{
			profile.LossProbability,
			profile.DuplicateProbability,
			profile.ReorderProbability,
		}
		if profile.BurstLoss != nil {
			probabilities = append(
				probabilities,
				profile.BurstLoss.GoodToBadProbability,
				profile.BurstLoss.BadToGoodProbability,
				profile.BurstLoss.GoodLossProbability,
				profile.BurstLoss.BadLossProbability,
			)
		}
		for _, probability := range probabilities {
			if probability < 0 || 1 < probability {
				return fmt.Errorf("%s probability %.6f is outside [0,1]", direction, probability)
			}
		}
		if profile.OuterMtu < 576 {
			return fmt.Errorf("%s outer MTU %d is below IPv4's supported minimum", direction, profile.OuterMtu)
		}
		switch profile.LossModel {
		case lossModelNone, lossModelIndependent:
		case lossModelEveryN:
			if profile.DropEveryPacketCount == 0 {
				return fmt.Errorf("%s every-N loss has a zero interval", direction)
			}
		case lossModelBurst:
			if profile.BurstLoss == nil {
				return fmt.Errorf("%s burst loss has no state profile", direction)
			}
		default:
			return fmt.Errorf("%s has unknown loss model %q", direction, profile.LossModel)
		}
		switch profile.OversizeMode {
		case oversizeModeDrop, oversizeModeError:
		default:
			return fmt.Errorf("%s has unknown oversized-packet mode %q", direction, profile.OversizeMode)
		}
		return nil
	}
	if err := validateLink("forward", self.Forward); err != nil {
		return err
	}
	return validateLink("reverse", self.Reverse)
}

// The hash identifies the exact resolved settings, including seed and notes.
func (self networkProfile) hash() (string, error) {
	encoded, err := json.Marshal(self)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
