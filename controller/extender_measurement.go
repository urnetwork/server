package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// lag.dev measurement certificates, the operator's half (lag.dev/PLAN.md C,
// A8-A10).
//
// An open tier extender with a public inbound address has one measurement
// name, its id under the measurement zone (N1), and a certificate for exactly
// that name, so a browser can reach its measurement API over tls (M). The name
// resolves to the extender's active addresses of both families (N3), so an
// address change moves records and never a certificate. The extender makes
// its own key and sends a certificate request with an activation (C4); this
// file reads the request, decides which extenders may be measured and at
// which addresses, and answers the extender's polls (A8). The issuance,
// renewal, revocation and the zone itself are the taskworker's
// (taskworker/work/extender_measurement*.go).
//
// What depends on the shape of a name is in three places, so a change of
// shape is a change of those: the two name functions here, which are the only
// callers of connect's; the request validation,
// ParseExtenderMeasurementCsrDer; and the record set,
// GetExtenderMeasurementHostsToPublish.
//
// Whether an extender may be measured is one rule everywhere,
// ExtenderMeasurementEligibleAddresses: an active extender in the open tier
// that is no canary (§2, decision 1), whose tcp carrier is on 443 where
// browsers connect (decision 4), at each active address that serves tcp
// (decision 14).

const (
	// The port a browser reaches the measurement api on: the extender's
	// existing tls port (decision 4, M1).
	ExtenderMeasurementPort = 443

	// The longest certificate request accepted, in base64 characters. A P-256
	// request for one name is about 300 bytes of DER.
	ExtenderMeasurementCsrMaxLength = 4096

	// Why an extender is withdrawn when its owner opts out (C4, C9).
	ExtenderMeasurementOptOutReason = "the extender's owner opted out of measurement"
)

// ExtenderMeasurementName is an extender's measurement name (N1): its id
// under the zone. The zero id is no extender's and has no name, "": nothing
// is issued, published or signed for it.
func ExtenderMeasurementName(extenderId server.Id, zone string) string {
	return connect.ExtenderMeasurementName(connect.Id(extenderId), zone)
}

// ParseExtenderMeasurementName is the extender a measurement name is for, the
// inverse of ExtenderMeasurementName. Only the canonical spelling parses.
func ParseExtenderMeasurementName(name string, zone string) (server.Id, bool) {
	extenderId, ok := connect.ParseExtenderMeasurementName(name, zone)
	return server.Id(extenderId), ok
}

// One certificate request as an activation carried it (C4), parsed.
type ExtenderMeasurementRequest struct {
	// DER
	Csr []byte
	// the request's SubjectPublicKeyInfo, DER
	PublicKey []byte
	// the one name it asks for, the extender's own
	Name string
}

// ParseExtenderMeasurementCsr reads a certificate request as an activation
// carries it: standard base64 of the DER (C4). The unpadded and url forms
// are read too.
func ParseExtenderMeasurementCsr(csrBase64 string, extenderId server.Id, zone string) (*ExtenderMeasurementRequest, error) {
	csr, err := decodeExtenderMeasurementCsr(csrBase64)
	if err != nil {
		return nil, err
	}
	return ParseExtenderMeasurementCsrDer(csr, extenderId, zone)
}

// The DER of a certificate request as an activation carries it.
func decodeExtenderMeasurementCsr(csrBase64 string) ([]byte, error) {
	csrBase64 = strings.TrimSpace(csrBase64)
	if csrBase64 == "" {
		return nil, fmt.Errorf("the measurement certificate request is empty")
	}
	if ExtenderMeasurementCsrMaxLength < len(csrBase64) {
		return nil, fmt.Errorf("the measurement certificate request is longer than %d characters", ExtenderMeasurementCsrMaxLength)
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if csr, err := encoding.DecodeString(csrBase64); err == nil {
			return csr, nil
		}
	}
	return nil, fmt.Errorf("the measurement certificate request is not base64")
}

// What can be read of a certificate request that is refused: its DER, its key
// and the names it asks for, each nil when the request cannot be read that
// far. A refusal keeps them, so A8 names the refused request's key
// (key_sha256); a request that cannot be parsed has no key to name.
func readRefusedExtenderMeasurementCsr(csrBase64 string) (csr []byte, publicKey []byte, names []string) {
	csr, err := decodeExtenderMeasurementCsr(csrBase64)
	if err != nil {
		return nil, nil, nil
	}
	request, err := x509.ParseCertificateRequest(csr)
	if err != nil {
		return nil, nil, nil
	}
	return csr, request.RawSubjectPublicKeyInfo, request.DNSNames
}

// ParseExtenderMeasurementCsrDer validates an extender's certificate request
// (C5): its signature verifies, its key is ECDSA P-256, and its only name is
// that extender's measurement name, in its one canonical spelling (N1) -- no
// second name, no address, email or uri name, and a common name only if it is
// that same name. A request for another extender's name is refused like any
// other wrong name: the name is the extender's id, and an extender gets a
// certificate for no id but its own.
func ParseExtenderMeasurementCsrDer(csr []byte, extenderId server.Id, zone string) (*ExtenderMeasurementRequest, error) {
	request, err := x509.ParseCertificateRequest(csr)
	if err != nil {
		return nil, fmt.Errorf("the measurement certificate request is not readable: %w", err)
	}
	if err := request.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the measurement certificate request signature does not verify: %w", err)
	}
	publicKey, ok := request.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return nil, fmt.Errorf("the measurement certificate request key must be ecdsa p-256")
	}
	if 0 < len(request.IPAddresses) || 0 < len(request.EmailAddresses) || 0 < len(request.URIs) {
		return nil, fmt.Errorf("the measurement certificate request may name the extender's measurement name only")
	}
	name := ExtenderMeasurementName(extenderId, zone)
	if name == "" {
		return nil, fmt.Errorf("the extender has no measurement name")
	}
	if len(request.DNSNames) != 1 || request.DNSNames[0] != name {
		return nil, fmt.Errorf(
			"the measurement certificate request names %v, not only the extender's measurement name %s",
			request.DNSNames,
			name,
		)
	}
	if commonName := request.Subject.CommonName; commonName != "" && commonName != name {
		return nil, fmt.Errorf("the measurement certificate request common name %q is not the extender's measurement name", commonName)
	}
	return &ExtenderMeasurementRequest{
		Csr:       csr,
		PublicKey: request.RawSubjectPublicKeyInfo,
		Name:      name,
	}, nil
}

// ExtenderMeasurementEligibleAddresses is the addresses an extender is
// measured at, IPv4 first, or why it may not be measured (C5, C8): the
// extender is active, in the open tier and no canary, serves tcp on the
// measurement port, and has at least one active address that serves tcp. A
// webrtc-only extender has no public inbound address and is not measured
// (decision 14).
func ExtenderMeasurementEligibleAddresses(
	extender *model.NetworkExtender,
	addresses []*model.NetworkExtenderAddress,
) (ips []netip.Addr, reason string) {
	switch {
	case extender == nil:
		return nil, "the extender is gone"
	case !extender.Active:
		return nil, "the extender is not active"
	case extender.DirectoryTier != connect.ExtenderDirectoryTierOpen:
		return nil, "an extender in the gated tier is never measured"
	case extender.CanaryChannel != "":
		return nil, "a canary is never measured"
	case extender.TcpPort != ExtenderMeasurementPort:
		return nil, fmt.Sprintf("the extender serves tcp on %d, not %d", extender.TcpPort, ExtenderMeasurementPort)
	}
	ips = []netip.Addr{}
	for _, address := range addresses {
		if !address.Active || !slices.Contains(address.Carriers, connect.ExtenderCarrierTcp) {
			continue
		}
		ip := address.Ip.Unmap().WithZone("")
		if !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil, "the extender has no public inbound address"
	}
	slices.SortFunc(ips, func(a netip.Addr, b netip.Addr) int {
		if a.Is4() != b.Is4() {
			if a.Is4() {
				return -1
			}
			return 1
		}
		return a.Compare(b)
	})
	return ips, ""
}

// ExtenderMeasurementWithdrawReason is why an extender's live certificates
// must be withdrawn (C9), empty while they may stay: the extender was
// deactivated, or it is in the gated tier or a canary, which an operator's
// override made it (A9). An address change withdraws nothing: the name is the
// extender's id, and the reconcile moves its records (C8).
func ExtenderMeasurementWithdrawReason(extender *model.NetworkExtender) string {
	switch {
	case extender == nil:
		return "the extender is gone"
	case !extender.Active:
		return "the extender is not active"
	case extender.DirectoryTier != connect.ExtenderDirectoryTierOpen:
		return "the extender moved to the gated tier"
	case extender.CanaryChannel != "":
		return "the extender became a canary"
	}
	return ""
}

// One extender that may serve the measurement api now (C8, A4): its name, the
// addresses the name resolves to, and since when it does.
type ExtenderMeasurementHost struct {
	ExtenderId server.Id
	// the extender's ed25519 identity key
	PublicKey []byte
	// the extender's measurement name (N1)
	Name string
	// its active public inbound addresses, IPv4 first (N3)
	Ips []netip.Addr
	// when the dns reconcile found the name published, in sync on every
	// authoritative server; nil while it is not yet (C8)
	PublishTime *time.Time
}

// GetExtenderMeasurementHostsToPublish reads every extender that may serve
// the measurement api at `now` (C8): one that may be measured and holds a
// live, already valid certificate for its measurement name, with the
// addresses it is measured at, whether or not its name is published yet. The
// dns reconcile publishes exactly these -- each name's A records its IPv4
// addresses and its AAAA records its IPv6 addresses. Ordered by name.
func GetExtenderMeasurementHostsToPublish(ctx context.Context, config *ExtenderConfig, now time.Time) []*ExtenderMeasurementHost {
	zone := config.Measurement.MeasurementZone()
	nameHosts := map[string]*ExtenderMeasurementHost{}
	for _, target := range model.GetLiveNetworkExtenderMeasurementCerts(ctx, now) {
		if now.Before(target.Cert.NotBefore) {
			continue
		}
		ips, reason := ExtenderMeasurementEligibleAddresses(target.Extender, target.Addresses)
		if reason != "" {
			continue
		}
		name := ExtenderMeasurementName(target.Extender.ExtenderId, zone)
		if name == "" || !slices.Contains(target.Cert.Names, name) {
			// a certificate for another zone's name
			continue
		}
		// a renewal overlaps the certificate it replaces; either serves, and
		// the name is published since the earliest of them says
		host, ok := nameHosts[name]
		if !ok {
			host = &ExtenderMeasurementHost{
				ExtenderId: target.Extender.ExtenderId,
				PublicKey:  target.Extender.PublicKey,
				Name:       name,
				Ips:        ips,
			}
			nameHosts[name] = host
		}
		if publishTime := target.Cert.PublishTime; publishTime != nil && (host.PublishTime == nil || publishTime.Before(*host.PublishTime)) {
			host.PublishTime = publishTime
		}
	}
	hosts := []*ExtenderMeasurementHost{}
	for _, host := range nameHosts {
		hosts = append(hosts, host)
	}
	slices.SortFunc(hosts, func(a *ExtenderMeasurementHost, b *ExtenderMeasurementHost) int {
		return strings.Compare(a.Name, b.Name)
	})
	return hosts
}

// GetExtenderMeasurementHosts reads every extender that serves the
// measurement api at `now`: one the dns reconcile publishes
// (GetExtenderMeasurementHostsToPublish) whose name it has published (C8).
// The lag.dev sample (A4) draws from these, and offers those whose
// certificate A8 has also handed to them (lagStillServing). A name is offered
// only once it resolves on every authoritative server: a resolver asked for
// it before would cache that it does not exist for the zone's negative ttl,
// and the browser that asked could not reach the extender until that ran out
// (C1). An extender whose certificate was just issued is left out until the
// reconcile that runs right after the issuance has published it. Ordered by
// name.
func GetExtenderMeasurementHosts(ctx context.Context, config *ExtenderConfig, now time.Time) []*ExtenderMeasurementHost {
	hosts := []*ExtenderMeasurementHost{}
	for _, host := range GetExtenderMeasurementHostsToPublish(ctx, config, now) {
		if host.PublishTime != nil {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// The id a record signed at issueTime carries as its MeasurementId (C10): the
// extender's own, 16 bytes, when measurement is enabled, the extender may be
// measured at one of the record's addresses, and it holds a live certificate,
// unexpired at issueTime, whose name the dns reconcile has published (C8) and
// which A8 has handed to it for its current key; nil otherwise. A reader
// computes the measurement name from it (N1), so it is never set before the
// name resolves, nor before the extender has the certificate to answer it
// with. The certificate state is read in the signing transaction
// (NetworkExtender.MeasurementCertNotAfter).
func extenderMeasurementId(
	config *ExtenderConfig,
	extender *model.NetworkExtender,
	addresses []*model.NetworkExtenderAddress,
	issueTime time.Time,
) []byte {
	if !config.Measurement.Enabled || extender.ExtenderId == (server.Id{}) {
		// the zero id names no extender, and a reader would compute no name
		// from it
		return nil
	}
	if extender.MeasurementCertNotAfter == nil || !issueTime.Before(*extender.MeasurementCertNotAfter) {
		return nil
	}
	if _, reason := ExtenderMeasurementEligibleAddresses(extender, addresses); reason != "" {
		return nil
	}
	return extender.ExtenderId.Bytes()
}

// Stores the certificate request an activation carried (A10, C4). A request
// that cannot be read, or that names anything but this extender's measurement
// name, is stored refused with the reason, which the extender reads from A8,
// and with the key and names of as much of it as could be read, which A8
// answers with; either way the activation's own answer does not change. An
// activation that carries no request never reaches here, so a stored request
// stands through every activation without one. The issuance task takes a
// pending request on its next pass (C5).
func storeExtenderMeasurementRequest(
	ctx context.Context,
	config *ExtenderConfig,
	extenderId server.Id,
	csrBase64 string,
	now time.Time,
) {
	request, err := ParseExtenderMeasurementCsr(csrBase64, extenderId, config.Measurement.MeasurementZone())
	if err != nil {
		csr, publicKey, names := readRefusedExtenderMeasurementCsr(csrBase64)
		model.RefuseNetworkExtenderMeasurementRequest(ctx, extenderId, csr, publicKey, names, err.Error(), now)
		if glog.V(1) {
			glog.Infof("[extendermeasurement]%s refused: %s\n", extenderId, err)
		}
		return
	}
	state := model.SetNetworkExtenderMeasurementRequest(
		ctx,
		extenderId,
		request.Csr,
		request.PublicKey,
		[]string{request.Name},
		now,
	)
	if glog.V(1) {
		glog.Infof("[extendermeasurement]%s requested %s: %s\n", extenderId, request.Name, state)
	}
}

// Withdraws the extender with this identity key from measurement because its
// owner opted out (C4, C9, decision 18). Nothing new is issued for it until an
// activation without the flag carries its request again.
func optOutExtenderMeasurement(ctx context.Context, publicKey []byte, now time.Time) {
	withdrawn := model.OptOutNetworkExtenderMeasurement(ctx, publicKey, ExtenderMeasurementOptOutReason, now)
	if 0 < withdrawn && glog.V(1) {
		glog.Infof("[extendermeasurement]%x opted out, %d certificates withdrawn\n", publicKey, withdrawn)
	}
}

// The hex SHA-256 of a SubjectPublicKeyInfo, which A8 names a key by; "" for
// none.
func extenderMeasurementKeySha256(publicKey []byte) string {
	if len(publicKey) == 0 {
		return ""
	}
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:])
}

// The extender's own identity key, hex, which picks one of several extenders
// a client activated. Empty picks its active, most recently activated one.
type ExtenderMeasurementCertArgs struct {
	PublicKeyHex string `json:"public_key_hex,omitempty"`
}

// A8's answer.
type ExtenderMeasurementCertResult struct {
	// none, pending, issued, refused or withdrawn
	State string `json:"state"`
	// the extender's one measurement name: the request's when pending or
	// refused, the certificate's when issued, and the withdrawn certificate's
	// when withdrawn
	Names []string `json:"names"`
	// the hex SHA-256 of the SubjectPublicKeyInfo of the key the answer is
	// for: the certificate's when issued, else the request's. Every answer
	// with a request or certificate behind it carries one -- pending, issued,
	// refused and withdrawn -- save a refusal of a request that could not be
	// parsed, which has no key. An extender whose key changed tells by it
	// that what is held is not for its key, and it reads a pending answer
	// without one as nothing held (C4)
	KeySha256 string `json:"key_sha256,omitempty"`
	// the leaf first, then the issuers; only when issued
	ChainPem string `json:"chain_pem,omitempty"`
	// the leaf's expiry; only when issued. The extender reads the leaf itself.
	NotAfter *time.Time `json:"not_after,omitempty"`
	// why it is refused or withdrawn, or what a pending request waits for
	Reason string `json:"reason,omitempty"`
}

// ExtenderMeasurementCert answers an extender's poll for its measurement
// certificate (A8), with the credential it activated with.
//
// The extender reads the answer as what the operator holds for its key
// (connect/extender/extender_lag_cert.go, C4): pending and issued name what
// is held, and key_sha256 names the key, so its activations carry no new
// request while both match; none, refused and withdrawn hold nothing, so its
// next activation carries its request again, unless its owner opted out. It
// installs an issued chain at once and reads again for each renewal (C6),
// which answers issued with a new chain and expiry. It keeps serving an
// installed certificate through none, pending and refused, and drops it at
// once on withdrawn (C9) -- after a deactivation, an operator's override, or
// its owner's opt-out.
//
// The state is the extender's latest request's, with two exceptions:
//
//   - while the operator does not issue, a pending request reads as refused
//     rather than pending, which an extender keeps polling for. The request
//     stays pending in its row and keeps its place in the queue (C7), and the
//     request the extender sends again later finds it as it is then;
//   - an issued request whose certificate just ran out reads as pending,
//     since the issuance task puts it back in the queue on its next pass.
//
// A withdrawn certificate is answered withdrawn, with its name, until the
// extender's next request arrives, and never none: a request row is never
// removed, so an extender that once asked is never answered none again.
//
// The one write is the first issued answer with each certificate, which
// records its delivery (model.MarkNetworkExtenderMeasurementCertDelivered):
// the extender installs a chain only from this answer, so only then does its
// name serve, and only then do the lag.dev sample (A4) and the records'
// MeasurementId (C10) offer it.
func ExtenderMeasurementCert(
	args *ExtenderMeasurementCertArgs,
	clientSession *session.ClientSession,
) (*ExtenderMeasurementCertResult, error) {
	result := &ExtenderMeasurementCertResult{
		State: model.NetworkExtenderMeasurementStateNone,
		Names: []string{},
	}
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		// the route requires a client jwt; this is the structural guard for a
		// caller that reaches the controller another way
		result.State = model.NetworkExtenderMeasurementStateRefused
		result.Reason = "a measurement certificate requires the extender's client"
		return result, nil
	}
	var publicKey []byte
	if args != nil && strings.TrimSpace(args.PublicKeyHex) != "" {
		var err error
		publicKey, err = hex.DecodeString(strings.TrimSpace(args.PublicKeyHex))
		if err != nil {
			result.State = model.NetworkExtenderMeasurementStateRefused
			result.Reason = "the extender public key is not readable"
			return result, nil
		}
	}

	now := server.NowUtc()
	extenderId, measurement, cert := model.GetNetworkExtenderMeasurementForClient(
		clientSession.Ctx,
		*clientSession.ByJwt.ClientId,
		publicKey,
		now,
	)
	if extenderId == nil || measurement == nil {
		return result, nil
	}
	result.State = measurement.State
	result.Names = measurement.Names
	result.KeySha256 = extenderMeasurementKeySha256(measurement.PublicKey)
	result.Reason = measurement.Reason

	if measurement.State == model.NetworkExtenderMeasurementStateIssued {
		if cert != nil {
			if cert.DeliverTime == nil {
				model.MarkNetworkExtenderMeasurementCertDelivered(clientSession.Ctx, cert.CertId, now)
			}
			notAfter := cert.NotAfter
			result.Names = cert.Names
			result.KeySha256 = extenderMeasurementKeySha256(cert.PublicKey)
			result.ChainPem = cert.ChainPem
			result.NotAfter = &notAfter
			result.Reason = ""
			return result, nil
		}
		result.State = model.NetworkExtenderMeasurementStatePending
		result.Reason = "the certificate ran out and is being issued again"
	}
	if result.State == model.NetworkExtenderMeasurementStatePending {
		if config, err := EnvExtenderConfig(); err != nil || !config.Measurement.Enabled {
			result.State = model.NetworkExtenderMeasurementStateRefused
			result.Reason = "the operator does not issue measurement certificates now"
		}
	}
	return result, nil
}
