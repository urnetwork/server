package work

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"math"
	"net/netip"
	"os"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The publisher's invariants (connect/EXTENDER-UPDATE2.md §1.4, §2.2, §2.3),
// pinned without a database: a fake fleet stands behind the publisher's seams,
// the records and the bundle are signed with a fixed test root key, and the
// store is the local blob store. What every bundle stored here must satisfy is
// checked by verifying it as a client would.

const testExtenderSeedHost = "ur.example"

var testExtenderSeedEpochStart = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

type testExtenderSeedFleet struct {
	extenders map[server.Id]*model.NetworkExtender
	addresses map[server.Id][]*model.NetworkExtenderAddress
	order     []server.Id
	revoked   map[string]time.Time
}

func newTestExtenderSeedFleet() *testExtenderSeedFleet {
	return &testExtenderSeedFleet{
		extenders: map[server.Id]*model.NetworkExtender{},
		addresses: map[server.Id][]*model.NetworkExtenderAddress{},
		revoked:   map[string]time.Time{},
	}
}

// Adds an active extender with one public address of each family given.
func (self *testExtenderSeedFleet) add(t *testing.T, tier int, canaryChannel string, ipVersions ...int) server.Id {
	t.Helper()
	publicKey := testExtenderSeedPublicKey(1_000_000 + len(self.order))
	extenderId := server.NewId()
	n := len(self.order) + 1
	self.extenders[extenderId] = &model.NetworkExtender{
		ExtenderId:    extenderId,
		PublicKey:     publicKey,
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		DnsTld:        "x.example.",
		CountryCode:   "us",
		Active:        true,
		DirectoryTier: tier,
		CanaryChannel: canaryChannel,
	}
	for _, ipVersion := range ipVersions {
		ip := netip.MustParseAddr(fmt.Sprintf("203.0.%d.%d", 100+n/250, n%250+1))
		if ipVersion == 6 {
			ip = netip.MustParseAddr(fmt.Sprintf("2001:db8:1::%x", n))
		}
		self.addresses[extenderId] = append(self.addresses[extenderId], &model.NetworkExtenderAddress{
			ExtenderId: extenderId,
			IpVersion:  ipVersion,
			Ip:         ip,
			Carriers:   []string{connect.ExtenderCarrierTcp},
			Active:     true,
		})
	}
	self.order = append(self.order, extenderId)
	return extenderId
}

func (self *testExtenderSeedFleet) remove(extenderId server.Id) {
	delete(self.extenders, extenderId)
	delete(self.addresses, extenderId)
}

func (self *testExtenderSeedFleet) revoke(extenderId server.Id, revokeTime time.Time) {
	self.revoked[hex.EncodeToString(self.extenders[extenderId].PublicKey)] = revokeTime
	self.remove(extenderId)
}

func (self *testExtenderSeedFleet) pool(context.Context) []*model.NetworkExtenderDnsAddress {
	addresses := []*model.NetworkExtenderDnsAddress{}
	for _, extenderId := range self.order {
		extender, ok := self.extenders[extenderId]
		if !ok {
			continue
		}
		for _, address := range self.addresses[extenderId] {
			addresses = append(addresses, &model.NetworkExtenderDnsAddress{
				ExtenderId:    extenderId,
				PublicKey:     extender.PublicKey,
				IpVersion:     address.IpVersion,
				Ip:            address.Ip,
				CountryCode:   extender.CountryCode,
				DirectoryTier: extender.DirectoryTier,
				CanaryChannel: extender.CanaryChannel,
			})
		}
	}
	return addresses
}

func (self *testExtenderSeedFleet) recordOf(_ context.Context, extenderId server.Id) (*model.NetworkExtender, []*model.NetworkExtenderAddress) {
	extender, ok := self.extenders[extenderId]
	if !ok || len(self.addresses[extenderId]) == 0 {
		return nil, nil
	}
	copied := *extender
	return &copied, self.addresses[extenderId]
}

func (self *testExtenderSeedFleet) revokedOf(_ context.Context, publicKeys [][]byte) []*model.NetworkExtenderSeedRevocation {
	revocations := []*model.NetworkExtenderSeedRevocation{}
	for _, publicKey := range publicKeys {
		if revokeTime, ok := self.revoked[hex.EncodeToString(publicKey)]; ok {
			revocations = append(revocations, &model.NetworkExtenderSeedRevocation{PublicKey: publicKey, RevokeTime: revokeTime})
		}
	}
	return revocations
}

type testExtenderSeedOperator struct {
	config         *controller.ExtenderConfig
	rootPrivateKey ed25519.PrivateKey
	rootKeys       *connect.ExtenderRootKeySet
	store          server.BlobStore
}

func newTestExtenderSeedOperator(t *testing.T) *testExtenderSeedOperator {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	return newTestExtenderSeedOperatorWithKey(t, seed, server.NewLocalBlobStore(t.TempDir(), "blob"))
}

func newTestExtenderSeedOperatorWithKey(t *testing.T, seed []byte, store server.BlobStore, extraPublicKeys ...ed25519.PublicKey) *testExtenderSeedOperator {
	t.Helper()
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	publicKeys := []ed25519.PublicKey{rootPrivateKey.Public().(ed25519.PublicKey)}
	publicKeys = append(publicKeys, extraPublicKeys...)
	publicKeyHexes := []string{}
	for _, publicKey := range publicKeys {
		publicKeyHexes = append(publicKeyHexes, hex.EncodeToString(publicKey))
	}
	return &testExtenderSeedOperator{
		config: &controller.ExtenderConfig{
			RootPrivateKeyHex: connect.ExtenderKeySeedHex(seed),
			RootPublicKeysHex: publicKeyHexes,
			NetworkHost:       testExtenderSeedHost,
			NetworkHosts:      []string{testExtenderSeedHost},
			Seed:              controller.ExtenderSeedConfig{Enabled: true},
		},
		rootPrivateKey: rootPrivateKey,
		rootKeys:       connect.NewExtenderRootKeySet(publicKeys...),
		store:          store,
	}
}

func (self *testExtenderSeedOperator) publisher(t *testing.T, fleet *testExtenderSeedFleet) *extenderSeedPublisher {
	t.Helper()
	publisher, err := newExtenderSeedPublisher(self.config, self.store)
	if err != nil {
		t.Fatal(err)
	}
	publisher.pool = fleet.pool
	publisher.recordOf = fleet.recordOf
	publisher.revoked = fleet.revokedOf
	return publisher
}

// The stored bundle, verified as a client verifies it at now.
func (self *testExtenderSeedOperator) stored(t *testing.T, now time.Time) (*connect.ExtenderSeedBundle, []byte) {
	t.Helper()
	artifact, err := controller.ReadExtenderSeedLatest(context.Background(), self.store, testExtenderSeedHost)
	if err != nil {
		t.Fatal(err)
	}
	if artifact == nil {
		return nil, nil
	}
	envelope, err := connect.ParseExtenderSeedEnvelope(artifact)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := connect.VerifyExtenderSeed(envelope, self.rootKeys, []string{testExtenderSeedHost}, now)
	if err != nil {
		t.Fatalf("the stored bundle does not verify: %s", err)
	}
	return bundle, artifact
}

// Every property a stored bundle must have, whatever the source data held.
func assertExtenderSeedBundleInvariants(t *testing.T, bundle *connect.ExtenderSeedBundle, artifact []byte, now time.Time) {
	t.Helper()
	if connect.ExtenderSeedMaxEnvelopeByteCount < len(artifact) {
		t.Fatalf("the artifact is %d bytes", len(artifact))
	}
	if connect.ExtenderSeedMaxRecordCount < len(bundle.Records) || connect.ExtenderSeedMaxRevocationCount < len(bundle.Revocations) {
		t.Fatalf("%d records and %d revocations", len(bundle.Records), len(bundle.Revocations))
	}
	addresses := 0
	for _, record := range bundle.Records {
		if !connect.ExtenderRecordOpen(record.Body) {
			t.Fatalf("the bundle carries a gated or channel-restricted record")
		}
		if len(record.Body.RequiredCapabilities) != 0 {
			t.Fatalf("the bundle carries a record requiring a capability")
		}
		if uint64(now.UnixMilli()) >= record.Body.ExpireTimeMs {
			t.Fatalf("the bundle carries an expired record")
		}
		if record.Body.ExpireTimeMs < uint64(bundle.ExpireTime.UnixMilli()) {
			t.Fatalf("a record expires before the bundle")
		}
		addresses += len(record.Body.Addresses)
	}
	if connect.ExtenderSeedMaxAddressCount < addresses {
		t.Fatalf("the records list %d addresses", addresses)
	}
	if !bundle.ExpireTime.After(now) {
		t.Fatalf("the bundle was stored expired")
	}
}

// A gated, a channel-restricted, or a tier-moved extender never enters a
// bundle -- not from the pool, not from a stale previous cohort, and not when
// whatever signed its record handed back a gated record (§1.4: "Gated and
// channel-restricted records never enter any public publication path,
// including the new seed publisher").
func TestExtenderSeedPublisherNeverPublishesGatedOrChannelRestrictedRecords(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	excluded := map[string]bool{}
	for range 10 {
		for _, extenderId := range []server.Id{
			fleet.add(t, connect.ExtenderDirectoryTierGated, "", 4, 6),
			fleet.add(t, connect.ExtenderDirectoryTierGated, connect.ExtenderChannelGated, 4),
			fleet.add(t, connect.ExtenderDirectoryTierOpen, connect.ExtenderChannelDns, 4, 6),
		} {
			excluded[hex.EncodeToString(fleet.extenders[extenderId].PublicKey)] = true
		}
	}
	open := []server.Id{}
	for range 12 {
		open = append(open, fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6))
	}
	// one open member moves to the gated tier between the pool read and its
	// record read, and another's signer answers a gated record
	movedId, forgedId := open[0], open[1]
	excluded[hex.EncodeToString(fleet.extenders[movedId].PublicKey)] = true
	excluded[hex.EncodeToString(fleet.extenders[forgedId].PublicKey)] = true

	publisher := operator.publisher(t, fleet)
	recordOf := publisher.recordOf
	publisher.recordOf = func(ctx context.Context, extenderId server.Id) (*model.NetworkExtender, []*model.NetworkExtenderAddress) {
		extender, addresses := recordOf(ctx, extenderId)
		if extenderId == movedId && extender != nil {
			extender.DirectoryTier = connect.ExtenderDirectoryTierGated
		}
		return extender, addresses
	}
	signRecord := publisher.signRecord
	publisher.signRecord = func(extender *model.NetworkExtender, addresses []*model.NetworkExtenderAddress, issueTime time.Time, lifetime time.Duration) (*protocol.ExtenderRecord, error) {
		if extender.ExtenderId == forgedId {
			gated := *extender
			gated.CanaryChannel = connect.ExtenderChannelGated
			record, _, err := controller.SignExtenderRecord(operator.config, operator.rootPrivateKey, &gated, addresses, issueTime)
			return record, err
		}
		return signRecord(extender, addresses, issueTime, lifetime)
	}

	now := testExtenderSeedEpochStart.Add(time.Hour)
	for pass := range 3 {
		result := publisher.publish(context.Background(), now)
		if pass == 0 && result.outcome != extenderSeedOutcomePublished {
			t.Fatalf("pass %d: %s", pass, result.outcome)
		}
		bundle, artifact := operator.stored(t, now)
		assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
		if len(bundle.Records) == 0 {
			t.Fatalf("pass %d published no record", pass)
		}
		for _, record := range bundle.Records {
			if excluded[hex.EncodeToString(record.Body.PublicKey)] {
				t.Fatalf("pass %d published an excluded extender", pass)
			}
		}
		now = now.Add(controller.ExtenderSeedRefreshTimeout)
	}
}

// A record that requires a capability never enters a bundle, whatever signed it:
// a seed is for every client, and one too old to read the requirement would dial
// the record anyway and fail.
func TestExtenderSeedPublisherNeverPublishesARecordRequiringACapability(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	for range 12 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)
	signRecord := publisher.signRecord
	requiring := map[string]bool{}
	signed := 0
	publisher.signRecord = func(extender *model.NetworkExtender, addresses []*model.NetworkExtenderAddress, issueTime time.Time, lifetime time.Duration) (*protocol.ExtenderRecord, error) {
		record, err := signRecord(extender, addresses, issueTime, lifetime)
		signed += 1
		if err != nil || signed%2 == 0 {
			return record, err
		}
		requiring[hex.EncodeToString(extender.PublicKey)] = true
		body := &protocol.ExtenderRecordBody{}
		if err := proto.Unmarshal(record.Body, body); err != nil {
			return nil, err
		}
		body.RequiredCapabilities = []string{connect.ExtenderCapabilityTcpCamouflageV1}
		return connect.SignExtenderRecord(operator.rootPrivateKey, body)
	}

	now := testExtenderSeedEpochStart.Add(time.Hour)
	if result := publisher.publish(context.Background(), now); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("first pass: %s", result.outcome)
	}
	bundle, artifact := operator.stored(t, now)
	assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
	if len(bundle.Records) == 0 || len(requiring) == 0 {
		t.Fatalf("%d records published, %d passed over", len(bundle.Records), len(requiring))
	}
	for _, record := range bundle.Records {
		if requiring[hex.EncodeToString(record.Body.PublicKey)] || len(record.Body.RequiredCapabilities) != 0 {
			t.Fatalf("a record requiring a capability was published")
		}
	}
}

// However large the fleet, however many addresses one record lists and however
// many members were revoked, a bundle stays within every envelope limit.
func TestExtenderSeedPublisherStaysWithinEnvelopeLimits(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	for range 400 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)

	// every record lists twelve addresses, so the address cap binds first
	signRecord := publisher.signRecord
	n := 0
	publisher.signRecord = func(extender *model.NetworkExtender, addresses []*model.NetworkExtenderAddress, issueTime time.Time, lifetime time.Duration) (*protocol.ExtenderRecord, error) {
		record, err := signRecord(extender, addresses, issueTime, lifetime)
		if err != nil {
			return nil, err
		}
		body := &protocol.ExtenderRecordBody{}
		if err := proto.Unmarshal(record.Body, body); err != nil {
			return nil, err
		}
		n += 1
		for i := range 10 {
			body.Addresses = append(body.Addresses, &protocol.ExtenderAddress{
				Ip:        fmt.Sprintf("2001:db8:%x::%x", n, i+1),
				IpVersion: 6,
				Carriers:  []string{connect.ExtenderCarrierTcp},
			})
		}
		return connect.SignExtenderRecord(operator.rootPrivateKey, body)
	}

	now := testExtenderSeedEpochStart.Add(time.Hour)
	if result := publisher.publish(context.Background(), now); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("first pass: %s", result.outcome)
	}
	bundle, artifact := operator.stored(t, now)
	assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
	if len(bundle.Records) != 2 {
		t.Fatalf("%d records of twelve addresses each, want the 2 that fit in 32", len(bundle.Records))
	}

	// back to ordinary records; then revoke every member of a long run of
	// bundles, so far more revocations are carried than one bundle holds
	publisher.signRecord = signRecord
	for range 12 {
		now = now.Add(time.Hour)
		current, _ := operator.stored(t, now)
		for _, record := range current.Records {
			for extenderId, extender := range fleet.extenders {
				if string(extender.PublicKey) == string(record.Body.PublicKey) {
					fleet.revoke(extenderId, now.Add(-time.Minute))
				}
			}
		}
		if result := publisher.publish(context.Background(), now); result.outcome != extenderSeedOutcomePublished {
			t.Fatalf("revocation pass: %s", result.outcome)
		}
		bundle, artifact = operator.stored(t, now)
		assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
		if len(bundle.Records) == 0 {
			t.Fatalf("a revocation pass published no record")
		}
	}
	if len(fleet.revoked) <= connect.ExtenderSeedMaxRevocationCount {
		t.Fatalf("only %d members were revoked; the test needs more than a bundle holds", len(fleet.revoked))
	}
	if len(bundle.Revocations) != connect.ExtenderSeedMaxRevocationCount {
		t.Fatalf("%d revocations, want the limit of %d after %d were revoked", len(bundle.Revocations), connect.ExtenderSeedMaxRevocationCount, len(fleet.revoked))
	}
	// a full cohort of ordinary records with the most revocations is raw
	// binary well under the 64 KiB + 1 bytes a client reads from a mirror
	if len(bundle.Records) != ExtenderSeedMaxRecordCount || 16*1024 < len(artifact) {
		t.Fatalf("a bundle of %d records and %d revocations is %d bytes", len(bundle.Records), len(bundle.Revocations), len(artifact))
	}
}

// No record that is expired, expires before the bundle, or is issued after it
// is ever published, whatever the signer answered; the bundle's validity never
// outlives its records.
func TestExtenderSeedPublisherNeverPublishesAnExpiredRecord(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	for range 30 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)
	signRecord := publisher.signRecord
	bad := map[string]bool{}
	signed := 0
	publisher.signRecord = func(extender *model.NetworkExtender, addresses []*model.NetworkExtenderAddress, issueTime time.Time, lifetime time.Duration) (*protocol.ExtenderRecord, error) {
		signed += 1
		var shift func(body *protocol.ExtenderRecordBody)
		switch signed % 4 {
		case 1:
			// expired a minute ago
			shift = func(body *protocol.ExtenderRecordBody) {
				body.IssueTimeMs = uint64(issueTime.Add(-25 * time.Hour).UnixMilli())
				body.ExpireTimeMs = uint64(issueTime.Add(-time.Hour).UnixMilli())
			}
		case 2:
			// valid now, but expires before the bundle would
			shift = func(body *protocol.ExtenderRecordBody) {
				body.ExpireTimeMs = uint64(issueTime.Add(lifetime - time.Minute).UnixMilli())
			}
		case 3:
			// issued after the bundle
			shift = func(body *protocol.ExtenderRecordBody) {
				body.IssueTimeMs = uint64(issueTime.Add(time.Minute).UnixMilli())
			}
		}
		record, err := signRecord(extender, addresses, issueTime, lifetime)
		if err != nil || shift == nil {
			return record, err
		}
		bad[hex.EncodeToString(extender.PublicKey)] = true
		body := &protocol.ExtenderRecordBody{}
		if err := proto.Unmarshal(record.Body, body); err != nil {
			return nil, err
		}
		shift(body)
		return connect.SignExtenderRecord(operator.rootPrivateKey, body)
	}

	now := testExtenderSeedEpochStart.Add(time.Hour)
	if result := publisher.publish(context.Background(), now); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("first pass: %s", result.outcome)
	}
	bundle, artifact := operator.stored(t, now)
	assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
	if len(bundle.Records) == 0 {
		t.Fatalf("no record was published")
	}
	for _, record := range bundle.Records {
		if bad[hex.EncodeToString(record.Body.PublicKey)] {
			t.Fatalf("a record the bundle cannot promise was published")
		}
	}
	if got, want := bundle.ExpireTime.Sub(bundle.IssueTime), controller.ExtenderRecordExpireTimeout; got != want {
		t.Fatalf("the bundle's validity is %s, want the record lifetime %s", got, want)
	}

	// a configured lifetime signs the records for it, and the bundle matches
	operator.config.Seed.RecordLifetimeHours = 72
	publisher.signRecord = signRecord
	now = now.Add(time.Hour)
	result := publisher.publish(context.Background(), now)
	if result.outcome != extenderSeedOutcomePublished || result.reason != "lifetime" {
		t.Fatalf("lifetime change: %s %s", result.outcome, result.reason)
	}
	bundle, artifact = operator.stored(t, now)
	assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
	if got := bundle.ExpireTime.Sub(bundle.IssueTime); got != 72*time.Hour {
		t.Fatalf("validity %s, want 72h", got)
	}
	for _, record := range bundle.Records {
		if time.UnixMilli(int64(record.Body.ExpireTimeMs)).Sub(time.UnixMilli(int64(record.Body.IssueTimeMs))) != 72*time.Hour {
			t.Fatalf("a record was not signed with the configured lifetime")
		}
	}
}

func TestNextExtenderSeedSequence(t *testing.T) {
	at := time.UnixMilli(1_800_000_000_000)
	for _, c := range []struct {
		floor uint64
		want  uint64
		ok    bool
	}{
		{0, 1_800_000_000_000, true},
		{1_799_999_999_999, 1_800_000_000_000, true},
		// the floor at or past the clock: one past the floor
		{1_800_000_000_000, 1_800_000_000_001, true},
		{1_900_000_000_000, 1_900_000_000_001, true},
		{math.MaxUint64 - 1, math.MaxUint64, true},
		{math.MaxUint64, 0, false},
	} {
		got, ok := nextExtenderSeedSequence(at, c.floor)
		if got != c.want || ok != c.ok {
			t.Errorf("floor %d: %d %t, want %d %t", c.floor, got, ok, c.want, c.ok)
		}
	}
	if got, ok := nextExtenderSeedSequence(time.Unix(-10, 0), 0); got != 1 || !ok {
		t.Errorf("a clock before 1970: %d %t, want 1", got, ok)
	}
}

// The sequence never regresses: not when the clock goes back, not across a
// signing-key rotation that leaves the stored bundle unverifiable, and not
// below a sequence another publish claimed but never stored as latest.
func TestExtenderSeedSequenceNeverRegresses(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	store := server.NewLocalBlobStore(t.TempDir(), "blob")
	operator := newTestExtenderSeedOperatorWithKey(t, seed, store)
	fleet := newTestExtenderSeedFleet()
	for range 20 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)

	ctx := context.Background()
	now := testExtenderSeedEpochStart.Add(time.Hour)
	sequences := []uint64{}
	record := func(result *extenderSeedPublishResult) {
		t.Helper()
		if result.outcome != extenderSeedOutcomePublished {
			t.Fatalf("pass at %s: %s", now, result.outcome)
		}
		if n := len(sequences); 0 < n && result.stored.Sequence <= sequences[n-1] {
			t.Fatalf("sequence %d after %d", result.stored.Sequence, sequences[n-1])
		}
		sequences = append(sequences, result.stored.Sequence)
	}
	record(publisher.publish(ctx, now))

	// the clock steps back two hours, and a member leaves so a bundle is due:
	// a member of the stored cohort, which the root key's ranking picks, not
	// whichever extender was added first
	first, _ := operator.stored(t, now)
	for extenderId, extender := range fleet.extenders {
		if string(extender.PublicKey) == string(first.Records[0].Body.PublicKey) {
			fleet.remove(extenderId)
		}
	}
	record(publisher.publish(ctx, now.Add(-2*time.Hour)))

	// another publish claimed a far higher sequence and never stored it
	claimed := sequences[len(sequences)-1] + 1_000_000
	claimArtifact, err := controller.ReadExtenderSeedLatest(ctx, store, testExtenderSeedHost)
	if err != nil {
		t.Fatal(err)
	}
	stagePath := t.TempDir() + "/claim.bin"
	if err := os.WriteFile(stagePath, claimArtifact, 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := store.PutIfAbsent(ctx, controller.ExtenderSeedSequenceKey(store, testExtenderSeedHost, claimed), stagePath, controller.ExtenderSeedContentType); err != nil || !created {
		t.Fatalf("claim: %t %v", created, err)
	}
	now = now.Add(controller.ExtenderSeedRefreshTimeout)
	record(publisher.publish(ctx, now))
	if sequences[len(sequences)-1] <= claimed {
		t.Fatalf("sequence %d is not above the claimed %d", sequences[len(sequences)-1], claimed)
	}

	// the root key rotates: the new key signs, and the stored bundle, signed
	// by the old one, no longer verifies, yet still holds the floor
	rotatedSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rotated := newTestExtenderSeedOperatorWithKey(t, rotatedSeed, store)
	rotatedPublisher := rotated.publisher(t, fleet)
	result := rotatedPublisher.publish(ctx, now.Add(-time.Hour))
	if result.reason != "initial" {
		t.Fatalf("an unverifiable stored bundle is not replaced as initial: %s", result.reason)
	}
	record(result)

	// a publish that loses the claim race -- another writer claims the same
	// sequence between this pass's floor read and its claim -- stores nothing
	racing := &testRacingSeedStore{BlobStore: store}
	racing.race = func(key string) {
		if _, err := store.PutIfAbsent(ctx, key, stagePath, controller.ExtenderSeedContentType); err != nil {
			t.Fatal(err)
		}
	}
	rotated.store = racing
	racePublisher := rotated.publisher(t, fleet)
	before, _ := rotated.stored(t, now)
	now = now.Add(controller.ExtenderSeedRefreshTimeout)
	if result := racePublisher.publish(ctx, now); result.outcome != extenderSeedOutcomeClaimed {
		t.Fatalf("a publish at a claimed sequence: %s", result.outcome)
	}
	after, _ := rotated.stored(t, now)
	if after.Sequence != before.Sequence {
		t.Fatalf("a lost claim changed the stored bundle")
	}
	raced, err := controller.ExtenderSeedClaimedSequenceFloor(ctx, store, testExtenderSeedHost)
	if err != nil {
		t.Fatal(err)
	}
	// and the next pass goes above the claim
	record(racePublisher.publish(ctx, now))
	if sequences[len(sequences)-1] <= raced {
		t.Fatalf("sequence %d is not above the raced claim %d", sequences[len(sequences)-1], raced)
	}
}

// A latest.bin behind a claimed sequence is replaced on the next pass, not at the
// next refresh: a mirror or a client may hold the higher sequence and refuse the
// stored bundle. That covers a publish that died between its claim and latest.bin,
// and a latest.bin restored from an older copy. Once above the claim, nothing more
// is due.
func TestExtenderSeedPublisherGoesAboveAClaimLatestDoesNotHold(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	for range 12 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)
	ctx := context.Background()
	now := testExtenderSeedEpochStart.Add(time.Hour)
	if result := publisher.publish(ctx, now); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("initial: %s", result.outcome)
	}
	first, firstArtifact := operator.stored(t, now)
	stagePath := t.TempDir() + "/first.bin"
	if err := os.WriteFile(stagePath, firstArtifact, 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if result := publisher.publish(ctx, now); result.outcome != extenderSeedOutcomeCurrent {
		t.Fatalf("nothing due: %s (%s)", result.outcome, result.reason)
	}

	// a publish that claimed a higher sequence and died before latest.bin
	claimed := first.Sequence + 1_000_000
	if created, err := operator.store.PutIfAbsent(ctx, controller.ExtenderSeedSequenceKey(operator.store, testExtenderSeedHost, claimed), stagePath, controller.ExtenderSeedContentType); err != nil || !created {
		t.Fatalf("claim: %t %v", created, err)
	}
	now = now.Add(10 * time.Minute)
	result := publisher.publish(ctx, now)
	if result.outcome != extenderSeedOutcomePublished || result.reason != "behind_claim" {
		t.Fatalf("behind a claim: %s (%s)", result.outcome, result.reason)
	}
	above := result.stored.Sequence
	if above <= claimed {
		t.Fatalf("sequence %d is not above the claimed %d", above, claimed)
	}
	now = now.Add(10 * time.Minute)
	if result := publisher.publish(ctx, now); result.outcome != extenderSeedOutcomeCurrent {
		t.Fatalf("above the claim: %s (%s)", result.outcome, result.reason)
	}

	// latest.bin restored from the older copy: the claims outlive it
	if err := operator.store.Put(ctx, controller.ExtenderSeedLatestKey(operator.store, testExtenderSeedHost), stagePath, controller.ExtenderSeedContentType); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	result = publisher.publish(ctx, now)
	if result.outcome != extenderSeedOutcomePublished || result.reason != "behind_claim" || result.stored.Sequence <= above {
		t.Fatalf("restored from an older copy: %s (%s), sequence after %d", result.outcome, result.reason, above)
	}
}

// A store whose first claim is beaten by another writer.
type testRacingSeedStore struct {
	server.BlobStore
	race func(key string)
}

func (self *testRacingSeedStore) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	if race := self.race; race != nil {
		self.race = nil
		race(key)
	}
	return self.BlobStore.PutIfAbsent(ctx, key, localPath, contentType)
}

// A bundle is replaced only when one is due: it stands within the refresh
// period, and is replaced on the refresh, when a member leaves, when a member
// is revoked (and the revocation is carried), and when the epoch turns.
func TestExtenderSeedPublisherReplacesTheBundleOnlyWhenDue(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	for range 60 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)
	ctx := context.Background()
	now := testExtenderSeedEpochStart.Add(time.Hour)
	expect := func(outcome string, reason string) *connect.ExtenderSeedBundle {
		t.Helper()
		result := publisher.publish(ctx, now)
		if result.outcome != outcome || result.reason != reason {
			t.Fatalf("at %s: %s (%s), want %s (%s)", now.Sub(testExtenderSeedEpochStart), result.outcome, result.reason, outcome, reason)
		}
		bundle, artifact := operator.stored(t, now)
		assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
		return bundle
	}

	first := expect(extenderSeedOutcomePublished, "initial")
	now = now.Add(10 * time.Minute)
	if same := expect(extenderSeedOutcomeCurrent, ""); same.Sequence != first.Sequence {
		t.Fatalf("a pass with nothing due stored a new bundle")
	}
	now = first.IssueTime.Add(controller.ExtenderSeedRefreshTimeout)
	refreshed := expect(extenderSeedOutcomePublished, "refresh")
	if string(refreshed.BodyDigest) == string(first.BodyDigest) {
		t.Fatalf("the refresh stored the same body")
	}
	if !sameExtenderSeedKeys(first, refreshed) {
		t.Fatalf("a refresh within the epoch changed the cohort")
	}

	// a member leaves: replaced alone
	now = now.Add(10 * time.Minute)
	var goneKey []byte
	for extenderId, extender := range fleet.extenders {
		if string(extender.PublicKey) == string(refreshed.Records[2].Body.PublicKey) {
			goneKey = extender.PublicKey
			fleet.remove(extenderId)
		}
	}
	gone := expect(extenderSeedOutcomePublished, "member_gone")
	for _, record := range gone.Records {
		if string(record.Body.PublicKey) == string(goneKey) {
			t.Fatalf("the member that left is still published")
		}
	}

	// a member is revoked: its revocation is carried
	now = now.Add(10 * time.Minute)
	var revokedKey []byte
	for extenderId, extender := range fleet.extenders {
		if string(extender.PublicKey) == string(gone.Records[0].Body.PublicKey) {
			revokedKey = extender.PublicKey
			fleet.revoke(extenderId, now.Add(-time.Minute))
		}
	}
	revoked := expect(extenderSeedOutcomePublished, "revoked")
	carried := false
	for _, revocation := range revoked.Revocations {
		carried = carried || string(revocation.Body.PublicKey) == string(revokedKey)
	}
	if !carried {
		t.Fatalf("the revoked member's revocation is not in the bundle")
	}
	now = now.Add(10 * time.Minute)
	expect(extenderSeedOutcomeCurrent, "")

	// the planned update: the epoch turns
	now = testExtenderSeedEpochStart.Add(ExtenderSeedCohortEpochTimeout).Add(time.Minute)
	if connect.ExtenderEpoch(now, ExtenderSeedCohortEpochTimeout) == connect.ExtenderEpoch(revoked.IssueTime, ExtenderSeedCohortEpochTimeout) {
		t.Fatalf("the test clock did not reach the next epoch")
	}
	expect(extenderSeedOutcomePublished, "epoch")
}

// A revocation is carried bundle to bundle while any record it dominates may
// still be valid anywhere: with a seed lifetime shorter than a day, that is
// the day a record of every other channel lives, and once both have passed it
// is dropped.
func TestExtenderSeedPublisherCarriesARevocationWhileAnyRecordItDominatesLives(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	// the shortest lifetime the publisher signs
	operator.config.Seed.RecordLifetimeHours = 1
	lifetime := operator.config.Seed.RecordLifetime()
	if controller.ExtenderRecordExpireTimeout <= lifetime {
		t.Fatalf("the test needs a seed lifetime under a day, got %s", lifetime)
	}
	fleet := newTestExtenderSeedFleet()
	for range 20 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)
	ctx := context.Background()
	now := testExtenderSeedEpochStart.Add(time.Hour)
	if result := publisher.publish(ctx, now); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("initial: %s", result.outcome)
	}
	first, _ := operator.stored(t, now)
	revokedKey := first.Records[0].Body.PublicKey
	revokeTime := now.Add(9 * time.Minute)
	for extenderId, extender := range fleet.extenders {
		if string(extender.PublicKey) == string(revokedKey) {
			fleet.revoke(extenderId, revokeTime)
		}
	}
	now = now.Add(10 * time.Minute)
	if result := publisher.publish(ctx, now); result.outcome != extenderSeedOutcomePublished || result.reason != "revoked" {
		t.Fatalf("revoked: %s %s", result.outcome, result.reason)
	}
	carries := func() bool {
		bundle, artifact := operator.stored(t, now)
		assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
		for _, revocation := range bundle.Revocations {
			if string(revocation.Body.PublicKey) == string(revokedKey) {
				return true
			}
		}
		return false
	}
	if !carries() {
		t.Fatalf("the revoked member's revocation is not in the bundle")
	}

	// every refresh through the day keeps it, though the seed lifetime is
	// long past: a day's record of the drip or the dns sets may still be held
	for now.Before(revokeTime.Add(controller.ExtenderRecordExpireTimeout)) {
		now = now.Add(controller.ExtenderSeedRefreshTimeout)
		if result := publisher.publish(ctx, now); result.outcome != extenderSeedOutcomePublished {
			t.Fatalf("refresh at %s: %s", now.Sub(revokeTime), result.outcome)
		}
		if now.Before(revokeTime.Add(controller.ExtenderRecordExpireTimeout)) && !carries() {
			t.Fatalf("the revocation was dropped %s after it was issued, within a record's day", now.Sub(revokeTime))
		}
	}
	// past the day and the clock allowance, it is dropped
	now = revokeTime.Add(controller.ExtenderRecordExpireTimeout + connect.ExtenderSeedClockSkew + controller.ExtenderSeedRefreshTimeout)
	if result := publisher.publish(ctx, now); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("after the day: %s", result.outcome)
	}
	if carries() {
		t.Fatalf("the revocation is still carried after every record it dominates expired")
	}
}

// A cohort short of the client's low-water mark grows within the hour once
// the open pool can fill it, not only at the six-hour refresh, and a pool
// that cannot fill it does not re-publish on every pass.
func TestExtenderSeedPublisherGrowsACohortShortOfTheLowWaterMark(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	// a configured count below the mark is raised to it
	operator.config.Seed.RecordCount = 2
	fleet := newTestExtenderSeedFleet()
	for range 2 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	// open, but with no public address: never publishable
	unpublishable := fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4)
	for _, address := range fleet.addresses[unpublishable] {
		address.Ip = netip.MustParseAddr("10.0.0.7")
	}
	publisher := operator.publisher(t, fleet)
	ctx := context.Background()
	now := testExtenderSeedEpochStart.Add(time.Hour)
	expect := func(outcome string, reason string, records int) {
		t.Helper()
		result := publisher.publish(ctx, now)
		if result.outcome != outcome || result.reason != reason {
			t.Fatalf("at %s: %s (%s), want %s (%s)", now.Sub(testExtenderSeedEpochStart), result.outcome, result.reason, outcome, reason)
		}
		bundle, artifact := operator.stored(t, now)
		assertExtenderSeedBundleInvariants(t, bundle, artifact, now)
		if len(bundle.Records) != records {
			t.Fatalf("at %s: %d records, want %d", now.Sub(testExtenderSeedEpochStart), len(bundle.Records), records)
		}
	}
	expect(extenderSeedOutcomePublished, "initial", 2)
	// the unpublishable candidate keeps the pool larger than the cohort, yet
	// asks for a new bundle at most once an hour
	for range 5 {
		now = now.Add(10 * time.Minute)
		expect(extenderSeedOutcomeCurrent, "", 2)
	}
	now = now.Add(10 * time.Minute)
	expect(extenderSeedOutcomePublished, "short", 2)
	now = now.Add(10 * time.Minute)
	expect(extenderSeedOutcomeCurrent, "", 2)

	// the pool grows: the next hourly draw fills the cohort to the mark and
	// keeps the members it had
	for range 6 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	before, _ := operator.stored(t, now)
	now = before.IssueTime.Add(extenderSeedShortRetryTimeout)
	expect(extenderSeedOutcomePublished, "short", 4)
	after, _ := operator.stored(t, now)
	for _, keyHex := range extenderSeedBundleKeyHexes(before) {
		if !slices.Contains(extenderSeedBundleKeyHexes(after), keyHex) {
			t.Fatalf("growing the cohort dropped a member")
		}
	}
	// at the mark, nothing more is due until the refresh
	now = now.Add(extenderSeedShortRetryTimeout)
	expect(extenderSeedOutcomeCurrent, "", 4)
}

// An empty open pool stores nothing: the stored bundle stays and ages out.
func TestExtenderSeedPublisherKeepsTheBundleWhenNothingIsOpen(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	for range 4 {
		fleet.add(t, connect.ExtenderDirectoryTierGated, "", 4, 6)
	}
	publisher := operator.publisher(t, fleet)
	now := testExtenderSeedEpochStart.Add(time.Hour)
	if result := publisher.publish(context.Background(), now); result.outcome != extenderSeedOutcomeEmpty {
		t.Fatalf("an all-gated fleet: %s", result.outcome)
	}
	if bundle, _ := operator.stored(t, now); bundle != nil {
		t.Fatalf("an all-gated fleet stored a bundle")
	}
}

// A signing key the published key set does not list is refused before
// anything is stored: a bundle clients would refuse is never served.
func TestExtenderSeedPublisherRefusesAKeyClientsDoNotAccept(t *testing.T) {
	operator := newTestExtenderSeedOperator(t)
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	operator.config.RootPublicKeysHex = []string{hex.EncodeToString(other)}
	fleet := newTestExtenderSeedFleet()
	for range 8 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4)
	}
	publisher := operator.publisher(t, fleet)
	now := testExtenderSeedEpochStart.Add(time.Hour)
	if result := publisher.publish(context.Background(), now); result.outcome != extenderSeedOutcomeSelfCheck {
		t.Fatalf("an unaccepted signing key: %s", result.outcome)
	}
	artifact, err := controller.ReadExtenderSeedLatest(context.Background(), operator.store, testExtenderSeedHost)
	if err != nil || artifact != nil {
		t.Fatalf("an unaccepted signing key stored a bundle")
	}
	if floor, err := controller.ExtenderSeedClaimedSequenceFloor(context.Background(), operator.store, testExtenderSeedHost); err != nil || floor != 0 {
		t.Fatalf("an unaccepted signing key claimed a sequence")
	}
}

func sameExtenderSeedKeys(a *connect.ExtenderSeedBundle, b *connect.ExtenderSeedBundle) bool {
	keys := func(bundle *connect.ExtenderSeedBundle) []string {
		return extenderSeedBundleKeyHexes(bundle)
	}
	ak, bk := keys(a), keys(b)
	if len(ak) != len(bk) {
		return false
	}
	for i := range ak {
		if ak[i] != bk[i] {
			return false
		}
	}
	return true
}
