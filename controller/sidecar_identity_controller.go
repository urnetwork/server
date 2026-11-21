package controller

// §5.15's routes (§5.5, §5.6, X10): POST /sidecar/bind appends a client
// generation's dual-signed binding to its sidecar's never-reaped log, and GET
// /sidecar/identity answers a sidecar's identity and bindings, by sidecar_id
// or by a client of any generation (plan-CORE-client-to-sidecar-lookup). Both
// are Client class on the strict_exp allowlist (api/route_authz.go).
//
// Bind takes no DPoP proof, so what keeps a copied token from binding a key of
// its own is the binding itself: both signatures verify over the received
// bytes (sidecar.VerifySidecarIdentityBinding), the bearer must be the
// binding's client in the binding's network, and a client binds exactly once
// (§5.7, plan-CORE-binding-wire #2). A refusal answers {"error", "code"} with
// sidecar_revoked, binding_invalid, generation_conflict or client_bound
// (plan-CORE-binding-wire #6), and never repeats the binding.
//
// The lookups never return a user_id, and serve the caller's own network or
// one an active §8.11 share binds to it (CORE-M6-04; §5.15's caller rule,
// model.GetSidecarIdentity): any other network's identity is 404, as though
// api held none. What they answer is untrusted: a verifier checks each binding
// against its own pin, a share link's against the host key its own log pinned
// (§17.9's target row).

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const sidecarIdentityUnavailableMessage = "Sidecar identities are not available yet."

// The codes of §5.15's refusals (plan-CORE-binding-wire #6), as the sidecar's
// SidecarApi reads them.
const (
	sidecarBindCodeRevoked            = "sidecar_revoked"
	sidecarBindCodeBindingInvalid     = "binding_invalid"
	sidecarBindCodeGenerationConflict = "generation_conflict"
	sidecarBindCodeClientBound        = "client_bound"
)

// A refusal with its route code: RaiseHttpError answers its status with
// {"error": message, "code": code} as the body.
type sidecarIdentityRefusal struct {
	status  int
	message string
	code    string
}

func (self *sidecarIdentityRefusal) Error() string {
	return fmt.Sprintf("%d %s", self.status, self.message)
}

// The coded body, the server's existing refusal shape.
func (self *sidecarIdentityRefusal) HttpErrorResultBody() any {
	return map[string]string{
		"error": self.message,
		"code":  self.code,
	}
}

// POST /sidecar/bind's body: the marshalled SignedSidecarIdentityBinding,
// which JSON carries in standard base64.
type SidecarBindArgs struct {
	Binding []byte `json:"binding"`
}

// POST /sidecar/bind's answer: the generation api recorded for the client.
// The bearer is answered nothing new: sockets reads a sidecar's subject from
// the service token's ur_sidecar_id, which the exchange names from this
// binding (QD-1029).
type SidecarBindResult struct {
	Generation int64 `json:"generation"`
}

// POST /sidecar/bind (§5.5, §5.6): verifies the binding and appends it under
// plan-CORE-binding-wire's rules. A byte-identical retry answers the stored
// generation and writes nothing.
func SidecarBind(bind *SidecarBindArgs, clientSession *session.ClientSession) (*SidecarBindResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, sidecarIdentityUnavailableMessage)
	}
	invalid := func(message string) (*SidecarBindResult, error) {
		return nil, &sidecarIdentityRefusal{status: http.StatusBadRequest, message: message, code: sidecarBindCodeBindingInvalid}
	}
	if clientSession.ByJwt.ClientId == nil {
		return invalid("A binding is posted by the client it binds.")
	}
	if bind == nil {
		return invalid("The binding is invalid.")
	}
	binding, err := sidecar.VerifySidecarIdentityBinding(bind.Binding)
	if err != nil {
		return invalid("The binding is invalid.")
	}
	clientId := server.Id(binding.ClientId)
	networkId := server.Id(binding.NetworkId)
	if clientId != *clientSession.ByJwt.ClientId || networkId != clientSession.ByJwt.NetworkId {
		return invalid("The binding names another client or network than the token's.")
	}
	outcome, generation := model.BindSidecarIdentity(
		clientSession.Ctx,
		&model.SidecarBindingRecord{
			NetworkId:         networkId,
			SidecarId:         server.Id(binding.SidecarId),
			ClientId:          clientId,
			Generation:        binding.Generation,
			ClientPublicKey:   binding.ClientPublicKey,
			IdentityPublicKey: binding.IdentityPublicKey,
			SignedBinding:     bind.Binding,
		},
		server.NowUtc(),
	)
	switch outcome {
	case model.SidecarBindAppended, model.SidecarBindRetried:
		return &SidecarBindResult{Generation: generation}, nil
	case model.SidecarBindSidecarRevoked:
		return nil, &sidecarIdentityRefusal{status: http.StatusForbidden, message: "The sidecar is revoked.", code: sidecarBindCodeRevoked}
	case model.SidecarBindClientBound:
		return nil, &sidecarIdentityRefusal{status: http.StatusConflict, message: "The client is bound already.", code: sidecarBindCodeClientBound}
	case model.SidecarBindGenerationConflict:
		return nil, &sidecarIdentityRefusal{status: http.StatusConflict, message: "The binding is not the next generation of its identity.", code: sidecarBindCodeGenerationConflict}
	}
	return nil, errors.New("unknown bind outcome")
}

// The lookup GET /sidecar/identity takes: a sidecar_id with a page of its
// bindings, or a client of any generation.
type GetSidecarIdentityArgs struct {
	BrowserAdmission *string
	SidecarId        *server.Id
	// 0: the newest page; else the page below this generation.
	BeforeGeneration int64
	ClientId         *server.Id
}

// GET /sidecar/identity's answer (§5.15): never a user_id.
type GetSidecarIdentityResult struct {
	BrowserAdmission  string    `json:"browser_admission,omitempty"`
	SidecarId         server.Id `json:"sidecar_id"`
	IdentityPublicKey []byte    `json:"identity_public_key"`
	IdentityAlg       string    `json:"identity_alg"`
	Status            string    `json:"status"`
	// The current binding: the newest generation's, or the asked client's.
	Binding []byte `json:"binding"`
	// By sidecar_id: a page of bindings, newest first, at most 64.
	Bindings [][]byte `json:"bindings,omitempty"`
}

// GET /sidecar/identity/<sidecar_id> and GET /sidecar/identity?client_id=<id>.
// An identity of neither the caller's network nor one an active §8.11 share
// binds to it is 404, the same answer as one api does not hold.
func GetSidecarIdentity(lookup *GetSidecarIdentityArgs, clientSession *session.ClientSession) (*GetSidecarIdentityResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, sidecarIdentityUnavailableMessage)
	}
	notFound := fmt.Errorf("%d No such sidecar identity.", http.StatusNotFound)
	var view *model.SidecarIdentityView
	switch {
	case lookup.BrowserAdmission != nil:
		mode := *lookup.BrowserAdmission
		jwt := clientSession.ByJwt
		if lookup.SidecarId == nil || lookup.ClientId != nil || lookup.BeforeGeneration != 0 || jwt == nil || jwt.ClientId == nil ||
			(mode != sidecar.BrowserAdmissionPage && mode != sidecar.BrowserAdmissionBurner) {
			return nil, notFound
		}
		member := model.ResolveNetworkSignIn(clientSession.Ctx, jwt.UserId, jwt.NetworkId, server.NowUtc())
		if member == nil || (member.MemberId == nil) != (jwt.MemberId == nil) || member.MemberId != nil && *member.MemberId != *jwt.MemberId {
			return nil, notFound
		}
		view = model.GetOwnBrowserAdmissionIdentity(clientSession.Ctx, member, *jwt.ClientId, *lookup.SidecarId, mode == sidecar.BrowserAdmissionBurner)
	case lookup.SidecarId != nil && lookup.ClientId == nil:
		if lookup.BeforeGeneration < 0 {
			return nil, fmt.Errorf("%d Invalid generation.", http.StatusBadRequest)
		}
		view = model.GetSidecarIdentity(clientSession.Ctx, clientSession.ByJwt.NetworkId, *lookup.SidecarId, lookup.BeforeGeneration)
	case lookup.ClientId != nil && lookup.SidecarId == nil:
		view = model.GetSidecarIdentityByClient(clientSession.Ctx, clientSession.ByJwt.NetworkId, *lookup.ClientId)
	default:
		return nil, fmt.Errorf("%d Name a sidecar_id or a client_id.", http.StatusBadRequest)
	}
	if view == nil {
		return nil, notFound
	}
	result := &GetSidecarIdentityResult{
		SidecarId:         view.SidecarId,
		IdentityPublicKey: view.IdentityPublicKey,
		IdentityAlg:       view.IdentityAlg,
		Status:            view.Status,
		Binding:           view.Binding,
	}
	if lookup.BrowserAdmission != nil {
		result.BrowserAdmission = *lookup.BrowserAdmission
	}
	if lookup.SidecarId != nil {
		result.Bindings = view.Bindings
	}
	return result, nil
}
