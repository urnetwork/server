package work

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"math"
	"net/netip"
	"slices"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The public seed publisher (connect/EXTENDER-UPDATE2.md §2.2, §2.3).
//
// One pass reads the stored bundle back from the blob store, decides whether a
// new one is due, and if so signs the cohort's records fresh, signs the bundle
// with the root key `extender.yml` already holds, checks it as a reader would,
// and stores it (controller/extender_seed_store.go). Mirrors serve that stored
// artifact; they never see a key.
//
// A new bundle is due when none is stored or the stored one does not verify,
// every ExtenderSeedRefreshTimeout, when the cohort epoch turns (the planned
// update), when the configured lifetime changed, when a member left the open
// pool, and when a member was revoked. Otherwise the pass stores nothing, so the
// sequence advances only when the bundle changes or ages.
//
// Validity. A bundle never outlives the records it promises: its expiry is the
// expiry of its records, which are signed at the bundle's issue time with the
// configured lifetime (ExtenderSeedConfig.RecordLifetime, a day by default). A
// record that would expire before the bundle, or is issued after it, is never
// published. Refreshing every six hours keeps three quarters of a day's
// validity on every healthy mirror.
//
// Sequence. The sequence is the issue time in unix milliseconds, raised past
// the stored bundle's sequence and every claimed sequence when either is at or
// above it. It needs no table: the stored bundle and the claims are the floor,
// they are read back on every pass, a claim is atomic, and the floor holds
// across a signing-key rotation because an unverifiable stored bundle still
// counts as a floor. The task's run-once key keeps one pass at a time; the
// claim keeps two overlapping passes from storing two bodies at one sequence.
//
// What may be published. Only open-tier records with no canary channel, from
// the dns default partitions (extender_seed_cohort.go), each checked again
// after signing, with only public addresses no other record of the bundle
// lists, within the envelope's limits. Revocations are the root-signed
// revocations of members the operator revoked, carried while a record they
// dominate could still be valid.

// The pass's outcomes, the fixed label set of its counter.
const (
	extenderSeedOutcomePublished  = "published"
	extenderSeedOutcomeCurrent    = "current"
	extenderSeedOutcomeEmpty      = "empty"
	extenderSeedOutcomeConfig     = "config_error"
	extenderSeedOutcomeStore      = "store_error"
	extenderSeedOutcomeSign       = "sign_error"
	extenderSeedOutcomeSelfCheck  = "self_check_failed"
	extenderSeedOutcomeClaimed    = "claimed"
	extenderSeedOutcomeSuperseded = "superseded"
	extenderSeedOutcomeExhausted  = "sequence_exhausted"
)

// The margin the running size estimate keeps below the envelope cap for the
// envelope's own fields and the body header.
const extenderSeedEnvelopeOverheadBytes = 1024

// Reads the open pool, an extender's record inputs, and the revoked members.
// Variables of the publisher so a test can stand in for the database.
type extenderSeedPublisher struct {
	config         *controller.ExtenderConfig
	host           string
	networkHosts   []string
	rootPrivateKey ed25519.PrivateKey
	rootKeys       *connect.ExtenderRootKeySet
	secret         []byte
	store          server.BlobStore

	pool       func(ctx context.Context) []*model.NetworkExtenderDnsAddress
	recordOf   func(ctx context.Context, extenderId server.Id) (*model.NetworkExtender, []*model.NetworkExtenderAddress)
	revoked    func(ctx context.Context, publicKeys [][]byte) []*model.NetworkExtenderSeedRevocation
	signRecord func(extender *model.NetworkExtender, addresses []*model.NetworkExtenderAddress, issueTime time.Time, lifetime time.Duration) (*protocol.ExtenderRecord, error)
}

// The publisher of the configured network, or the config outcome when the
// configuration cannot sign or verify a bundle.
func newExtenderSeedPublisher(config *controller.ExtenderConfig, store server.BlobStore) (*extenderSeedPublisher, error) {
	host, err := controller.NormalizeExtenderSeedNetworkHost(config.NetworkHost)
	if err != nil {
		return nil, err
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		return nil, err
	}
	rootKeys, err := connect.NewExtenderRootKeySetFromHex(config.RootPublicKeys()...)
	if err != nil {
		return nil, err
	}
	secret, err := config.DirectorySecret()
	if err != nil {
		return nil, err
	}
	networkHosts := append([]string{config.NetworkHost}, config.NetworkHosts...)
	return &extenderSeedPublisher{
		config:         config,
		host:           host,
		networkHosts:   networkHosts,
		rootPrivateKey: rootPrivateKey,
		rootKeys:       rootKeys,
		secret:         secret,
		store:          store,
		pool:           model.GetActiveNetworkExtenderDnsAddresses,
		recordOf:       model.GetActiveNetworkExtenderForRecord,
		revoked:        model.GetRevokedNetworkExtendersByPublicKey,
		signRecord:     newExtenderSeedRecordSigner(config, rootPrivateKey),
	}, nil
}

// Signs one cohort member's record with the configured lifetime. The record is
// signed by controller.SignExtenderRecord, the drip's and the dns TXT sets'
// signer, so a seed record is the record every other channel carries; only a
// lifetime other than the record default re-signs the same body with that
// expiry.
func newExtenderSeedRecordSigner(
	config *controller.ExtenderConfig,
	rootPrivateKey ed25519.PrivateKey,
) func(*model.NetworkExtender, []*model.NetworkExtenderAddress, time.Time, time.Duration) (*protocol.ExtenderRecord, error) {
	return func(
		extender *model.NetworkExtender,
		addresses []*model.NetworkExtenderAddress,
		issueTime time.Time,
		lifetime time.Duration,
	) (*protocol.ExtenderRecord, error) {
		record, _, err := controller.SignExtenderRecord(config, rootPrivateKey, extender, addresses, issueTime)
		if err != nil {
			return nil, err
		}
		if lifetime == controller.ExtenderRecordExpireTimeout {
			return record, nil
		}
		body := &protocol.ExtenderRecordBody{}
		if err := proto.Unmarshal(record.Body, body); err != nil {
			return nil, err
		}
		body.ExpireTimeMs = uint64(issueTime.Add(lifetime).UnixMilli())
		return connect.SignExtenderRecord(rootPrivateKey, body)
	}
}

// What one pass did, and the bundle the origin holds after it: the one written,
// or the one kept, verified at its own issue time (its freshness is the
// mirrors' report).
type extenderSeedPublishResult struct {
	outcome string
	// why a new bundle was due, empty when none was
	reason string
	stored *connect.ExtenderSeedBundle
}

// The stored bundle as read back: verified at its own issue time when it
// verifies, and its sequence as a floor whether or not it does.
type extenderSeedStored struct {
	bundle   *connect.ExtenderSeedBundle
	sequence uint64
}

func (self *extenderSeedPublisher) readStored(artifact []byte) *extenderSeedStored {
	stored := &extenderSeedStored{}
	if len(artifact) == 0 {
		return stored
	}
	envelope, err := connect.ParseExtenderSeedEnvelope(artifact)
	if err != nil {
		return stored
	}
	body := &protocol.ExtenderSeedBody{}
	if err := proto.Unmarshal(envelope.Body, body); err != nil {
		return stored
	}
	// a floor even when it does not verify: a bundle signed by a key since
	// rotated out still holds the sequence it took
	stored.sequence = body.Sequence
	if bundle, err := extenderSeedVerifyAtIssue(envelope, self.rootKeys, self.networkHosts); err == nil {
		stored.bundle = bundle
	}
	return stored
}

// The sequence of a stored artifact for WriteExtenderSeed's last check.
func extenderSeedArtifactSequence(artifact []byte) (uint64, bool) {
	envelope, err := connect.ParseExtenderSeedEnvelope(artifact)
	if err != nil {
		return 0, false
	}
	body := &protocol.ExtenderSeedBody{}
	if err := proto.Unmarshal(envelope.Body, body); err != nil {
		return 0, false
	}
	return body.Sequence, true
}

// The sequence of a bundle issued at issueTime above floor: the issue time in
// unix milliseconds, or one past the floor when the floor is at or above it.
// False when the floor leaves no sequence.
func nextExtenderSeedSequence(issueTime time.Time, floor uint64) (uint64, bool) {
	sequence := uint64(0)
	if 0 < issueTime.UnixMilli() {
		sequence = uint64(issueTime.UnixMilli())
	}
	if sequence <= floor {
		if floor == math.MaxUint64 {
			return 0, false
		}
		sequence = floor + 1
	}
	return sequence, true
}

// The bundle's identity keys in its order.
func extenderSeedBundleKeyHexes(bundle *connect.ExtenderSeedBundle) []string {
	keyHexes := []string{}
	if bundle == nil {
		return keyHexes
	}
	for _, record := range bundle.Records {
		keyHexes = append(keyHexes, hex.EncodeToString(record.Body.PublicKey))
	}
	return keyHexes
}

// publish runs one pass at now.
func (self *extenderSeedPublisher) publish(ctx context.Context, now time.Time) *extenderSeedPublishResult {
	result := &extenderSeedPublishResult{}
	lifetime := self.config.Seed.RecordLifetime()

	artifact, err := controller.ReadExtenderSeedLatest(ctx, self.store, self.host)
	if err != nil && !errors.Is(err, controller.ErrExtenderSeedArtifactTooLarge) {
		glog.Errorf("[extenderseed]the stored bundle could not be read: %s\n", err)
		result.outcome = extenderSeedOutcomeStore
		return result
	}
	stored := self.readStored(artifact)
	result.stored = stored.bundle
	claimedFloor, err := controller.ExtenderSeedClaimedSequenceFloor(ctx, self.store, self.host)
	if err != nil {
		glog.Errorf("[extenderseed]the claimed sequences could not be listed: %s\n", err)
		result.outcome = extenderSeedOutcomeStore
		return result
	}

	epoch := connect.ExtenderEpoch(now, ExtenderSeedCohortEpochTimeout)
	storedKeyHexes := extenderSeedBundleKeyHexes(stored.bundle)
	previousKeyHexes := []string{}
	if stored.bundle != nil && connect.ExtenderEpoch(stored.bundle.IssueTime, ExtenderSeedCohortEpochTimeout) == epoch {
		previousKeyHexes = storedKeyHexes
	}
	draw := newExtenderSeedCohortDraw(self.pool(ctx), self.secret, epoch, previousKeyHexes, self.config.Seed.RecordCount)

	// the members the operator has revoked since they were published
	storedPublicKeys := [][]byte{}
	for _, keyHex := range storedKeyHexes {
		if publicKey, err := hex.DecodeString(keyHex); err == nil {
			storedPublicKeys = append(storedPublicKeys, publicKey)
		}
	}
	revocations := self.revoked(ctx, storedPublicKeys)

	result.reason = self.dueReason(stored.bundle, now, epoch, lifetime, draw, revocations)
	if result.reason == "" {
		result.outcome = extenderSeedOutcomeCurrent
		return result
	}

	issueTime := now.UTC()
	expireTime := issueTime.Add(lifetime)
	records := self.drawRecords(ctx, draw, issueTime, expireTime, lifetime)
	if len(records) == 0 {
		// nothing open to publish: the stored bundle stays and ages out
		// rather than be replaced by one that promises nothing
		result.outcome = extenderSeedOutcomeEmpty
		return result
	}
	seedRevocations, err := self.revocations(stored.bundle, records, revocations, now, lifetime)
	if err != nil {
		glog.Errorf("[extenderseed]a revocation could not be signed: %s\n", err)
		result.outcome = extenderSeedOutcomeSign
		return result
	}

	sequence, ok := nextExtenderSeedSequence(issueTime, max(stored.sequence, claimedFloor))
	if !ok {
		result.outcome = extenderSeedOutcomeExhausted
		return result
	}
	body, err := connect.NewExtenderSeedBody(self.host, sequence, issueTime, expireTime, records, seedRevocations)
	if err != nil {
		glog.Errorf("[extenderseed]the bundle is not well formed (%s): %s\n", connect.ExtenderSeedErrorCode(err), err)
		result.outcome = extenderSeedOutcomeSign
		return result
	}
	envelope, err := connect.SignExtenderSeed(self.rootPrivateKey, body)
	if err != nil {
		glog.Errorf("[extenderseed]the bundle could not be signed (%s): %s\n", connect.ExtenderSeedErrorCode(err), err)
		result.outcome = extenderSeedOutcomeSign
		return result
	}
	// a reader's check, under the published key set: the signing key must be
	// one clients accept, and every record must verify
	bundle, err := connect.VerifyExtenderSeed(envelope, self.rootKeys, self.networkHosts, now)
	if err != nil || len(bundle.Records) != len(records) {
		glog.Errorf("[extenderseed]the signed bundle does not verify under root_public_keys_hex (%s)\n", connect.ExtenderSeedErrorCode(err))
		result.outcome = extenderSeedOutcomeSelfCheck
		return result
	}
	seedArtifact, err := connect.MarshalExtenderSeedEnvelope(envelope)
	if err != nil {
		result.outcome = extenderSeedOutcomeSign
		return result
	}

	switch err := controller.WriteExtenderSeed(ctx, self.store, self.host, sequence, seedArtifact, extenderSeedArtifactSequence); {
	case err == nil:
	case errors.Is(err, controller.ErrExtenderSeedSequenceClaimed):
		result.outcome = extenderSeedOutcomeClaimed
		return result
	case errors.Is(err, controller.ErrExtenderSeedSequenceSuperseded):
		result.outcome = extenderSeedOutcomeSuperseded
		return result
	default:
		glog.Errorf("[extenderseed]the bundle could not be stored: %s\n", err)
		result.outcome = extenderSeedOutcomeStore
		return result
	}
	result.outcome = extenderSeedOutcomePublished
	result.stored = bundle
	return result
}

// Why a new bundle is due at now, or empty when the stored one stands.
func (self *extenderSeedPublisher) dueReason(
	stored *connect.ExtenderSeedBundle,
	now time.Time,
	epoch uint64,
	lifetime time.Duration,
	draw *extenderSeedCohortDraw,
	revocations []*model.NetworkExtenderSeedRevocation,
) string {
	switch {
	case stored == nil:
		return "initial"
	case connect.ExtenderEpoch(stored.IssueTime, ExtenderSeedCohortEpochTimeout) != epoch:
		return "epoch"
	case controller.ExtenderSeedRefreshTimeout <= now.Sub(stored.IssueTime):
		return "refresh"
	case stored.ExpireTime.Sub(stored.IssueTime) != lifetime:
		return "lifetime"
	}
	// a short cohort grows at the next refresh rather than on every pass: a
	// candidate the publisher passes over would otherwise ask again each time
	revokedKeys := map[string]bool{}
	for _, revocation := range stored.Revocations {
		revokedKeys[string(revocation.Body.PublicKey)] = true
	}
	for _, revocation := range revocations {
		if !revokedKeys[string(revocation.PublicKey)] {
			return "revoked"
		}
	}
	for _, keyHex := range extenderSeedBundleKeyHexes(stored) {
		if !draw.open(keyHex) {
			return "member_gone"
		}
	}
	return ""
}

// Signs records for the draw's candidates in order until the cohort is full,
// passing over every candidate whose record cannot be published.
func (self *extenderSeedPublisher) drawRecords(
	ctx context.Context,
	draw *extenderSeedCohortDraw,
	issueTime time.Time,
	expireTime time.Time,
	lifetime time.Duration,
) []*protocol.ExtenderRecord {
	records := []*protocol.ExtenderRecord{}
	usedIps := map[netip.Addr]bool{}
	addressCount := 0
	byteCount := extenderSeedEnvelopeOverheadBytes
	for candidate := draw.next(); candidate != nil; candidate = draw.next() {
		extender, addresses := self.recordOf(ctx, candidate.extenderId)
		if extender == nil || !extender.OpenChannelPublishable() {
			// gone, or moved to the gated tier or a canary channel since
			// the pool was read
			continue
		}
		// only public addresses no other record of the bundle lists
		publishable := []*model.NetworkExtenderAddress{}
		for _, address := range addresses {
			ip := address.Ip.Unmap()
			if !extenderSeedPublicAddress(ip) || usedIps[ip] {
				continue
			}
			publishable = append(publishable, address)
		}
		if len(publishable) == 0 || connect.ExtenderSeedMaxAddressCount < addressCount+len(publishable) {
			continue
		}
		record, err := self.signRecord(extender, publishable, issueTime, lifetime)
		if err != nil {
			glog.Errorf("[extenderseed]a cohort record could not be signed: %s\n", err)
			continue
		}
		recordIps, ok := extenderSeedRecordPublishable(record, self.host, issueTime, expireTime, usedIps)
		if !ok {
			continue
		}
		size := proto.Size(record)
		if connect.ExtenderSeedMaxRecordByteCount < size || connect.ExtenderSeedMaxEnvelopeByteCount < byteCount+size+4 {
			continue
		}
		if connect.ExtenderSeedMaxAddressCount < addressCount+len(recordIps) {
			continue
		}
		for _, ip := range recordIps {
			usedIps[ip] = true
		}
		addressCount += len(recordIps)
		byteCount += size + 4
		records = append(records, record)
		draw.accept(candidate)
	}
	return records
}

// Whether an address may appear in a public seed, the reader's own rule: a
// global unicast address that is not private.
func extenderSeedPublicAddress(ip netip.Addr) bool {
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate()
}

// The check of one signed record before it enters a bundle, whatever signed
// it: open tier with no canary channel, of the bundle's host, issued no later
// than the bundle and expiring no earlier, listing only public addresses no
// other record lists. Returns the record's addresses.
func extenderSeedRecordPublishable(
	record *protocol.ExtenderRecord,
	host string,
	issueTime time.Time,
	expireTime time.Time,
	usedIps map[netip.Addr]bool,
) ([]netip.Addr, bool) {
	if record == nil {
		return nil, false
	}
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(record.Body, body); err != nil {
		return nil, false
	}
	if !connect.ExtenderRecordOpen(body) {
		return nil, false
	}
	if connect.NormalizeExtenderNetworkHost(body.NetworkHost) != host || len(body.PublicKey) != ed25519.PublicKeySize {
		return nil, false
	}
	issueTimeMs := uint64(issueTime.UnixMilli())
	expireTimeMs := uint64(expireTime.UnixMilli())
	if body.IssueTimeMs == 0 || issueTimeMs < body.IssueTimeMs || body.ExpireTimeMs < expireTimeMs || body.ExpireTimeMs <= body.IssueTimeMs {
		return nil, false
	}
	if len(body.Addresses) == 0 {
		return nil, false
	}
	ips := []netip.Addr{}
	for _, address := range body.Addresses {
		if address == nil {
			return nil, false
		}
		ip, err := netip.ParseAddr(address.Ip)
		if err != nil {
			return nil, false
		}
		ip = ip.Unmap()
		if !extenderSeedPublicAddress(ip) || usedIps[ip] || slices.Contains(ips, ip) {
			return nil, false
		}
		ips = append(ips, ip)
	}
	return ips, true
}

// The revocations of a new bundle: one, freshly signed at its revocation time,
// for each stored member the operator has since revoked, and every revocation
// the stored bundle carried while a record it dominates could still be valid.
// None for a key the new bundle publishes; newest first, within the limit.
func (self *extenderSeedPublisher) revocations(
	stored *connect.ExtenderSeedBundle,
	records []*protocol.ExtenderRecord,
	revoked []*model.NetworkExtenderSeedRevocation,
	now time.Time,
	lifetime time.Duration,
) ([]*protocol.ExtenderRevocation, error) {
	published := map[string]bool{}
	for _, record := range records {
		body := &protocol.ExtenderRecordBody{}
		if err := proto.Unmarshal(record.Body, body); err == nil {
			published[string(body.PublicKey)] = true
		}
	}

	type entry struct {
		revocation  *protocol.ExtenderRevocation
		issueTimeMs uint64
	}
	byKey := map[string]*entry{}
	add := func(publicKey []byte, revocation *protocol.ExtenderRevocation, issueTimeMs uint64) {
		key := string(publicKey)
		if published[key] || len(publicKey) != ed25519.PublicKeySize {
			return
		}
		if previous, ok := byKey[key]; ok && issueTimeMs <= previous.issueTimeMs {
			return
		}
		byKey[key] = &entry{revocation: revocation, issueTimeMs: issueTimeMs}
	}

	// a record a revocation older than this dominates has expired
	carryAfterMs := uint64(max(0, now.Add(-lifetime-connect.ExtenderSeedClockSkew).UnixMilli()))
	if stored != nil {
		for _, revocation := range stored.Revocations {
			if carryAfterMs < revocation.Body.IssueTimeMs {
				add(revocation.Body.PublicKey, revocation.Revocation, revocation.Body.IssueTimeMs)
			}
		}
	}
	for _, revocation := range revoked {
		signed, _, err := controller.SignExtenderRevocation(
			self.config,
			self.rootPrivateKey,
			&model.NetworkExtender{PublicKey: revocation.PublicKey},
			revocation.RevokeTime,
		)
		if err != nil {
			return nil, err
		}
		add(revocation.PublicKey, signed, uint64(revocation.RevokeTime.UnixMilli()))
	}

	entries := []*entry{}
	for _, e := range byKey {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].issueTimeMs != entries[j].issueTimeMs {
			return entries[j].issueTimeMs < entries[i].issueTimeMs
		}
		return string(entries[i].revocation.Body) < string(entries[j].revocation.Body)
	})
	revocations := []*protocol.ExtenderRevocation{}
	for _, e := range entries {
		if connect.ExtenderSeedMaxRevocationCount <= len(revocations) {
			break
		}
		revocations = append(revocations, e.revocation)
	}
	return revocations, nil
}
