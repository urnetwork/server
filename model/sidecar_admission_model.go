package model

// §7.9's admissions at api (TEAM-M2-16; R-TEAM-5, D-TEAM-5, §20.4, §20.7):
//
//	cap        api's verifier counts the admissions standing at the tip and
//	           refuses a statement that would take one more past the
//	           network's config_admissions, member devices and agents alike,
//	           with no member-device cap, answering QuotaRefusal (429, or 402
//	           when a higher tier lifts it). Dark behind
//	           enforce_sidecar_limits, with no I/O while off (§20.5). Readers
//	           know no tier, so this is api's alone, as the team tier of a
//	           directory RepoCreate is
//	enroll     a pre-approved key's POST /network/enroll may carry
//	           `admission`: the device's own SidecarAdmit, signed by the
//	           Delegation whose seeds its key's #<bundle> held (§7.8, C-03).
//	           api checks its shape against the key (this network, the key's
//	           ephemeral flag, a Delegation's signature), checks the bearer
//	           without spending it, then appends it as any statement, verified
//	           at the tip under the cap, before the mint, so a mint that fails
//	           leaves an admission its retry finds again by its id. Whether the
//	           Delegation may admit it is the log's rule alone (the scoped row)
//
// A count is the verifier's, never a request's: what the log holds at the tip
// api appends at, so two api instances racing an append each count the other's
// admission once its entry lands at the position they lost (§7.9's 412 row).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// §20.4's key for the cap.
const ConfigAdmissionsLimitKey = "config_admissions"

// The admissions standing in state at timeMs, which config_admissions bounds:
// every row neither revoked nor past its own not_after, member devices and
// agents alike, parked ones included, since a park is no removal. A revoked
// row stays a tombstone until its not_after and counts for nothing.
func ConfigAdmissionCount(state *sidecar.ConfigState, timeMs int64) int64 {
	count := int64(0)
	for _, sidecarId := range state.SidecarIds() {
		row := state.SidecarRow(sidecarId)
		if row == nil || row.GetRevokedAt() != 0 {
			continue
		}
		if row.GetNotAfter() != 0 && row.GetNotAfter() <= timeMs {
			continue
		}
		count += 1
	}
	return count
}

// Whether a statement whose verdict at state is verdict takes an admission
// where it lands: a SidecarAdmit, or the AuthorityProposal or cosign that
// completes a proposal of one (§7.9's completion row). A refused statement
// takes none. An ApiTick's activation of a proposed SidecarAdmit takes one
// too, where the tick lands, which CheckConfigActivationCap counts.
func ConfigStatementAdmits(state *sidecar.ConfigState, signedBytes []byte, verdict sidecar.ConfigVerdict) bool {
	if !verdict.Accepted() {
		return false
	}
	if verdict.SignedType == bridge.SignedType_SIGNED_SIDECAR_ADMIT {
		return true
	}
	if len(verdict.CompletedProposalId) == 0 {
		return false
	}
	switch verdict.SignedType {
	case bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL:
		signed := &bridge.Signed{}
		proposal := &protocol.AuthorityProposal{}
		if proto.Unmarshal(signedBytes, signed) != nil || proto.Unmarshal(signed.GetBody(), proposal) != nil {
			// a statement that does not parse is refused before this; count it
			// as one, which errs toward the cap
			return true
		}
		return bridge.SignedType(proposal.GetStatementType()) == bridge.SignedType_SIGNED_SIDECAR_ADMIT
	case bridge.SignedType_SIGNED_AUTHORITY_COSIGN:
		for _, proposal := range state.Rows().GetProposals() {
			if bytes.Equal(proposal.GetProposalId(), verdict.CompletedProposalId) {
				return bridge.SignedType(proposal.GetStatementType()) == bridge.SignedType_SIGNED_SIDECAR_ADMIT
			}
		}
		// a completion whose proposal the tip does not show: count it
		return true
	}
	return false
}

// The refusal for usedCount admissions and one more in networkId, or nil when
// the network has room. Nil with no I/O while sidecar limits are dark.
func ConfigAdmissionsExceeded(ctx context.Context, networkId server.Id, usedCount int64) *sidecar.QuotaRefusal {
	if !Pro().EnforceSidecarLimits {
		return nil
	}
	tier, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
	limit := limits.Limit(ConfigAdmissionsLimitKey)
	if limit == proSidecarLimitNone || usedCount < limit {
		return nil
	}
	return &sidecar.QuotaRefusal{
		LimitKey: ConfigAdmissionsLimitKey,
		Limit:    limit,
		Used:     usedCount,
		Upgrade:  quotaRefusalUpgrade(tier, ConfigAdmissionsLimitKey, usedCount, networkSidecarLimitHasRow(ctx, nil, networkId, ConfigAdmissionsLimitKey)),
	}
}

// The sequencer's admission cap (ConfigLogSettings.AdmissionCap): a statement
// that takes an admission at state, the tip api appends at, is refused past
// config_admissions with a *QuotaRefusalError, which the route answers as the
// QuotaRefusal itself. Anything else passes.
func CheckConfigAdmissionCap(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, signedBytes []byte, verdict sidecar.ConfigVerdict, timeMs int64) error {
	if !Pro().EnforceSidecarLimits || !ConfigStatementAdmits(state, signedBytes, verdict) {
		return nil
	}
	if refusal := ConfigAdmissionsExceeded(ctx, networkId, ConfigAdmissionCount(state, timeMs)); refusal != nil {
		return NewQuotaRefusalError(refusal)
	}
	return nil
}

// How many admissions a tick's activations take at state: its activated
// proposals of a SidecarAdmit, as the tip's rows hold them. One the tip does
// not show counts as one, which errs toward the cap.
func ConfigTickAdmissionCount(state *sidecar.ConfigState, tick *bridge.ApiTick) int64 {
	pending := state.PendingProposals()
	count := int64(0)
	for _, proposalId := range tick.GetActivatedProposalIds() {
		admits := true
		for _, proposal := range pending {
			if bytes.Equal(proposal.GetProposalId(), proposalId) {
				admits = bridge.SignedType(proposal.GetStatementType()) == bridge.SignedType_SIGNED_SIDECAR_ADMIT
				break
			}
		}
		if admits {
			count += 1
		}
	}
	return count
}

// The sequencer's cap on a tick's activations (ConfigLogSettings.ActivationCap):
// a tick whose activations admit at state, the tip api appends at, is refused
// when they would stand past config_admissions, with a *QuotaRefusalError.
// The proposal stays open, and its task looks again (config_log_tick.go).
func CheckConfigActivationCap(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, tick *bridge.ApiTick, timeMs int64) error {
	if !Pro().EnforceSidecarLimits {
		return nil
	}
	admitCount := ConfigTickAdmissionCount(state, tick)
	if admitCount == 0 {
		return nil
	}
	if refusal := ConfigAdmissionsExceeded(ctx, networkId, ConfigAdmissionCount(state, timeMs)+admitCount-1); refusal != nil {
		return NewQuotaRefusalError(refusal)
	}
	return nil
}

// The code of an enrollment whose admission the log refuses at the tip.
const NetworkEnrollCodeAdmissionRefused = "admission_refused"

// A pre-approved key's admission as an enrollment carries it.
type networkEnrollAdmission struct {
	// the Signed SidecarAdmit, verbatim
	signedBytes []byte
	admit       *protocol.SidecarAdmit
	// the Delegation that signed it, its 32-byte id
	delegationId []byte
}

// Parses an enrollment's admission: a Signed SidecarAdmit that a Delegation's
// key signed (§6.9's Signers: a 32-byte signer_id and a 32-byte key_id). Only
// its shape: the signature and the Delegation's right to admit are the log's.
func parseNetworkEnrollAdmission(signedBytes []byte) (*networkEnrollAdmission, error) {
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		return nil, fmt.Errorf("an admission that is no Signed: %w", err)
	}
	if bridge.SignedType(signed.GetType()) != bridge.SignedType_SIGNED_SIDECAR_ADMIT {
		return nil, fmt.Errorf("an admission of type %d", signed.GetType())
	}
	if len(signed.GetSignerId()) != sha256.Size || len(signed.GetKeyId()) != sha256.Size {
		return nil, errors.New("an admission no Delegation signed")
	}
	message, err := sidecar.ParseConfigStatement(bridge.SignedPurposeAuthority, bridge.SignedType_SIGNED_SIDECAR_ADMIT, signed.GetBody())
	if err != nil {
		return nil, err
	}
	admit, ok := message.(*protocol.SidecarAdmit)
	if !ok {
		return nil, errors.New("an admission that is no SidecarAdmit")
	}
	return &networkEnrollAdmission{
		signedBytes:  bytes.Clone(signedBytes),
		admit:        admit,
		delegationId: bytes.Clone(signed.GetSignerId()),
	}, nil
}

// Whether the admission fits the key it rides with: the key's network, the
// key pre-approved (only such a key's bundle holds a Delegation's seeds), and
// the key's ephemeral flag, since the mint's lease (§7.7) and the reap's tick
// (§7.9) both follow it.
func (self *networkEnrollAdmission) fits(key *networkEnrollKey) error {
	if !bytes.Equal(self.admit.GetNetworkId(), sidecar.Id(key.networkId).Bytes()) {
		return errors.New("an admission into another network")
	}
	if !key.preApproved {
		return errors.New("an admission with a key that is not pre-approved")
	}
	if self.admit.GetEphemeral() != key.ephemeral {
		return errors.New("an admission whose ephemeral flag is not its key's")
	}
	return nil
}

// Appends an enrollment's admission to the network's log through this
// process's sequencer, verified at the tip under the cap. A test replaces it.
var networkEnrollAppendAdmission = func(ctx context.Context, networkId server.Id, signedBytes []byte) (*bridge.AuthPin, error) {
	return AppendConfigStatement(ctx, networkId, signedBytes)
}

// Replaces the admission's append, returning a restore function.
func Testing_SetNetworkEnrollAppendAdmission(appendAdmission func(ctx context.Context, networkId server.Id, signedBytes []byte) (*bridge.AuthPin, error)) func() {
	previous := networkEnrollAppendAdmission
	networkEnrollAppendAdmission = appendAdmission
	return func() {
		networkEnrollAppendAdmission = previous
	}
}

// Lands an enrollment's admission: the bearer checked without spending a use,
// then the append. Every refusal is a *NetworkEnrollRefusal or a
// *QuotaRefusalError holding no request value.
func landNetworkEnrollAdmission(ctx context.Context, bearer string, key *networkEnrollKey, admission *networkEnrollAdmission) (*bridge.AuthPin, error) {
	if err := admission.fits(key); err != nil {
		return nil, newNetworkEnrollRefusal(http.StatusBadRequest, NetworkEnrollCodeBadRequest, "The admission does not fit the auth key.")
	}
	// the bearer is the gate on who may post the Delegation's admission here,
	// so an exhausted, expired or revoked one posts nothing
	if _, err := PeekAuthKey(ctx, bearer); err != nil {
		return nil, newNetworkEnrollRefusal(http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid, networkEnrollAuthKeyInvalidMessage)
	}
	pin, err := networkEnrollAppendAdmission(ctx, key.networkId, admission.signedBytes)
	if err == nil {
		return pin, nil
	}
	quotaRefusal := &QuotaRefusalError{}
	if errors.As(err, &quotaRefusal) {
		return nil, quotaRefusal
	}
	configLogError := &ConfigLogError{}
	if errors.As(err, &configLogError) {
		switch configLogError.Status {
		case http.StatusForbidden, http.StatusConflict:
			return nil, newNetworkEnrollRefusal(http.StatusForbidden, NetworkEnrollCodeAdmissionRefused, "The network's log refuses the admission.")
		case http.StatusTooManyRequests:
			return nil, newNetworkEnrollRefusal(http.StatusTooManyRequests, NetworkEnrollCodeRefused, "The network's log is at its append limit.")
		}
	}
	limitError := &ConfigLogLimitError{}
	if errors.As(err, &limitError) {
		return nil, &NetworkEnrollRefusal{
			Status:     http.StatusTooManyRequests,
			Code:       NetworkEnrollCodeRefused,
			Message:    "The network's log is at its append limit.",
			RetryAfter: limitError.RetryAfterSeconds(),
		}
	}
	return nil, newNetworkEnrollRefusal(http.StatusServiceUnavailable, NetworkEnrollCodeUnavailable, "The network's log is unavailable.")
}
