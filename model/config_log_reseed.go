package model

// §7.9's site loss at api (TEAM-M3-02; D-TEAM-19, I-16, FS-41, FI-5, FI-10;
// §19.10): the authorization log comes back from devices, never re-sequenced,
// while sidecar.yml's reseed is true.
//
//	upload   POST /network/config/reseed {genesis, entries, rows, pins,
//	         config_pushes} from a member device that is not read_only: the
//	         verbatim run it keeps (§7.9's retain row), the rows of its bases,
//	         or a phone's (k, A_k) pins. api keeps each sidecar's newest upload
//	         in this process's memory, soft state the devices upload again
//	         until the log resumes
//	verify   each upload's run is checked as a cold gate starts (a rows-only
//	         ConfigLog from the run's newest base: its links, every base's and
//	         statement's signature and each signer rule onward), and its
//	         uploader must stand in it as an untagged, not ephemeral, not
//	         read_only member device; an upload that fails counts for nothing
//	adopt    position by position from the oldest that devices of two members
//	         hold, by bytes or pin, each entry on its predecessor's digest; one
//	         member's where the run admits uploading devices of one member only;
//	         a position two members do not agree on stops the log before it, so
//	         no lone device fills a position with a forged TICK, a reordered
//	         statement or one it signs (AdoptConfigRun). The adopted run is then
//	         verified again as one run, which compares every later base's
//	         tail_verdicts with the verdicts judged on the run (DESIGN-DETAILS
//	         5.7), and must agree with the restored network_acl_head's (n, A_n)
//	resume   an admin's device, once it has re-seeded the config repo, uploads
//	         config_pushes from version 1 to the re-seeded head: api writes the
//	         adopted run to proj/ at its own positions, the Signed Genesis at 0
//	         and the rows to projrows/, only once the newest adopted base's
//	         (config_version, config_head_sha256) lies on that walk from C_0
//	         (plan-TEAM-api-config-head-reach (5)); a run that fails is
//	         discarded, written nowhere. The admin's device then publishes a
//	         base at tip+1 through POST /network/config/projection, and signers
//	         re-submit what no adopted entry holds
//
// Nothing here writes a position another entry holds: a PUT that finds other
// bytes refuses the whole run, so positions and digests resume unchanged and
// readers apply them without a FORK. A network with no restored
// network_acl_head row and no upload has nothing to adopt.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of a site loss's uploads.
type ConfigReseedSettings struct {
	// The most entries one upload carries: 16,384, past the about two fold
	// spans of 1,024 a device keeps and the 4,096 a base's tail may hold.
	MaxEntryCount int
	// The most rows one upload carries, one per base among its entries: 64.
	MaxRowsCount int
	// The most pins a phone's upload carries: 16,384, one per entry.
	MaxPinCount int
	// The most config pushes an admin's upload carries, from version 1 to the
	// re-seeded head: 65,536, a long-lived network's config history.
	MaxConfigPushCount int
	// The most devices whose uploads a network keeps: 4,096; past it the
	// newest replaces the oldest.
	MaxUploadCount int
}

func DefaultConfigReseedSettings() *ConfigReseedSettings {
	return &ConfigReseedSettings{
		MaxEntryCount:      16384,
		MaxRowsCount:       64,
		MaxPinCount:        16384,
		MaxConfigPushCount: 65536,
		MaxUploadCount:     4096,
	}
}

var (
	// No run that devices of two members hold, or one member's where one is
	// all the network has: nothing is adopted.
	ErrConfigReseedNoRun = errors.New("no run the network's members' devices agree on")
	// The adopted run does not pass through the restored network_acl_head's
	// (n, A_n), or ends below it.
	ErrConfigReseedDisagrees = errors.New("the adopted run disagrees with the restored network_acl_head")
	// The adopted run, verified as one run, fails: a base whose tail_verdicts
	// disagree with the run, or a run no cold start takes.
	ErrConfigReseedRunRefused = errors.New("the adopted run does not verify")
)

// One device's upload.
type ConfigReseedUpload struct {
	// the uploading device, from its client's binding
	SidecarId server.Id
	// the Signed Genesis, verbatim; nil for an upload of pins alone
	SignedGenesis []byte
	// the verbatim AuthEntry bytes it keeps, consecutive, in position order
	Entries [][]byte
	// the rows of each base among the entries, verbatim
	Rows [][]byte
	// a phone's (k, A_k) pins, for positions it keeps no bytes of
	Pins []*bridge.AuthPin
}

// What the uploads give.
type ConfigReseedAdoption struct {
	SignedGenesis []byte
	// the adopted entries in position order, from FirstPosition
	Entries [][]byte
	// the rows the adopted bases name, by rows_sha256
	Rows map[[sha256.Size]byte][]byte
	// the first adopted position and the newest adopted pin
	FirstPosition uint64
	Tip           *bridge.AuthPin
	// the position the log stops before because no two members agree on it;
	// 0 when every position some device holds was adopted
	StoppedAt uint64
	// the state at Tip, verified as one run
	state *sidecar.ConfigState
}

// The newest adopted base's header, nil when the run holds none.
func (self *ConfigReseedAdoption) NewestBase() *bridge.ProjectionHeader {
	if self.state == nil {
		return nil
	}
	return self.state.Base()
}

// One position's candidates: a digest, the entry's bytes where an upload holds
// them, and the members whose devices hold it.
type configReseedCandidate struct {
	entryBytes []byte
	prevChain  []byte
	memberIds  map[server.Id]bool
}

// A rows-only log's settings for a reseed's checks, its refusals logged by the
// log.
func configReseedLogSettings() *sidecar.ConfigLogSettings {
	return configLogVerifierSettings()
}

// The rows of uploads by rows_sha256.
func configReseedRows(uploads []*ConfigReseedUpload) map[[sha256.Size]byte][]byte {
	rows := map[[sha256.Size]byte][]byte{}
	for _, upload := range uploads {
		for _, rowsBytes := range upload.Rows {
			rows[sha256.Sum256(rowsBytes)] = rowsBytes
		}
	}
	return rows
}

func configReseedRowsSource(rows map[[sha256.Size]byte][]byte) sidecar.ConfigRowsOnlySource {
	return func(networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
		if len(rowsSha256) != sha256.Size {
			return nil, errors.New("rows_sha256 of the wrong length")
		}
		rowsBytes, found := rows[[sha256.Size]byte(rowsSha256)]
		if !found {
			return nil, fmt.Errorf("no upload holds rows %x", rowsSha256[:8])
		}
		return rowsBytes, nil
	}
}

// A run verified as a cold gate starts: the state at its end.
func verifyConfigReseedRun(signedGenesis []byte, entries [][]byte, rows map[[sha256.Size]byte][]byte) (*sidecar.ConfigState, error) {
	if len(entries) == 0 {
		return nil, errors.New("no entries")
	}
	first := &bridge.AuthEntry{}
	if err := proto.Unmarshal(entries[0], first); err != nil {
		return nil, err
	}
	var log *sidecar.ConfigLog
	var err error
	if first.GetN() == 1 {
		log, err = sidecar.NewConfigLogRowsOnly(configReseedLogSettings(), signedGenesis)
		if err == nil {
			_, err = log.ApplyRun(entries, configReseedRowsSource(rows))
		}
	} else {
		log, err = sidecar.RestoreConfigLogRowsOnly(configReseedLogSettings(), &sidecar.ConfigRetained{SignedGenesis: signedGenesis, Entries: entries}, configReseedRowsSource(rows), nil)
	}
	if err != nil {
		return nil, err
	}
	return log.State(), nil
}

// The member a device uploads for: its row in state, untagged, not ephemeral,
// not read_only, admitted and standing; false for any other device.
func configReseedUploaderMember(state *sidecar.ConfigState, sidecarId server.Id) (server.Id, bool) {
	row := state.SidecarRow(sidecar.Id(sidecarId))
	if row == nil || row.GetRevokedAt() != 0 || row.GetParkedAt() != 0 || row.GetEphemeral() || len(row.GetTagIds()) != 0 ||
		row.GetCapabilities()&uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_READ_ONLY) != 0 {
		return server.Id{}, false
	}
	memberId, err := sidecar.IdFromBytes(row.GetMemberId())
	if err != nil || state.Member(memberId) == nil {
		return server.Id{}, false
	}
	return server.Id(memberId), true
}

// How many members' devices must hold a position: two, or one where the
// state's members with a device that may upload, an untagged, not ephemeral,
// not read_only, standing one, are one or none.
func configReseedRequiredMembers(state *sidecar.ConfigState) int {
	memberIds := map[server.Id]bool{}
	for _, sidecarId := range state.SidecarIds() {
		if memberId, ok := configReseedUploaderMember(state, server.Id(sidecarId)); ok {
			memberIds[memberId] = true
		}
	}
	return min(2, max(1, len(memberIds)))
}

// The run the uploads give the network (see the file header): every upload
// verified under each genesis some upload holds, then adopted position by
// position. restoredHead is the restored network_acl_head's pin, nil for none.
func AdoptConfigRun(restoredHead *bridge.AuthPin, uploads []*ConfigReseedUpload) (*ConfigReseedAdoption, error) {
	genesisCandidates := [][]byte{}
	for _, upload := range uploads {
		if upload.SignedGenesis != nil && !slices.ContainsFunc(genesisCandidates, func(candidate []byte) bool { return bytes.Equal(candidate, upload.SignedGenesis) }) {
			genesisCandidates = append(genesisCandidates, upload.SignedGenesis)
		}
	}
	var adopted *ConfigReseedAdoption
	var lastErr error = ErrConfigReseedNoRun
	for _, signedGenesis := range genesisCandidates {
		adoption, err := adoptConfigRunUnder(signedGenesis, restoredHead, uploads)
		if err != nil {
			lastErr = err
			continue
		}
		if adopted != nil {
			// two geneses that members agree on: a fork no server picks between
			return nil, fmt.Errorf("%w: uploads agree on two geneses", ErrConfigReseedNoRun)
		}
		adopted = adoption
	}
	if adopted == nil {
		return nil, lastErr
	}
	return adopted, nil
}

func adoptConfigRunUnder(signedGenesis []byte, restoredHead *bridge.AuthPin, uploads []*ConfigReseedUpload) (*ConfigReseedAdoption, error) {
	rows := configReseedRows(uploads)
	type verifiedUpload struct {
		upload   *ConfigReseedUpload
		state    *sidecar.ConfigState
		memberId server.Id
	}
	verified := []*verifiedUpload{}
	var best *sidecar.ConfigState
	for _, upload := range uploads {
		if len(upload.Entries) == 0 || !bytes.Equal(upload.SignedGenesis, signedGenesis) {
			continue
		}
		state, err := verifyConfigReseedRun(signedGenesis, upload.Entries, rows)
		if err != nil {
			// a run that does not verify counts for nothing
			continue
		}
		memberId, ok := configReseedUploaderMember(state, upload.SidecarId)
		if !ok {
			continue
		}
		verified = append(verified, &verifiedUpload{upload: upload, state: state, memberId: memberId})
		if best == nil || best.Pin().GetVersion() < state.Pin().GetVersion() {
			best = state
		}
	}
	if best == nil {
		return nil, ErrConfigReseedNoRun
	}
	required := configReseedRequiredMembers(best)

	genesisHolders := map[server.Id]bool{}
	candidates := map[uint64]map[[sha256.Size]byte]*configReseedCandidate{}
	vote := func(n uint64, chain [sha256.Size]byte, memberId server.Id, entryBytes []byte, prevChain []byte) {
		byChain := candidates[n]
		if byChain == nil {
			byChain = map[[sha256.Size]byte]*configReseedCandidate{}
			candidates[n] = byChain
		}
		candidate := byChain[chain]
		if candidate == nil {
			candidate = &configReseedCandidate{memberIds: map[server.Id]bool{}}
			byChain[chain] = candidate
		}
		candidate.memberIds[memberId] = true
		if entryBytes != nil && candidate.entryBytes == nil {
			candidate.entryBytes = entryBytes
			candidate.prevChain = prevChain
		}
	}
	for _, upload := range verified {
		genesisHolders[upload.memberId] = true
		for _, entryBytes := range upload.upload.Entries {
			entry := &bridge.AuthEntry{}
			if proto.Unmarshal(entryBytes, entry) != nil {
				continue
			}
			chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
			if err != nil {
				continue
			}
			vote(entry.GetN(), chain, upload.memberId, entryBytes, entry.GetPrevChain())
		}
	}
	// pins: a device that keeps no bytes vouches for digests, its standing read
	// in the best verified state
	for _, upload := range uploads {
		if len(upload.Pins) == 0 {
			continue
		}
		memberId, ok := configReseedUploaderMember(best, upload.SidecarId)
		if !ok {
			continue
		}
		for _, pin := range upload.Pins {
			if len(pin.GetChain()) != sha256.Size || pin.GetVersion() < 1 {
				continue
			}
			vote(pin.GetVersion(), [sha256.Size]byte(pin.GetChain()), memberId, nil, nil)
		}
	}
	if len(genesisHolders) < required {
		return nil, fmt.Errorf("%w: the genesis is held by %d members' devices", ErrConfigReseedNoRun, len(genesisHolders))
	}

	// the one candidate at n that enough members hold and whose bytes some
	// upload carries, extending prevChain where it is given; false for none or
	// two
	agreed := func(n uint64, prevChain []byte) (*configReseedCandidate, [sha256.Size]byte, bool) {
		var found *configReseedCandidate
		var foundChain [sha256.Size]byte
		for chain, candidate := range candidates[n] {
			if len(candidate.memberIds) < required || candidate.entryBytes == nil {
				continue
			}
			if prevChain != nil && !bytes.Equal(candidate.prevChain, prevChain) {
				continue
			}
			if found != nil {
				return nil, [sha256.Size]byte{}, false
			}
			found, foundChain = candidate, chain
		}
		return found, foundChain, found != nil
	}
	positions := make([]uint64, 0, len(candidates))
	for n := range candidates {
		positions = append(positions, n)
	}
	slices.Sort(positions)
	adoption := &ConfigReseedAdoption{SignedGenesis: signedGenesis, Rows: map[[sha256.Size]byte][]byte{}}
	var chain [sha256.Size]byte
	for _, n := range positions {
		if adoption.Tip != nil && n != adoption.Tip.GetVersion()+1 {
			continue
		}
		var prevChain []byte
		if adoption.Tip != nil {
			prevChain = chain[:]
		}
		candidate, candidateChain, ok := agreed(n, prevChain)
		if !ok {
			if adoption.Tip != nil {
				adoption.StoppedAt = n
				break
			}
			continue
		}
		if adoption.Tip == nil {
			adoption.FirstPosition = n
		}
		adoption.Entries = append(adoption.Entries, candidate.entryBytes)
		chain = candidateChain
		adoption.Tip = &bridge.AuthPin{Version: n, Chain: bytes.Clone(chain[:])}
	}
	if adoption.Tip == nil {
		return nil, ErrConfigReseedNoRun
	}
	if adoption.StoppedAt == 0 && adoption.Tip.GetVersion() < positions[len(positions)-1] {
		adoption.StoppedAt = adoption.Tip.GetVersion() + 1
	}

	state, err := verifyConfigReseedRun(signedGenesis, adoption.Entries, rows)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfigReseedRunRefused, err)
	}
	adoption.state = state
	adoption.Rows = configReseedBaseRows(adoption.Entries, rows)

	if restoredHead != nil && 0 < restoredHead.GetVersion() {
		switch {
		case adoption.Tip.GetVersion() < restoredHead.GetVersion():
			return nil, fmt.Errorf("%w: the run ends at %d, below the head's %d", ErrConfigReseedDisagrees, adoption.Tip.GetVersion(), restoredHead.GetVersion())
		case adoption.FirstPosition <= restoredHead.GetVersion() && !state.OnChain(restoredHead):
			return nil, fmt.Errorf("%w: another A_n at %d", ErrConfigReseedDisagrees, restoredHead.GetVersion())
		}
	}
	return adoption, nil
}

// The rows the bases among entries name, from rows by rows_sha256. A base
// whose rows_sha256 is not a digest names none.
func configReseedBaseRows(entries [][]byte, rows map[[sha256.Size]byte][]byte) map[[sha256.Size]byte][]byte {
	baseRows := map[[sha256.Size]byte][]byte{}
	for _, entryBytes := range entries {
		entry := &bridge.AuthEntry{}
		if proto.Unmarshal(entryBytes, entry) != nil || entry.GetKind() != bridge.AuthEntryKindBase {
			continue
		}
		signed := &bridge.Signed{}
		header := &bridge.ProjectionHeader{}
		if proto.Unmarshal(entry.GetBody(), signed) != nil || proto.Unmarshal(signed.GetBody(), header) != nil {
			continue
		}
		// the length first, since converting a shorter slice to the array
		// panics
		if len(header.GetRowsSha256()) != sha256.Size {
			continue
		}
		rowsSha256 := [sha256.Size]byte(header.GetRowsSha256())
		if rowsBytes, found := rows[rowsSha256]; found {
			baseRows[rowsSha256] = rowsBytes
		}
	}
	return baseRows
}

// Whether the newest adopted base names a config head on the walk from C_0
// that an admin's re-seeded config repo carries (plan-TEAM-api-config-head-
// reach (5)): configPushes, every version from 1 to that base's.
func CheckConfigReseedPushes(adoption *ConfigReseedAdoption, configPushes [][]byte) error {
	base := adoption.NewestBase()
	if base == nil {
		return fmt.Errorf("%w: the adopted run holds no base", ErrConfigReseedRunRefused)
	}
	configRepoId, err := sidecar.IdFromBytes(adoption.state.Genesis().GetConfigRepoId())
	if err != nil {
		return err
	}
	header := &bridge.ProjectionHeader{ConfigVersion: base.GetConfigVersion(), ConfigHeadSha256: base.GetConfigHeadSha256()}
	if err := sidecar.CheckConfigPushes(configRepoId, nil, header, configPushes); err != nil {
		return fmt.Errorf("%w: %w", ErrConfigReseedRunRefused, err)
	}
	return nil
}

// Writes an adopted run to the store at its own positions: the Signed Genesis
// at 0, every base's rows, then each entry, all If-None-Match. A position that
// holds the same bytes is a retry; one that holds other bytes refuses the run,
// writing nothing past it. The network's verifier is then loaded again from the
// store.
func (self *ConfigLogSequencer) WriteAdoptedRun(ctx context.Context, networkId server.Id, adoption *ConfigReseedAdoption) error {
	putOnce := func(key string, value []byte) error {
		_, err := self.cluster.PutIfNoneMatch(self.issueP0(ctx, len(value)), key, value, nil)
		var unknown *sidecar.BlobUnknownError
		if err == nil {
			return nil
		}
		if !errors.Is(err, sidecar.ErrBlobPrecondition) && !errors.As(err, &unknown) {
			return err
		}
		held, found, getErr := configLogGet(ctx, self.cluster, key)
		switch {
		case getErr != nil:
			return getErr
		case found && bytes.Equal(held, value):
			return nil
		case found:
			return fmt.Errorf("%w: %s holds another entry", ErrConfigReseedDisagrees, key)
		}
		_, err = self.cluster.PutIfNoneMatch(self.issueP0(ctx, len(value)), key, value, nil)
		return err
	}
	sidecarNetworkId := sidecar.Id(networkId)
	if err := putOnce(sidecar.ConfigEntryKey(sidecarNetworkId, 0), adoption.SignedGenesis); err != nil {
		return err
	}
	rowsKeys := make([][sha256.Size]byte, 0, len(adoption.Rows))
	for rowsSha256 := range adoption.Rows {
		rowsKeys = append(rowsKeys, rowsSha256)
	}
	slices.SortFunc(rowsKeys, func(a [sha256.Size]byte, b [sha256.Size]byte) int { return bytes.Compare(a[:], b[:]) })
	for _, rowsSha256 := range rowsKeys {
		if err := putOnce(sidecar.ConfigRowsKey(sidecarNetworkId, rowsSha256[:]), adoption.Rows[rowsSha256]); err != nil {
			return err
		}
	}
	for i, entryBytes := range adoption.Entries {
		n := adoption.FirstPosition + uint64(i)
		if err := putOnce(sidecar.ConfigEntryKey(sidecarNetworkId, n), entryBytes); err != nil {
			return err
		}
	}
	network := self.network(networkId)
	func() {
		network.stateLock.Lock()
		defer network.stateLock.Unlock()
		network.log, network.signedGenesis = nil, nil
		clear(network.basePins)
		network.bases = nil
	}()
	return nil
}

// Each network's uploads this process holds: soft state, which devices upload
// again until their network's log resumes. Safe for concurrent use.
type ConfigReseedUploads struct {
	settings *ConfigReseedSettings

	stateLock sync.Mutex
	// network -> sidecar -> its newest upload, with the order they came in
	networkUploads map[server.Id]map[server.Id]*ConfigReseedUpload
	networkOrders  map[server.Id][]server.Id
}

func NewConfigReseedUploads(settings *ConfigReseedSettings) *ConfigReseedUploads {
	if settings == nil {
		settings = DefaultConfigReseedSettings()
	}
	return &ConfigReseedUploads{
		settings:       settings,
		networkUploads: map[server.Id]map[server.Id]*ConfigReseedUpload{},
		networkOrders:  map[server.Id][]server.Id{},
	}
}

// Keeps upload as its sidecar's newest for the network.
func (self *ConfigReseedUploads) Put(networkId server.Id, upload *ConfigReseedUpload) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	uploads := self.networkUploads[networkId]
	if uploads == nil {
		uploads = map[server.Id]*ConfigReseedUpload{}
		self.networkUploads[networkId] = uploads
	}
	order := slices.DeleteFunc(self.networkOrders[networkId], func(sidecarId server.Id) bool { return sidecarId == upload.SidecarId })
	order = append(order, upload.SidecarId)
	for max(1, self.settings.MaxUploadCount) < len(order) {
		delete(uploads, order[0])
		order = order[1:]
	}
	uploads[upload.SidecarId] = upload
	self.networkOrders[networkId] = order
}

// The network's uploads, in the order their sidecars last uploaded.
func (self *ConfigReseedUploads) List(networkId server.Id) []*ConfigReseedUpload {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	uploads := []*ConfigReseedUpload{}
	for _, sidecarId := range self.networkOrders[networkId] {
		uploads = append(uploads, self.networkUploads[networkId][sidecarId])
	}
	return uploads
}

// Drops the network's uploads once its run is written.
func (self *ConfigReseedUploads) Clear(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.networkUploads, networkId)
	delete(self.networkOrders, networkId)
}

// This process's uploads.
var configReseedUploads = NewConfigReseedUploads(nil)

// POST /network/config/reseed's body.
type ConfigReseedArgs struct {
	Genesis      []byte          `json:"genesis,omitempty"`
	Entries      [][]byte        `json:"entries,omitempty"`
	Rows         [][]byte        `json:"rows,omitempty"`
	Pins         []*ConfigLogPin `json:"pins,omitempty"`
	ConfigPushes [][]byte        `json:"config_pushes,omitempty"`
}

// What an upload did.
type ConfigReseedResult struct {
	// the run adopted so far, nil while none is
	Tip *ConfigLogPin `json:"tip,omitempty"`
	// the position the log stops before, 0 for none
	StoppedAt uint64 `json:"stopped_at,omitempty"`
	// the adopted run is written to the store, and the log resumes at tip+1
	Written bool `json:"written,omitempty"`
}

// The restored network_acl_head's pin, nil for none. Tests replace it.
var configReseedRestoredHead = func(ctx context.Context, networkId server.Id) (*bridge.AuthPin, error) {
	head, err := GetNetworkAclHead(ctx, networkId)
	if err != nil || head == nil {
		return nil, err
	}
	return &bridge.AuthPin{Version: head.Version, Chain: head.Chain}, nil
}

// POST /network/config/reseed (see the file header): only while sidecar.yml's
// reseed is true, from a client token whose client a sidecar binds.
func PostConfigReseed(args *ConfigReseedArgs, clientSession *session.ClientSession) (*ConfigReseedResult, error) {
	return postConfigReseed(args, clientSession, configReseedUploads, DefaultConfigReseedSettings())
}

func postConfigReseed(args *ConfigReseedArgs, clientSession *session.ClientSession, uploads *ConfigReseedUploads, settings *ConfigReseedSettings) (*ConfigReseedResult, error) {
	refused := func(status int, code string, text string) error {
		return &ConfigLogError{Status: status, Code: code, Err: errors.New(text)}
	}
	if !server.CurrentSidecarConfig().Reseed() {
		return nil, refused(http.StatusForbidden, "config_refused", "the log takes uploads only while a restore runs")
	}
	if args == nil {
		return nil, refused(http.StatusBadRequest, "config_refused", "no upload")
	}
	if settings.MaxEntryCount < len(args.Entries) || settings.MaxRowsCount < len(args.Rows) || settings.MaxPinCount < len(args.Pins) ||
		settings.MaxConfigPushCount < len(args.ConfigPushes) {
		return nil, refused(http.StatusBadRequest, "config_refused", "an upload past its bounds")
	}
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId == nil {
		return nil, refused(http.StatusForbidden, "config_unadmitted", "a sidecar's client uploads")
	}
	view := GetSidecarIdentityByClient(clientSession.Ctx, byJwt.NetworkId, *byJwt.ClientId)
	if view == nil {
		return nil, refused(http.StatusForbidden, "config_unadmitted", "the client binds no sidecar")
	}
	upload := &ConfigReseedUpload{
		SidecarId:     view.SidecarId,
		SignedGenesis: args.Genesis,
		Entries:       args.Entries,
		Rows:          args.Rows,
	}
	for _, pin := range args.Pins {
		if pin != nil {
			upload.Pins = append(upload.Pins, &bridge.AuthPin{Version: pin.N, Chain: pin.Chain})
		}
	}
	networkId := byJwt.NetworkId
	uploads.Put(networkId, upload)

	restoredHead, err := configReseedRestoredHead(clientSession.Ctx, networkId)
	if err != nil {
		return nil, refused(http.StatusServiceUnavailable, "unavailable", "the restored head cannot be read")
	}
	adoption, err := AdoptConfigRun(restoredHead, uploads.List(networkId))
	if err != nil {
		if errors.Is(err, ErrConfigReseedNoRun) {
			return &ConfigReseedResult{}, nil
		}
		return nil, &ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: err}
	}
	result := &ConfigReseedResult{Tip: configLogPinJson(adoption.Tip), StoppedAt: adoption.StoppedAt}
	if len(args.ConfigPushes) == 0 {
		return result, nil
	}
	// an admin's device has re-seeded the config repo: the run resumes only on
	// its walk from C_0, and a run that fails is discarded
	if _, ok := configReseedUploaderMember(adoption.state, view.SidecarId); !ok || !adoption.state.IsConfigWriter(sidecar.Id(view.SidecarId)) {
		return nil, refused(http.StatusForbidden, "config_refused", "only a config writer's device re-seeds the config")
	}
	if err := CheckConfigReseedPushes(adoption, args.ConfigPushes); err != nil {
		uploads.Clear(networkId)
		return nil, &ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: err}
	}
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	if err := sequencer.WriteAdoptedRun(clientSession.Ctx, networkId, adoption); err != nil {
		return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
	}
	uploads.Clear(networkId)
	result.Written = true
	return result, nil
}
