package server

// The route a Team direct MX's probe outcome reaches api by (QD-1103, owner decision 639; MAIL-M7-23; §15.4's Team
// paragraph, §12.8's direct MX, §19.4's mxedge row; D-OPS-1). mxedge's router probes each direct-MX receiver from the
// edge, since owner decision 33's check of public TCP 25, STARTTLS and the receiver's mail_host_key SPKI has to run
// there (mxedgeservice.MxDirectMxProber), and posts each outcome on MailDirectMxProbeGatePath through the block's
// PlatformClientSource, on the `edge` class's bearer its platform clients are minted on (QD-812):
//
//	POST /internal/gate/mail-direct-mx-probe  edge  {network_id, receiver_sidecar_id, address, passed}
//	  -> {recorded}
//
// api's handler (api/handlers/internal_gate_mail_direct_mx_probe_handlers.go) takes only that class. It checks that
// the direct-MX row still stands and records the outcome as the prober once did from mxedge
// (model.RecordMailDirectMxProbe): the row's probe state in mail_direct_mx and, on a pass, §12.8's records at
// mx.<net>.<root> through the zone's writer in api's own dns root, in one transaction, then the network's {mxr_}
// routes. So mxedge's restricted Postgres role (QD-992, pg-mxedge.yml) reads only what the router routes and probes
// by and inserts only its connection records, and the zone's writers stay api and the dns block.
//
// recorded false is a row gone in between, a DELETE or a receiver's removal that came first: a no-op success, as it
// was when the prober wrote. A network api has no row of is refused 404, and every other refusal (403 for another
// class's bearer, 400 for a body the format does not take, 409 for a zone that refuses the records now, 503 for a
// process that cannot record now) leaves the row as it was, for the prober's next pass.
//
// The format has no limit of its own beyond the request's and the answer's sizes, which each end's settings hold.
// Nothing here logs a bearer, a network, a receiver or an address (I-7, I-13).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
)

// The route, under the internal gate's prefix.
const MailDirectMxProbeGatePath = InternalGatePathPrefix + "mail-direct-mx-probe"

// What the prober posts for one probe of one direct-MX row: the row's key and the probe's outcome, nothing more.
type MailDirectMxProbeGateArgs struct {
	NetworkId         Id `json:"network_id"`
	ReceiverSidecarId Id `json:"receiver_sidecar_id"`
	// The row's address as the prober probed it, IPv4 or IPv6, in its canonical text.
	Address string `json:"address"`
	// Whether the probe passed: TCP 25's 220, STARTTLS, and a handshake presenting a published mail_host_key's SPKI.
	Passed bool `json:"passed"`
}

// What api answers.
type MailDirectMxProbeGateResult struct {
	// The row stood and the outcome is recorded. false: the row is gone, and nothing was written.
	Recorded bool `json:"recorded"`
}

// A request the format does not take. The text names the part, never its value.
var ErrMailDirectMxProbeGateArgsInvalid = errors.New("mail direct mx probe gate: request invalid")

// A call api did not answer as recorded or gone: api away, a refusal, an answer that does not read. The text names
// the part or the status, never the bearer or the request.
var ErrMailDirectMxProbeGate = errors.New("mail direct mx probe gate")

// Checks the request against the format: a network and a receiver, and an address in its one canonical text, with no
// zone and not IPv4 mapped into IPv6, as mail_direct_mx holds it. Answers the address.
func (self *MailDirectMxProbeGateArgs) Validate() (netip.Addr, error) {
	switch {
	case self == nil:
		return netip.Addr{}, fmt.Errorf("%w: no request", ErrMailDirectMxProbeGateArgsInvalid)
	case self.NetworkId == (Id{}):
		return netip.Addr{}, fmt.Errorf("%w: the network", ErrMailDirectMxProbeGateArgsInvalid)
	case self.ReceiverSidecarId == (Id{}):
		return netip.Addr{}, fmt.Errorf("%w: the receiver", ErrMailDirectMxProbeGateArgsInvalid)
	}
	address, err := netip.ParseAddr(self.Address)
	if err != nil || address.Zone() != "" || address.Is4In6() || address.String() != self.Address {
		return netip.Addr{}, fmt.Errorf("%w: the address", ErrMailDirectMxProbeGateArgsInvalid)
	}
	return address, nil
}

// Posts one direct-MX probe's outcome to api on MailDirectMxProbeGatePath with the block's edge bearer, over the
// source's own HTTP client, the call bounded by its RequestTimeout on its clock and the answer by its
// AnswerMaxByteCount, as its mints are. It needs none of the source's clients: the bearer is the caller. An error is
// a request the format does not take, a closed source, or a call api did not answer 200 (ErrMailDirectMxProbeGate,
// naming the part or the status): the outcome is not recorded, and the prober tries again at its next pass.
func (self *PlatformClientSource) RecordMailDirectMxProbe(ctx context.Context, args *MailDirectMxProbeGateArgs) (*MailDirectMxProbeGateResult, error) {
	if self == nil {
		return nil, fmt.Errorf("%w: no source", ErrMailDirectMxProbeGate)
	}
	if _, err := args.Validate(); err != nil {
		return nil, err
	}
	if self.ctx.Err() != nil {
		return nil, fmt.Errorf("%w: closed", ErrMailDirectMxProbeGate)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("%w: the request could not be made", ErrMailDirectMxProbeGate)
	}
	askCtx, done := self.askContext(ctx)
	defer done()
	request, err := http.NewRequestWithContext(askCtx, http.MethodPost, self.args.ApiUrl+MailDirectMxProbeGatePath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: the request could not be made", ErrMailDirectMxProbeGate)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+self.args.Bearer)
	response, err := self.httpClient.Do(request)
	if err != nil {
		// a transport error can quote the url, which holds no secret, but nothing of the request's headers or body
		return nil, fmt.Errorf("%w: api did not answer", ErrMailDirectMxProbeGate)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: api answered %d", ErrMailDirectMxProbeGate, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, self.settings.AnswerMaxByteCount+1))
	if err != nil || self.settings.AnswerMaxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("%w: api's answer could not be read", ErrMailDirectMxProbeGate)
	}
	result := &MailDirectMxProbeGateResult{}
	if err := json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("%w: api's answer is not a probe's", ErrMailDirectMxProbeGate)
	}
	return result, nil
}
