package controller

// POST /sidecar/wake {sidecar_id} (§6.4's Bound, §6.16, §18.7's step 2; D-TRUST-10, D-PROTO-5, owner decision 88;
// PROTO-M8-02): one of a member's devices summons another of that member's, a silent app host, so it witnesses again
// before witness_bound_hours stops shared writes. Client class, on §7.4's strict_exp allowlist: the caller is a
// sidecar's own client, and the summoned sidecar is another admitted device of the caller's member, both standing in
// the network's verified state, neither revoked, parked, tagged nor ephemeral. At most once a WakeInterval (1 h) a
// summoned sidecar, whoever asks, by {wake_<sidecarId>}, answering 429 past it.
//
// The wake is a forced core hint to the summoned sidecar's current client, empty of content: core's version is api's
// clock in microseconds, which the hint script raises past the stored value, and Forced makes §18.7's enqueue due at
// once whatever the client's live marker says. The enqueue needs the client's wake token, which only app hosts hold,
// so a desktop summoned this way gets a core hint and no push: the sidecar asking picks own app hosts alone, and the
// token is what keeps every other device unwoken.
//
// Dark until M8: while MG1 is not in the binary or sidecar.yml's wake switch is off it answers 503 with Retry-After.
// A process with no config log sequencer (its first metadata cluster unwired) answers 503 too: the members' rows are
// what authorize a wake, and nothing is woken unverified.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// POST /sidecar/wake's tunables.
type SidecarWakeSettings struct {
	// How often one sidecar may be summoned, whoever asks: 1 h, {wake_<sidecarId>}'s ttl (§6.16), and the sidecar's
	// own SidecarProtocolSettings.WakeInterval. Must be positive.
	Interval time.Duration
}

func DefaultSidecarWakeSettings() *SidecarWakeSettings {
	return &SidecarWakeSettings{
		Interval: 1 * time.Hour,
	}
}

// The process's settings, and the reads a test replaces: the verified state's rows, the summoned client and the hint.
var sidecarWakeSettings = DefaultSidecarWakeSettings()
var sidecarWakeRows = func(ctx context.Context, networkId server.Id, sidecarIds []sidecar.Id) (map[sidecar.Id]*bridge.SidecarRow, error) {
	sequencer, err := model.CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	return sequencer.SidecarRows(ctx, networkId, sidecarIds)
}
var sidecarWakeHint = model.SidecarHint

// POST /sidecar/wake's body.
type SidecarWakeArgs struct {
	// The sidecar to summon: another admitted device of the caller's member.
	SidecarId server.Id `json:"sidecar_id"`
}

// POST /sidecar/wake's answer: nothing but the 200.
type SidecarWakeResult struct {
}

// Whether the route is open: MG1, whose bindings name clients, in the binary, and sidecar.yml's wake switch on. The
// handler asks before it reads the body.
func SidecarWakeOpen() bool {
	return server.SidecarMg1Linked() && server.CurrentSidecarConfig().Wake()
}

// Whether a row stands as a member's device in the verified state: admitted, neither revoked nor parked, untagged
// with a member, and not ephemeral.
func sidecarWakeMemberDevice(row *bridge.SidecarRow) bool {
	return row != nil && row.GetRevokedAt() == 0 && row.GetParkedAt() == 0 && len(row.GetMemberId()) == len(sidecar.Id{}) &&
		len(row.GetTagIds()) == 0 && !row.GetEphemeral()
}

// See the file header.
func SidecarWake(args *SidecarWakeArgs, clientSession *session.ClientSession) (*SidecarWakeResult, error) {
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	if args == nil || args.SidecarId == (server.Id{}) {
		return nil, fmt.Errorf("%d A sidecar_id is required.", http.StatusBadRequest)
	}
	if byJwt.ClientId == nil {
		return nil, fmt.Errorf("%d The caller is not a client.", http.StatusForbidden)
	}
	caller := model.GetSidecarIdentityByClient(ctx, byJwt.NetworkId, *byJwt.ClientId)
	if caller == nil {
		return nil, fmt.Errorf("%d The caller is no sidecar's client.", http.StatusForbidden)
	}
	if caller.SidecarId == args.SidecarId {
		return nil, fmt.Errorf("%d A sidecar does not summon itself.", http.StatusBadRequest)
	}
	callerSidecarId, targetSidecarId := sidecar.Id(caller.SidecarId), sidecar.Id(args.SidecarId)
	rows, err := sidecarWakeRows(ctx, byJwt.NetworkId, []sidecar.Id{callerSidecarId, targetSidecarId})
	if err != nil {
		return nil, fmt.Errorf("%d The network's state is not available.", http.StatusServiceUnavailable)
	}
	callerRow, targetRow := rows[callerSidecarId], rows[targetSidecarId]
	if !sidecarWakeMemberDevice(callerRow) || !sidecarWakeMemberDevice(targetRow) || string(callerRow.GetMemberId()) != string(targetRow.GetMemberId()) {
		// one answer for an unknown sidecar, another member's and one that does not stand, so nothing is told
		return nil, fmt.Errorf("%d The sidecar is not another admitted device of the caller's member.", http.StatusForbidden)
	}
	clientId, found := model.GetSidecarCurrentClientId(ctx, byJwt.NetworkId, args.SidecarId)
	if !found {
		return nil, fmt.Errorf("%d The sidecar is not another admitted device of the caller's member.", http.StatusForbidden)
	}
	taken, err := model.TakeSidecarWake(ctx, args.SidecarId, sidecarWakeSettings.Interval)
	if err != nil {
		return nil, fmt.Errorf("%d The wake limit is not available.", http.StatusServiceUnavailable)
	}
	if !taken {
		return nil, fmt.Errorf("%d The sidecar was summoned within the hour.", http.StatusTooManyRequests)
	}
	hint := &sidecar.ClientHint{
		Topic:   sidecar.PushTopicCore,
		Version: uint64(server.NowUtc().UnixMicro()),
		Forced:  true,
	}
	if err := sidecarWakeHint(ctx, []server.Id{clientId}, hint); err != nil {
		return nil, fmt.Errorf("%d The wake was not written.", http.StatusServiceUnavailable)
	}
	return &SidecarWakeResult{}, nil
}
