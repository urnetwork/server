package sidecart3

// §22.8's version rows beyond the schedules (TEST-M2-16; D-TEST-7): "sidecar N vs server N±1 ... explicit VERSION or
// UNIMPLEMENTED". A version pair is two sides of §6's subprotocol that disagree, and every disagreement must be
// answered with one of the two codes, never a silent misparse or a hang. The other half of that row, "migration N+1
// before code", is FS-55's (server/local/fs55, OPS-M1-02), cited here and not run.
//
// What runs: both sides are this build's dispatchers on server's exchange (exchange.go), each a VersionSide that plants
// what a build on the other side of the pair would send. The newer side calls message types the older has no handler
// for, and a raw client of the newer's sends an envelope of the next wire major; the older side calls a type the newer
// retired, and a raw client of the older's sends the previous major. Each must come back as Error{UNIMPLEMENTED} or
// Error{VERSION} with the receiver's range, as rpc_subprotocol.go answers them.
//
// What does not run, and reads open in every result: a side built from another commit set. In this build N has no
// release, so N is the previous gate's commit set, which a run names (VersionSide.CommitSet) but this file does not
// build; until a build of N's commit set runs against this one, the pair proves this build's answers to a planted newer
// or older peer, not an old binary's parse of a new one. Safe for concurrent use once built.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

// One of a T3 pair of §6 dispatchers: its platform client, its sidecar id and its dispatcher.
type T3SubprotocolSide struct {
	Name        string
	Client      *sidecartest.NetPlatformClient
	SidecarId   sidecar.Id
	Subprotocol *sidecar.Subprotocol

	closeOnce sync.Once
}

// Ends the side's dispatcher, client and transport, once: a row that ends a device mid-way and its test's cleanup may
// both call it.
func (self *T3SubprotocolSide) Close() {
	self.closeOnce.Do(func() {
		self.Subprotocol.Close()
		self.Client.Close()
	})
}

// A side's state as its dispatcher reads it: the shared verified state, for its own id.
type t3SubprotocolAdmissions struct {
	authority *sidecartest.FakeAuthority
	sidecarId sidecar.Id
}

func (self *t3SubprotocolAdmissions) Authority() sidecar.SidecarAuthority {
	return self.authority
}

func (self *t3SubprotocolAdmissions) Admitted() bool {
	admission := self.authority.Admission(self.sidecarId)
	return admission != nil && admission.Row.GetRevokedAt() == 0
}

// Admitted sidecars of one network in one verified state, a dispatcher each on a Required client of platform, the
// client settings each opens with adjusted by adjust (nil for none). The caller closes each side.
func newT3SubprotocolSides(ctx context.Context, t testing.TB, platform *exchangePlatform, adjust func(name string, settings *connect.ClientSettings), names ...string) ([]*T3SubprotocolSide, error) {
	networkId := sidecar.NewId()
	authority := sidecartest.NewFakeAuthority()
	sides := []*T3SubprotocolSide{}
	fail := func(err error) ([]*T3SubprotocolSide, error) {
		for _, side := range sides {
			side.Close()
		}
		return nil, err
	}
	for _, name := range names {
		clientKeySeed := make([]byte, ed25519.SeedSize)
		identitySeed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(clientKeySeed); err != nil {
			return fail(err)
		}
		if _, err := rand.Read(identitySeed); err != nil {
			return fail(err)
		}
		clientSettings := connect.DefaultClientSettings()
		sidecar.ApplySidecarClientSettings(clientSettings)
		clientSettings.ClientKeySeed = bytes.Clone(clientKeySeed)
		if adjust != nil {
			adjust(name, clientSettings)
		}
		platformClient, err := platform.OpenClient(ctx, name, clientSettings)
		if err != nil {
			return fail(err)
		}
		sidecarId := sidecar.NewId()
		binding, err := sidecar.NewSidecarIdentityBinding(&sidecar.SidecarIdentityBinding{
			NetworkId:  networkId,
			SidecarId:  sidecarId,
			ClientId:   sidecar.Id(platformClient.ClientId),
			Generation: 1,
			IssuedAt:   time.Now(),
		}, identitySeed, clientKeySeed)
		if err != nil {
			platformClient.Close()
			return fail(err)
		}
		at := authority.AdmitSidecar(&sidecartest.FakeAuthorityAdmission{
			SidecarId:         sidecarId,
			MemberId:          sidecar.NewId(),
			IdentityPublicKey: ed25519.NewKeyFromSeed(identitySeed).Public().(ed25519.PublicKey),
		})
		chain := sha256.Sum256(binary.BigEndian.AppendUint64([]byte("urnetwork-sidecar-test/t3-pair/"), at))
		authority.SetPin(&bridge.AuthPin{Version: at, Chain: chain[:]})
		protocolSettings := sidecar.DefaultSidecarProtocolSettings()
		subprotocol, err := sidecar.NewSubprotocol(ctx, &sidecar.SubprotocolEnv{
			Client:     platformClient.Client,
			Store:      sidecartest.NewFakeStore().Root(),
			Settings:   protocolSettings,
			Budget:     sidecar.NewSubprotocolBudget(protocolSettings, 64*1024*1024),
			Inbound:    sidecar.SubprotocolInboundWithoutPolicy,
			NetworkId:  networkId,
			Binding:    binding,
			Admissions: &t3SubprotocolAdmissions{authority: authority, sidecarId: sidecarId},
		})
		if err != nil {
			platformClient.Close()
			return fail(err)
		}
		sides = append(sides, &T3SubprotocolSide{Name: name, Client: platformClient, SidecarId: sidecarId, Subprotocol: subprotocol})
	}
	return sides, nil
}

// One side of a version pair: what it was built from, and what it plants that the other side's build would not know.
type VersionSide struct {
	// "N" or "N+1", for a report.
	Name string
	// The commit set the side stands for, by repo, as the run names it: the previous gate's for N. Only this build
	// runs here (the file header), so this names what the pair stands for and builds nothing.
	CommitSet map[string]string
	// Message types this side calls that the other's build has no handler for: a newer side's new types, or an older
	// side's retired ones.
	PlantedMessageTypes []sidecarprotocol.MessageType
	// The wire majors a raw envelope of this side's carries, one envelope each: the next one for a newer side, the
	// previous (0 beside this build's 1) for an older.
	PlantedWireVersions []uint32
}

// N against N+1 (or N-1): the sides of one §22.8 version row.
type VersionPair struct {
	Older *VersionSide
	Newer *VersionSide
}

// What one planted disagreement was answered with.
type VersionPairAnswer struct {
	From string
	To   string
	// The planted message type, or PING for a planted major.
	MessageType sidecarprotocol.MessageType
	// Whether a major was planted, which only VERSION answers, and which; a planted type only UNIMPLEMENTED answers.
	PlantedMajor bool
	WireVersion  uint32
	// The code the receiver answered, ERR_UNSPECIFIED when none came.
	Code sidecarprotocol.ErrorCode
	// VERSION's range, when it came.
	MinVersion uint32
	MaxVersion uint32
	// A failure that is not an answer: a send that went nowhere, a reply that did not parse, a call answered 200.
	Err error
}

// "newer to older, message type 0x8f01".
func (self *VersionPairAnswer) String() string {
	if self.PlantedMajor {
		return fmt.Sprintf("%s to %s, wire major %d", self.From, self.To, self.WireVersion)
	}
	return fmt.Sprintf("%s to %s, message type 0x%04x", self.From, self.To, uint32(self.MessageType))
}

// What a pair's run found.
type VersionPairResult struct {
	Answers []*VersionPairAnswer
	// What the pair stands for and did not run.
	Open []string
}

// Each disagreement not answered as §22.8 has it: a planted type with anything but UNIMPLEMENTED, a planted major with
// anything but VERSION and the receiver's range.
func (self *VersionPairResult) Problems() []string {
	problems := []string{}
	for _, answer := range self.Answers {
		want := sidecarprotocol.ErrorCode_ERR_UNIMPLEMENTED
		if answer.PlantedMajor {
			want = sidecarprotocol.ErrorCode_ERR_VERSION
		}
		switch {
		case answer.Err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", answer, answer.Err))
		case answer.Code != want:
			problems = append(problems, fmt.Sprintf("%s: answered %s, not %s", answer, answer.Code, want))
		case want == sidecarprotocol.ErrorCode_ERR_VERSION && (answer.MinVersion != sidecar.WireVersionMajor || answer.MaxVersion != sidecar.WireVersionMajor):
			problems = append(problems, fmt.Sprintf("%s: VERSION named %d to %d, not the receiver's %d", answer, answer.MinVersion, answer.MaxVersion, sidecar.WireVersionMajor))
		}
	}
	return problems
}

// What a version pair's run waits on, in real time, since connect's timers are real (X9).
type VersionPairSettings struct {
	// One call's or probe's answer.
	AnswerTimeout time.Duration
}

func DefaultVersionPairSettings() *VersionPairSettings {
	return &VersionPairSettings{
		AnswerTimeout: 30 * time.Second,
	}
}

// Runs pair on platform: a dispatcher for each side and a raw client beside each, every planted type called and every
// planted major sent both ways. settings nil takes the defaults.
func RunVersionPair(ctx context.Context, t testing.TB, platform *exchangePlatform, pair *VersionPair, settings *VersionPairSettings) (*VersionPairResult, error) {
	if settings == nil {
		settings = DefaultVersionPairSettings()
	}
	result := &VersionPairResult{
		Open: []string{fmt.Sprintf(
			"%s's commit set %v against %s's %v: both sides are this build, so no binary of another commit set ran (the file header)",
			pair.Older.Name, pair.Older.CommitSet, pair.Newer.Name, pair.Newer.CommitSet,
		)},
	}
	sides, err := newT3SubprotocolSides(ctx, t, platform, nil, pair.Older.Name, pair.Newer.Name)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, side := range sides {
			side.Close()
		}
	}()
	older, newer := sides[0], sides[1]
	// a handler of each side's own, so a planted type is the only thing the receiver lacks
	for _, side := range sides {
		if _, err := side.Subprotocol.Handle(versionPairEchoType, func(ctx context.Context, request *sidecar.SubprotocolRequest) ([]byte, error) {
			return request.Body, nil
		}, nil); err != nil {
			return nil, err
		}
	}
	for _, c := range []struct {
		from *T3SubprotocolSide
		to   *T3SubprotocolSide
		side *VersionSide
	}{{from: newer, to: older, side: pair.Newer}, {from: older, to: newer, side: pair.Older}} {
		// the pair's own type answers, so a planted type's refusal is the receiver's choice and not a broken pair
		callCtx, cancel := context.WithTimeout(ctx, settings.AnswerTimeout)
		echo, err := c.from.Subprotocol.Call(callCtx, sidecar.Id(c.to.Client.ClientId), versionPairEchoType, []byte("echo"), nil)
		cancel()
		if err != nil || !bytes.Equal(echo, []byte("echo")) {
			return nil, fmt.Errorf("%s to %s: the pair's own type did not answer: %v", c.from.Name, c.to.Name, err)
		}
		for _, messageType := range c.side.PlantedMessageTypes {
			answer := &VersionPairAnswer{From: c.from.Name, To: c.to.Name, MessageType: messageType}
			callCtx, cancel := context.WithTimeout(ctx, settings.AnswerTimeout)
			_, err := c.from.Subprotocol.Call(callCtx, sidecar.Id(c.to.Client.ClientId), messageType, []byte("planted"), nil)
			cancel()
			subprotocolError := &sidecar.SubprotocolError{}
			switch {
			case err == nil:
				answer.Err = errors.New("the receiver answered a type its build has no handler for")
			case errors.As(err, &subprotocolError):
				answer.Code = subprotocolError.Code
				answer.MinVersion = subprotocolError.MinVersion
				answer.MaxVersion = subprotocolError.MaxVersion
			default:
				answer.Err = err
			}
			result.Answers = append(result.Answers, answer)
		}
		for _, wireVersion := range c.side.PlantedWireVersions {
			answer, err := probeWireVersion(ctx, t, platform, c.side, wireVersion, c.to, settings)
			if err != nil {
				return nil, err
			}
			result.Answers = append(result.Answers, answer)
		}
	}
	return result, nil
}

// A type both sides of a pair handle: service 0x8f, past every service §6.11 assigns (a type is service << 8 | message).
const versionPairEchoType = sidecarprotocol.MessageType(0x8f00)

// A raw client of side's sends to's client one envelope of wireVersion, a PING asking an answer, and reads the answer to
// it.
func probeWireVersion(ctx context.Context, t testing.TB, platform *exchangePlatform, side *VersionSide, wireVersion uint32, to *T3SubprotocolSide, settings *VersionPairSettings) (*VersionPairAnswer, error) {
	answer := &VersionPairAnswer{From: side.Name + "'s raw client", To: to.Name, MessageType: sidecarprotocol.MessageType_PING, PlantedMajor: true, WireVersion: wireVersion}
	clientSettings := connect.DefaultClientSettings()
	sidecar.ApplySidecarClientSettings(clientSettings)
	probe, err := platform.OpenClient(ctx, side.Name+"-probe", clientSettings)
	if err != nil {
		return nil, err
	}
	defer probe.Close()
	requestIdBytes := make([]byte, 8)
	if _, err := rand.Read(requestIdBytes); err != nil {
		return nil, err
	}
	requestId := binary.BigEndian.Uint64(requestIdBytes) | 1
	replies := make(chan *sidecarprotocol.Envelope, 4)
	unregister, err := connect.RegisterSubprotocol(
		probe.Client,
		connect.SubprotocolId(sidecar.SubprotocolId),
		connect.ProtoCodec[*sidecarprotocol.Envelope](),
		func(source connect.TransferPath, envelope *sidecarprotocol.Envelope, peer connect.Peer) {
			if source.SourceId != to.Client.ClientId || envelope.RequestId != requestId {
				return
			}
			select {
			case replies <- proto.Clone(envelope).(*sidecarprotocol.Envelope):
			default:
			}
		},
	)
	if err != nil {
		return nil, err
	}
	defer unregister()
	envelope := &sidecarprotocol.Envelope{
		Version:   wireVersion,
		Type:      uint32(sidecarprotocol.MessageType_PING),
		RequestId: requestId,
		TimeoutMs: uint32(settings.AnswerTimeout.Milliseconds()),
	}
	if !connect.SendSubprotocol(probe.Client, connect.SubprotocolId(sidecar.SubprotocolId), envelope, to.Client.ClientId, nil) {
		answer.Err = errors.New("the probe's envelope was not sent")
		return answer, nil
	}
	select {
	case reply := <-replies:
		if reply.Type != uint32(sidecarprotocol.MessageType_ERROR) {
			answer.Err = fmt.Errorf("an answer of type %d, not an Error", reply.Type)
			return answer, nil
		}
		refusal := &sidecarprotocol.Error{}
		if err := proto.Unmarshal(reply.Body, refusal); err != nil {
			answer.Err = fmt.Errorf("an Error that does not parse: %w", err)
			return answer, nil
		}
		answer.Code = sidecarprotocol.ErrorCode(refusal.Code)
		answer.MinVersion = refusal.MinVersion
		answer.MaxVersion = refusal.MaxVersion
	case <-time.After(settings.AnswerTimeout):
		answer.Err = fmt.Errorf("no answer within %s: a silent drop", settings.AnswerTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return answer, nil
}

// The planted types a newer side of this build calls: in service 0x8f, past every service §6.11 assigns, after
// versionPairEchoType.
func DefaultNewerPlantedMessageTypes() []sidecarprotocol.MessageType {
	return []sidecarprotocol.MessageType{0x8f01, 0x8f02}
}
