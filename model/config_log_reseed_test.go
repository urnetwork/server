package model

// §7.9's site loss at api (config_log_reseed.go; TEAM-M3-02, FS-41's log cases): devices' uploads of the log they keep,
// adopted position by position only where devices of two members agree, verified as one run, held to the restored
// head, written back at unchanged positions so the log resumes there. Each network's log is real, signed as its
// devices sign it (sidecartest's ConfigLogSuiteWorld) and appended through api's sequencer on T2's fake Blobs; a site
// loss is a second, empty cluster.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// A first metadata cluster on T2's fake Blobs.
func testReseedFakeCluster(t testing.TB) sidecar.BlobCluster {
	t.Helper()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

// A network's log as its devices keep it: the owner's first device and, in a two-member network, a member B's device.
type reseedWorld struct {
	t               testing.TB
	ctx             context.Context
	networkId       server.Id
	ownerSidecarId  server.Id
	memberSidecarId server.Id
	world           *sidecartest.ConfigLogSuiteWorld
	sequencer       *ConfigLogSequencer
	tip             *bridge.AuthPin
}

func newReseedWorld(t testing.TB, twoMembers bool) *reseedWorld {
	t.Helper()
	return newReseedWorldFor(t, server.NewId(), server.NewId(), twoMembers)
}

// A reseedWorld whose network and owner are networkId's and ownerUserId's, as a network Postgres holds.
func newReseedWorldFor(t testing.TB, networkId server.Id, ownerUserId server.Id, twoMembers bool) *reseedWorld {
	t.Helper()
	ctx := t.Context()
	cluster := testReseedFakeCluster(t)
	self := &reseedWorld{t: t, ctx: ctx, networkId: networkId, ownerSidecarId: server.NewId()}
	self.world = sidecartest.NewConfigLogSuiteWorldFor(ctx, cluster, sidecar.Id(networkId), sidecar.Id(ownerUserId), sidecar.Id(self.ownerSidecarId))
	settings := DefaultConfigLogSettings()
	settings.Clock = self.world.Clock()
	self.sequencer = NewConfigLogSequencer(cluster, settings, nil)
	if err := self.world.Start(testConfigLogSuiteSequencer{sequencer: self.sequencer}); err != nil {
		t.Fatal(err)
	}
	tip, err := self.sequencer.Tip(ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	self.tip = tip
	if twoMembers {
		memberId := server.NewId()
		self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
			Header: self.world.Header(), MemberId: sidecar.Id(memberId).Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER,
		}))
		self.memberSidecarId = server.NewId()
		self.admit(self.memberSidecarId, memberId)
	}
	// a few more entries each device keeps
	for range 3 {
		self.admit(server.NewId(), ownerUserId)
	}
	return self
}

func (self *reseedWorld) post(signed []byte) {
	self.t.Helper()
	pin, err := self.sequencer.AppendStatement(self.ctx, self.networkId, signed)
	if err != nil {
		self.t.Fatal(err)
	}
	self.world.SetTip(pin)
	self.tip = pin
}

func (self *reseedWorld) admit(sidecarId server.Id, memberId server.Id) {
	self.t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		self.t.Fatal(err)
	}
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header:            self.world.Header(),
		NetworkId:         sidecar.Id(self.networkId).Bytes(),
		SidecarId:         sidecar.Id(sidecarId).Bytes(),
		IdentityPublicKey: publicKey,
		IdentityAlg:       "ed25519",
		MemberId:          sidecar.Id(memberId).Bytes(),
	}))
}

// What a device keeps: the genesis, every entry and the rows of their bases.
func (self *reseedWorld) kept(sidecarId server.Id) *ConfigReseedUpload {
	self.t.Helper()
	upload := &ConfigReseedUpload{SidecarId: sidecarId}
	after := uint64(0)
	for {
		read, err := self.sequencer.Read(self.ctx, self.networkId, after)
		if err != nil {
			self.t.Fatal(err)
		}
		if after == 0 {
			upload.SignedGenesis = read.SignedGenesis
		}
		upload.Entries = append(upload.Entries, read.Entries...)
		upload.Rows = append(upload.Rows, read.Rows...)
		if !read.More || read.Tip == nil {
			return upload
		}
		after = read.Tip.GetVersion()
	}
}

// The base an admin's device publishes at tip+1 once the adopted run is written (§7.9's resume): signed by the
// owner's first device at the tip, over base 1's rows, fold and config head, which the re-seeded config repo still
// names, with the tail's verdicts: every statement the world lands takes effect, and a base carries 0 (DESIGN-DETAILS
// 5.2.2).
func (self *reseedWorld) resumeBase() *ConfigLogBase {
	self.t.Helper()
	kept := self.kept(self.ownerSidecarId)
	entry := &bridge.AuthEntry{}
	signed := &bridge.Signed{}
	baseOne := &bridge.ProjectionHeader{}
	if err := proto.Unmarshal(kept.Entries[0], entry); err != nil {
		self.t.Fatal(err)
	}
	if err := proto.Unmarshal(entry.GetBody(), signed); err != nil {
		self.t.Fatal(err)
	}
	if err := proto.Unmarshal(signed.GetBody(), baseOne); err != nil {
		self.t.Fatal(err)
	}
	rows := kept.Rows[0]
	rowsSha256 := sha256.Sum256(rows)
	foldedThrough := baseOne.GetFoldedThrough()
	tailVerdicts := make([]byte, (2*(self.tip.GetVersion()-foldedThrough)+7)/8)
	for n := foldedThrough + 1; n <= self.tip.GetVersion(); n += 1 {
		tailEntry := &bridge.AuthEntry{}
		if err := proto.Unmarshal(kept.Entries[n-1], tailEntry); err != nil {
			self.t.Fatal(err)
		}
		if tailEntry.GetKind() != bridge.AuthEntryKindBase {
			j := n - foldedThrough - 1
			tailVerdicts[j/4] |= 1 << (2 * (j % 4))
		}
	}
	signedHeader := self.world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, &bridge.ProjectionHeader{
		NetworkId:        sidecar.Id(self.networkId).Bytes(),
		Version:          self.tip.GetVersion() + 1,
		PrevChain:        self.tip.GetChain(),
		FoldedThrough:    foldedThrough,
		ConfigVersion:    baseOne.GetConfigVersion(),
		ConfigHeadSha256: baseOne.GetConfigHeadSha256(),
		RowsSha256:       rowsSha256[:],
		RowsByteCount:    uint32(len(rows)),
		TailVerdicts:     tailVerdicts,
	})
	return &ConfigLogBase{SignedHeader: signedHeader, Rows: rows}
}

// An empty TICK at the tip's next position, its link good: what one device forges.
func (self *reseedWorld) forgedTick() []byte {
	self.t.Helper()
	body, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.ApiTick{})
	entryBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.AuthEntry{
		NetworkId: sidecar.Id(self.networkId).Bytes(),
		N:         self.tip.GetVersion() + 1,
		PrevChain: self.tip.GetChain(),
		Kind:      bridge.AuthEntryKindTick,
		TimeMs:    self.world.Clock().Now().UnixMilli() + 1000,
		Body:      body,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return entryBytes
}

// Two members' devices holding the same log: every position adopted, at its own position, through the restored
// head, ending at the tip.
func TestAdoptConfigRunTakesWhatTwoMembersHold(t *testing.T) {
	self := newReseedWorld(t, true)
	owner, member := self.kept(self.ownerSidecarId), self.kept(self.memberSidecarId)
	adoption, err := AdoptConfigRun(self.tip, []*ConfigReseedUpload{owner, member})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, adoption.FirstPosition, uint64(1))
	connect.AssertEqual(t, adoption.Tip.GetVersion(), self.tip.GetVersion())
	connect.AssertEqual(t, bytes.Equal(adoption.Tip.GetChain(), self.tip.GetChain()), true)
	connect.AssertEqual(t, adoption.StoppedAt, uint64(0))
	connect.AssertEqual(t, len(adoption.Entries), len(owner.Entries))
	for i := range owner.Entries {
		if !bytes.Equal(adoption.Entries[i], owner.Entries[i]) {
			t.Fatalf("entry %d is not the bytes devices hold", i+1)
		}
	}
	if base := adoption.NewestBase(); base == nil || base.GetVersion() != 1 {
		t.Fatalf("the newest adopted base: %v", base)
	}
}

// FS-41: one member's forged TICK at the next position takes none; a position only one member's device holds stops
// the log before it; and with a member's run that does not verify, the other member alone adopts nothing.
func TestAdoptConfigRunTakesNoPositionOneMemberHoldsAlone(t *testing.T) {
	self := newReseedWorld(t, true)
	owner, member := self.kept(self.ownerSidecarId), self.kept(self.memberSidecarId)

	forged := *owner
	forged.Entries = append(append([][]byte{}, owner.Entries...), self.forgedTick())
	adoption, err := AdoptConfigRun(self.tip, []*ConfigReseedUpload{&forged, member})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, adoption.Tip.GetVersion(), self.tip.GetVersion())
	connect.AssertEqual(t, adoption.StoppedAt, self.tip.GetVersion()+1)

	short := *member
	short.Entries = member.Entries[:len(member.Entries)-2]
	adoption, err = AdoptConfigRun(nil, []*ConfigReseedUpload{owner, &short})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, adoption.Tip.GetVersion(), self.tip.GetVersion()-2)
	connect.AssertEqual(t, adoption.StoppedAt, self.tip.GetVersion()-1)

	tampered := *member
	tampered.Entries = append([][]byte{}, member.Entries...)
	entry := &bridge.AuthEntry{}
	connect.AssertEqual(t, proto.Unmarshal(tampered.Entries[2], entry), nil)
	entry.TimeMs += 1
	tampered.Entries[2], _ = proto.Marshal(entry)
	_, err = AdoptConfigRun(nil, []*ConfigReseedUpload{owner, &tampered})
	if !errors.Is(err, ErrConfigReseedNoRun) {
		t.Fatalf("one member's run alone: %v", err)
	}
}

// One member's devices suffice where the network admits uploading devices of one member only: a personal network.
func TestAdoptConfigRunTakesOneMembersRunInAPersonalNetwork(t *testing.T) {
	self := newReseedWorld(t, false)
	adoption, err := AdoptConfigRun(self.tip, []*ConfigReseedUpload{self.kept(self.ownerSidecarId)})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, adoption.Tip.GetVersion(), self.tip.GetVersion())
}

// The adopted run must agree with the restored network_acl_head: another A_n there, or a run ending below it,
// adopts nothing.
func TestAdoptConfigRunHoldsToTheRestoredHead(t *testing.T) {
	self := newReseedWorld(t, true)
	uploads := []*ConfigReseedUpload{self.kept(self.ownerSidecarId), self.kept(self.memberSidecarId)}
	other := bytes.Repeat([]byte{9}, 32)
	_, err := AdoptConfigRun(&bridge.AuthPin{Version: self.tip.GetVersion() - 1, Chain: other}, uploads)
	if !errors.Is(err, ErrConfigReseedDisagrees) {
		t.Fatalf("another A_n: %v", err)
	}
	_, err = AdoptConfigRun(&bridge.AuthPin{Version: self.tip.GetVersion() + 3, Chain: other}, uploads)
	if !errors.Is(err, ErrConfigReseedDisagrees) {
		t.Fatalf("a run below the head: %v", err)
	}
}

// After a site loss the adopted run is written to an empty store at unchanged positions, the genesis at 0: a reader
// gets the same log, and the log resumes at tip+1. A second write of the same run is a retry; a store holding other
// bytes at a position refuses it.
func TestAdoptedRunResumesTheLogAtUnchangedPositions(t *testing.T) {
	self := newReseedWorld(t, true)
	owner, member := self.kept(self.ownerSidecarId), self.kept(self.memberSidecarId)
	adoption, err := AdoptConfigRun(self.tip, []*ConfigReseedUpload{owner, member})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigReseedPushes(adoption, self.world.ConfigPushes(0, adoption.NewestBase().GetConfigVersion())); err != nil {
		t.Fatalf("the re-seeded config's walk: %v", err)
	}
	if err := CheckConfigReseedPushes(adoption, nil); !errors.Is(err, ErrConfigReseedRunRefused) {
		t.Fatalf("no walk: %v", err)
	}

	lost := testReseedFakeCluster(t)
	settings := DefaultConfigLogSettings()
	settings.Clock = self.world.Clock()
	restored := NewConfigLogSequencer(lost, settings, nil)
	connect.AssertEqual(t, restored.WriteAdoptedRun(self.ctx, self.networkId, adoption), nil)
	connect.AssertEqual(t, restored.WriteAdoptedRun(self.ctx, self.networkId, adoption), nil)
	tip, err := restored.Tip(self.ctx, self.networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, tip.GetVersion(), self.tip.GetVersion())
	connect.AssertEqual(t, bytes.Equal(tip.GetChain(), self.tip.GetChain()), true)
	read, err := restored.Read(self.ctx, self.networkId, 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, bytes.Equal(read.SignedGenesis, owner.SignedGenesis), true)

	// the log resumes at tip+1 with the same digests, so no reader forks
	pin, err := restored.AppendStatement(self.ctx, self.networkId, self.world.MemberAdmit())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, pin.GetVersion(), self.tip.GetVersion()+1)

	// a store holding other bytes at an adopted position refuses the run
	forked := *adoption
	forked.Entries = append([][]byte{}, adoption.Entries...)
	forked.Entries[len(forked.Entries)-1] = self.forgedTick()
	if err := restored.WriteAdoptedRun(self.ctx, self.networkId, &forked); !errors.Is(err, ErrConfigReseedDisagrees) {
		t.Fatalf("over other bytes: %v", err)
	}
}

// Uploads are taken only while sidecar.yml's reseed is true.
func TestConfigReseedIsRefusedOutsideARestore(t *testing.T) {
	_, err := postConfigReseed(&ConfigReseedArgs{}, nil, NewConfigReseedUploads(nil), DefaultConfigReseedSettings())
	configLogError := &ConfigLogError{}
	if !errors.As(err, &configLogError) || configLogError.Status != 403 {
		t.Fatalf("outside a restore: %v", err)
	}
}

// A Team whose other members hold only read_only page Workers resumes from its one uploading member's devices, and a
// read_only device's upload counts for nothing.
func TestAdoptConfigRunCountsOnlyMembersThatMayUpload(t *testing.T) {
	self := newReseedWorld(t, false)
	memberId := server.NewId()
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		Header: self.world.Header(), MemberId: sidecar.Id(memberId).Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER,
	}))
	workerId := server.NewId()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header:            self.world.Header(),
		NetworkId:         sidecar.Id(self.networkId).Bytes(),
		SidecarId:         sidecar.Id(workerId).Bytes(),
		IdentityPublicKey: publicKey,
		IdentityAlg:       "ed25519",
		MemberId:          sidecar.Id(memberId).Bytes(),
		Capabilities:      uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_READ_ONLY),
	}))
	adoption, err := AdoptConfigRun(self.tip, []*ConfigReseedUpload{self.kept(self.ownerSidecarId)})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, adoption.Tip.GetVersion(), self.tip.GetVersion())

	_, err = AdoptConfigRun(self.tip, []*ConfigReseedUpload{self.kept(workerId)})
	if !errors.Is(err, ErrConfigReseedNoRun) {
		t.Fatalf("a read_only device's upload alone: %v", err)
	}
}

// The adopted rows are those the run's bases name: a base whose rows_sha256 is short of a digest names none, and
// neither does a statement or bytes that are no entry.
func TestConfigReseedBaseRowsSkipsWhatNamesNoDigest(t *testing.T) {
	base := func(rowsSha256 []byte) []byte {
		headerBytes, err := proto.Marshal(&bridge.ProjectionHeader{RowsSha256: rowsSha256})
		if err != nil {
			t.Fatal(err)
		}
		signedBytes, err := proto.Marshal(&bridge.Signed{Body: headerBytes})
		if err != nil {
			t.Fatal(err)
		}
		entryBytes, err := proto.Marshal(&bridge.AuthEntry{Kind: bridge.AuthEntryKindBase, Body: signedBytes})
		if err != nil {
			t.Fatal(err)
		}
		return entryBytes
	}
	heldRows := []byte("rows a base names")
	heldSha256 := sha256.Sum256(heldRows)
	unheldSha256 := sha256.Sum256([]byte("rows no upload holds"))
	statement, err := proto.Marshal(&bridge.AuthEntry{Kind: bridge.AuthEntryKindStatement, Body: heldSha256[:]})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[[sha256.Size]byte][]byte{heldSha256: heldRows}

	baseRows := configReseedBaseRows([][]byte{
		base(heldSha256[:sha256.Size-1]),
		base(nil),
		base(unheldSha256[:]),
		statement,
		[]byte("no entry"),
		base(heldSha256[:]),
	}, rows)
	connect.AssertEqual(t, len(baseRows), 1)
	connect.AssertEqual(t, bytes.Equal(baseRows[heldSha256], heldRows), true)
}
