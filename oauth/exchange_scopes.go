package oauth

// The token exchange's scopes from the projection (§7.8, TEAM-M2-15; R-TEAM-8,
// D-TEAM-8, I-1, I-7, FS-19): the ExchangeScopeSource api installs once it
// hosts §8.9's gate cache. Every scope comes from the newest verified state the
// gate holds of the client's network, so the exchange grants exactly what evs
// and gkm then honor (D-ACL-21), and proj_v is the position n of the state read.
//
//	repo:<id>:<role>   §8's rows through ACL's AclExchangeScopes, which owns the
//	                   deposit|r|rw|admin table, §10.6's audience split and the
//	                   directory's scopes; one refused scope refuses itself alone
//	repo:<config>:admin  from base 1 to the bootstrap sidecar alone, the
//	                   Genesis's owner_sidecar_id while it stands as a config
//	                   writer: it creates the config repo (§11.12), pushes its
//	                   head and forms its group (§10.6); no row of §8 grants it
//	recovery:<M>       to an unadmitted client of member M alone, at gkm, for M's
//	                   own recovery wraps (§7.9's Recovery before admission row):
//	                   its sidecar holds no row in the state, its client is no
//	                   tagged or ephemeral one, M is the member its credential
//	                   names and a member of the state
//	forward_only       a sidecar S's forwarding client F (§7.4, owner decision
//	                   197; TEAM-M8-02) acts as S at every gate (D-ACL-3), so it
//	                   gets repo:<config_repo_id>:r alone, at evs and gkm, and
//	                   only while S's forwarding Delegation is live (§7.9's
//	                   forwarding row), whatever S reads: no other repo scope, no
//	                   bootstrap admin, no recovery scope, nothing at another
//	                   audience. Only the newest active forwarding child of S's
//	                   current client speaks for S (model.ForwardingClientSidecar),
//	                   so a setup that supersedes S's Delegation with other keys
//	                   ends the old F's exchange; the token names S as its sidecar
//
// Refused whole: a client whose sidecar the state revoked, reaped, parked or
// suspended, whose not_after passed or whose member left (invalid_grant); an
// unadmitted client asking anything but its own recovery scope at gkm
// (invalid_grant); a request no scope of which is granted (invalid_scope); and
// the api's audience, which is never issued (invalid_target). A network whose
// state the gate has not loaded is refused as unavailable, never guessed: a
// network the gate has never heard of is told to it, so its load starts.
//
// Nothing here reads a token or a proof; the exchange checked both. Safe for
// concurrent use.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The exchange cannot judge the request now: the network's verified state is
// not loaded, so it is refused, retryable, rather than guessed.
var ErrExchangeUnavailable = errors.New("temporarily_unavailable")

// §8.9's gate cache as the exchange reads it: sidecar.AclGateCache's methods, or
// a test's stand-in.
type ExchangeGate interface {
	// The network's newest verified state and what the cache says of it, with
	// no I/O.
	State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus)
	// Tells the cache the exchange needs the network, which starts its load.
	Changed(networkId sidecar.Id)
}

// What the projection's source reads besides the gate.
type ExchangeScopeSettings struct {
	// The clock grants and admissions expire by. nil is real time.
	Clock sidecar.Clock
}

func DefaultExchangeScopeSettings() *ExchangeScopeSettings {
	return &ExchangeScopeSettings{}
}

// The source's reads of api's records, which tests replace.
var (
	// whether the client's owner row marks it tagged or ephemeral, no member's
	// device
	exchangeScopeClientTaggedOrEphemeral = func(ctx context.Context, clientId server.Id) (bool, error) {
		return model.ClientTaggedOrEphemeral(ctx, clientId)
	}
	// whether the client is a forwarding client or a child of one
	exchangeScopeClientForwardOnly = model.ClientForwardOnly
	// the sidecar a forwarding client speaks for, nil for one that speaks for
	// none
	exchangeScopeForwardingSidecar = model.ForwardingClientSidecar
)

// The real ExchangeScopeSource over a gate.
type ProjectionExchangeScopeSource struct {
	gate     ExchangeGate
	settings *ExchangeScopeSettings
}

// The source over gate; nil settings are the defaults.
func NewProjectionExchangeScopeSource(gate ExchangeGate, settings *ExchangeScopeSettings) *ProjectionExchangeScopeSource {
	if settings == nil {
		settings = DefaultExchangeScopeSettings()
	}
	return &ProjectionExchangeScopeSource{gate: gate, settings: settings}
}

var _ ExchangeScopeSource = (*ProjectionExchangeScopeSource)(nil)

func (self *ProjectionExchangeScopeSource) now() time.Time {
	if self.settings.Clock != nil {
		return self.settings.Clock.Now()
	}
	return server.NowUtc()
}

// The acl audience a service audience takes repo scopes as, 0 for the services
// that take none; false for an audience the exchange never issues.
func exchangeAclAudience(audience string) (sidecar.AclTokenAudience, bool) {
	switch audience {
	case "evs":
		return sidecar.AclTokenAudienceEvs, true
	case "gkm":
		return sidecar.AclTokenAudienceGkm, true
	}
	if IsServiceTokenAudience(audience) {
		return 0, true
	}
	return 0, false
}

// One repo scope parsed, false for a scope that is not repo:<id>:<role>.
func parseExchangeRepoScope(scope string) (repoId sidecar.Id, role string, ok bool) {
	rest, ok := strings.CutPrefix(scope, ServiceTokenRepoScopePrefix)
	if !ok {
		return sidecar.Id{}, "", false
	}
	idText, role, ok := strings.Cut(rest, ":")
	if !ok {
		return sidecar.Id{}, "", false
	}
	repoId, err := sidecar.ParseId(idText)
	if err != nil || repoId == (sidecar.Id{}) {
		return sidecar.Id{}, "", false
	}
	return repoId, role, true
}

// The member a recovery scope names, false for a scope that is not
// recovery:<member_id>.
func parseExchangeRecoveryScope(scope string) (sidecar.Id, bool) {
	idText, ok := strings.CutPrefix(scope, ServiceTokenRecoveryScopePrefix)
	if !ok {
		return sidecar.Id{}, false
	}
	memberId, err := sidecar.ParseId(idText)
	if err != nil || memberId == (sidecar.Id{}) {
		return sidecar.Id{}, false
	}
	return memberId, true
}

// The network's newest verified state, or ErrExchangeUnavailable while the gate
// holds none.
func (self *ProjectionExchangeScopeSource) snapshot(networkId server.Id) (*sidecar.AclGateSnapshot, error) {
	if self.gate == nil {
		return nil, fmt.Errorf("%w: no gate", ErrExchangeUnavailable)
	}
	snapshot, status := self.gate.State(sidecar.Id(networkId))
	switch status {
	case sidecar.AclGateLoaded:
	case sidecar.AclGateUnknown:
		self.gate.Changed(sidecar.Id(networkId))
		return nil, fmt.Errorf("%w: the network's state is not loaded yet", ErrExchangeUnavailable)
	default:
		return nil, fmt.Errorf("%w: the network's state is not loaded", ErrExchangeUnavailable)
	}
	if snapshot == nil || snapshot.Config == nil || snapshot.Acl == nil {
		return nil, fmt.Errorf("%w: the network's state is not loaded", ErrExchangeUnavailable)
	}
	return snapshot, nil
}

// Whether the state lets the admitted sidecar take any token at now: not
// revoked or reaped, its member still a member, not parked or suspended, its
// not_after unpassed (§8.6's hard denies). A row the state does not hold is no
// admission.
func exchangeSidecarStanding(state *sidecar.ConfigState, acl *sidecar.AclState, sidecarId sidecar.Id, now time.Time) bool {
	row := state.SidecarRow(sidecarId)
	if row == nil || !acl.Admitted(sidecarId) || state.Parked(sidecarId) || state.Suspended(sidecarId) {
		return false
	}
	return row.GetNotAfter() == 0 || now.UnixMilli() < row.GetNotAfter()
}

// The member an admitted sidecar's row names, nil for a tagged sidecar.
func exchangeSidecarMember(state *sidecar.ConfigState, sidecarId sidecar.Id) *server.Id {
	admitted := state.Sidecar(sidecarId)
	if admitted == nil || admitted.MemberId == (sidecar.Id{}) || 0 < len(admitted.TagIds) {
		return nil
	}
	memberId := server.Id(admitted.MemberId)
	return &memberId
}

// The scopes for the request, from the newest verified state of its network.
func (self *ProjectionExchangeScopeSource) Grant(ctx context.Context, request *ExchangeScopeRequest) (*ExchangeScopeGrant, error) {
	if request == nil {
		return nil, fmt.Errorf("%w: no request", ErrInvalidRequest)
	}
	aclAudience, ok := exchangeAclAudience(request.Audience)
	if !ok {
		return nil, fmt.Errorf("%w: the audience is never issued", ErrInvalidTarget)
	}
	snapshot, err := self.snapshot(request.NetworkId)
	if err != nil {
		return nil, err
	}
	state := snapshot.Config
	now := self.now()

	forwardOnly, err := exchangeScopeClientForwardOnly(ctx, request.NetworkId, request.ClientId)
	if err != nil {
		return nil, fmt.Errorf("%w: the client's marks: %w", ErrExchangeUnavailable, err)
	}
	if forwardOnly {
		return self.grantForwardOnly(ctx, snapshot, request, aclAudience, now)
	}

	if request.SidecarId == nil || state.SidecarRow(sidecar.Id(*request.SidecarId)) == nil {
		return self.grantUnadmitted(ctx, snapshot, request)
	}
	sidecarId := sidecar.Id(*request.SidecarId)
	if !exchangeSidecarStanding(state, snapshot.Acl, sidecarId, now) {
		return nil, fmt.Errorf("%w: the client's sidecar is not admitted", ErrInvalidGrant)
	}

	configRepoId, err := sidecar.IdFromBytes(state.Genesis().GetConfigRepoId())
	if err != nil {
		return nil, fmt.Errorf("the state's genesis names no config repo: %w", err)
	}
	// the bootstrap's admin scope is §7.9's, never the table's
	bootstrapAdmin := aclAudience != 0 && state.Base() != nil &&
		bytes.Equal(state.Genesis().GetOwnerSidecarId(), sidecarId.Bytes()) &&
		state.IsConfigWriter(sidecarId)

	tableScopes := []string{}
	for _, scope := range request.Scopes {
		repoId, role, ok := parseExchangeRepoScope(scope)
		if ok && repoId == configRepoId && role == "admin" {
			continue
		}
		if ok {
			tableScopes = append(tableScopes, scope)
		}
	}
	tableGranted := map[string]bool{}
	if 0 < len(tableScopes) && aclAudience != 0 {
		tableGrant, err := sidecar.AclExchangeScopes(state, &sidecar.AclExchangeRequest{
			SidecarId: sidecarId,
			Audience:  aclAudience,
			Scopes:    tableScopes,
			Now:       now,
		})
		if err != nil {
			if errors.Is(err, sidecar.ErrAclExchangeScope) {
				return nil, fmt.Errorf("%w: %w", ErrInvalidScope, err)
			}
			return nil, err
		}
		for _, scope := range tableGrant.Scopes {
			tableGranted[scope] = true
		}
	}

	grant := &ExchangeScopeGrant{
		Scopes:      []string{},
		ProjVersion: snapshot.Pin.GetVersion(),
		MemberId:    exchangeSidecarMember(state, sidecarId),
	}
	for _, scope := range request.Scopes {
		repoId, role, ok := parseExchangeRepoScope(scope)
		switch {
		case ok && repoId == configRepoId && role == "admin":
			if bootstrapAdmin {
				grant.Scopes = append(grant.Scopes, scope)
			}
		case tableGranted[scope]:
			grant.Scopes = append(grant.Scopes, scope)
		}
	}
	if 0 < len(request.Scopes) && len(grant.Scopes) == 0 {
		return nil, fmt.Errorf("%w: the projection grants none of the scopes", ErrInvalidScope)
	}
	return grant, nil
}

// An unadmitted client's grant: only its own member's recovery scope at gkm, or
// a refusal.
func (self *ProjectionExchangeScopeSource) grantUnadmitted(ctx context.Context, snapshot *sidecar.AclGateSnapshot, request *ExchangeScopeRequest) (*ExchangeScopeGrant, error) {
	refused := fmt.Errorf("%w: an unadmitted client gets only its own recovery scope at gkm", ErrInvalidGrant)
	if request.Audience != "gkm" || request.MemberId == nil {
		return nil, refused
	}
	memberId := sidecar.Id(*request.MemberId)
	asked := false
	for _, scope := range request.Scopes {
		if scopeMemberId, ok := parseExchangeRecoveryScope(scope); ok && scopeMemberId == memberId {
			asked = true
		}
	}
	if !asked || snapshot.Config.Member(memberId) == nil {
		return nil, refused
	}
	taggedOrEphemeral, err := exchangeScopeClientTaggedOrEphemeral(ctx, request.SubjectClientId)
	if err != nil {
		return nil, fmt.Errorf("%w: the client's owner row: %w", ErrExchangeUnavailable, err)
	}
	if taggedOrEphemeral {
		return nil, refused
	}
	grant := &ExchangeScopeGrant{
		Scopes:      []string{},
		ProjVersion: snapshot.Pin.GetVersion(),
		MemberId:    request.MemberId,
	}
	for _, scope := range request.Scopes {
		if scopeMemberId, ok := parseExchangeRecoveryScope(scope); ok && scopeMemberId == memberId {
			grant.Scopes = append(grant.Scopes, scope)
		}
	}
	return grant, nil
}

// A forwarding client's grant: the config's r at evs or gkm while its sidecar's
// forwarding Delegation is live, or a refusal.
func (self *ProjectionExchangeScopeSource) grantForwardOnly(ctx context.Context, snapshot *sidecar.AclGateSnapshot, request *ExchangeScopeRequest, aclAudience sidecar.AclTokenAudience, now time.Time) (*ExchangeScopeGrant, error) {
	if aclAudience == 0 {
		return nil, fmt.Errorf("%w: a forwarding client exchanges for the config's r at evs and gkm alone", ErrInvalidScope)
	}
	forwardSidecarId, err := exchangeScopeForwardingSidecar(ctx, request.NetworkId, request.ClientId)
	if err != nil {
		return nil, fmt.Errorf("%w: the forwarding client's sidecar: %w", ErrExchangeUnavailable, err)
	}
	if forwardSidecarId == nil {
		return nil, fmt.Errorf("%w: the client speaks for no sidecar", ErrInvalidGrant)
	}
	state := snapshot.Config
	sidecarId := sidecar.Id(*forwardSidecarId)
	if !exchangeSidecarStanding(state, snapshot.Acl, sidecarId, now) {
		return nil, fmt.Errorf("%w: the forwarding client's sidecar is not admitted", ErrInvalidGrant)
	}
	tableGrant, err := sidecar.AclExchangeScopes(state, &sidecar.AclExchangeRequest{
		SidecarId:   sidecarId,
		Audience:    aclAudience,
		Scopes:      request.Scopes,
		ForwardOnly: true,
		Now:         now,
	})
	if err != nil {
		if errors.Is(err, sidecar.ErrAclExchangeScope) {
			return nil, fmt.Errorf("%w: %w", ErrInvalidScope, err)
		}
		return nil, err
	}
	if len(tableGrant.Scopes) == 0 {
		return nil, fmt.Errorf("%w: a forwarding client gets the config's r alone, while its sidecar's forwarding Delegation is live", ErrInvalidScope)
	}
	return &ExchangeScopeGrant{
		Scopes:      tableGrant.Scopes,
		ProjVersion: snapshot.Pin.GetVersion(),
		MemberId:    exchangeSidecarMember(state, sidecarId),
		SidecarId:   forwardSidecarId,
	}, nil
}
