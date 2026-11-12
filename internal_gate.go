package server

// api's internal surface for the hosts with no Postgres (QD-055: plan-SOCK-gate-data, plan-QUOTA-host-reach):
// every route under InternalGatePathPrefix takes one caller's bearer, verified by hash. QD-055 also has the lb
// refuse the prefix from outside, which warp does not do yet, so until it does the bearer is the gate alone. The
// scheme is the one plan-TLS-internal-credentials and plan-DNS-internal-route-credential settle for the dns API's
// internal routes (QD-027):
//
//   - each caller holds 256 random bits, its current bearer and during a rotation its previous one, in
//     InternalGateCredentialFileName(<service>) among its secret_files, read once at start, and sends the
//     current one as `Authorization: Bearer <bearer>`;
//   - api holds only their SHA-256, by caller name with its class, in InternalGateVerifyFileName among its
//     secret_files, and compares a bearer's digest with every digest in constant time, so a rotation needs no
//     downtime: the verify file lists both while callers move;
//   - each route takes only the classes it lists, so one service's bearer reaches no other's reads.
//
// The two file names and the verify file's layout are this draft's reading: the decisions name what each file
// holds, not what it is called. A process whose file is missing or does not parse holds no credential: a caller
// sends nothing and its host stays not ready, and api answers every internal route 403.

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

// Where every internal route lives on api.
const InternalGatePathPrefix = "/internal/gate/"

// api's verify file among its secret_files: SHA-256 digests only, never a bearer.
const InternalGateVerifyFileName = "internal-gate-verify.yml"

// A caller's credential file among its secret_files.
func InternalGateCredentialFileName(caller InternalGateCaller) string {
	return "internal-gate-" + string(caller) + ".yml"
}

// The internal gate's credential format, each limit a field whose default is the format's value (QD-702).
type InternalGateSettings struct {
	// The bytes of one bearer: 256 bits, sent as unpadded base64url. A format limit both ends hold alike, so a
	// credential file of any other size is refused.
	BearerByteCount int
}

func DefaultInternalGateSettings() *InternalGateSettings {
	return &InternalGateSettings{
		BearerByteCount: 32,
	}
}

// A bearer's encoding.
var internalGateBearerEncoding = base64.RawURLEncoding

// Who calls an internal route, by the bearer it carries: §19.4's services with no Postgres. Each route lists the
// classes it takes.
type InternalGateCaller string

const (
	InternalGateCallerEvs     InternalGateCaller = "evs"
	InternalGateCallerGkm     InternalGateCaller = "gkm"
	InternalGateCallerSockets InternalGateCaller = "sockets"
	InternalGateCallerTlsedge InternalGateCaller = "tlsedge"
	InternalGateCallerMxedge  InternalGateCaller = "mxedge"
	// connect's contract gate in CreateContract, which reads projections through api (QD-680)
	InternalGateCallerConnect InternalGateCaller = "connect"
)

// Whether a class is one this release knows.
func (self InternalGateCaller) known() bool {
	switch self {
	case InternalGateCallerEvs, InternalGateCallerGkm, InternalGateCallerSockets, InternalGateCallerTlsedge, InternalGateCallerMxedge,
		InternalGateCallerConnect:
		return true
	}
	return false
}

// A credential file or verify file that cannot be used. The text names the part, never a bearer.
var ErrInternalGateCredentialRefused = errors.New("internal gate credential refused")

// One caller's entry in the verify file.
type internalGateVerifyEntryYaml struct {
	Class  string   `yaml:"class"`
	Sha256 []string `yaml:"sha256"`
}

// One digest a bearer may hash to, and its class.
type internalGateDigest struct {
	caller InternalGateCaller
	digest [sha256.Size]byte
}

// api's view of its verify file. Safe for concurrent use; no I/O.
type InternalGateVerifier struct {
	digests []internalGateDigest
}

// Reads the verify file: a map of caller name to its class and the SHA-256, in hex, of each bearer it may send,
// its current and its previous during a rotation:
//
//	evs: {class: evs, sha256: [<hex>, <hex>]}
func NewInternalGateVerifier(data []byte) (*InternalGateVerifier, error) {
	refuse := func(format string, arguments ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInternalGateCredentialRefused, InternalGateVerifyFileName, fmt.Sprintf(format, arguments...))
	}
	entries := map[string]internalGateVerifyEntryYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&entries); err != nil {
		return nil, refuse("%v", err)
	}
	self := &InternalGateVerifier{}
	for callerName, entry := range entries {
		caller := InternalGateCaller(entry.Class)
		if !caller.known() {
			return nil, refuse("%s names no class", callerName)
		}
		if len(entry.Sha256) == 0 {
			return nil, refuse("%s holds no digest", callerName)
		}
		for _, text := range entry.Sha256 {
			digest, err := hex.DecodeString(text)
			if err != nil || len(digest) != sha256.Size {
				return nil, refuse("%s holds a digest that is not SHA-256 in hex", callerName)
			}
			self.digests = append(self.digests, internalGateDigest{caller: caller, digest: [sha256.Size]byte(digest)})
		}
	}
	if len(self.digests) == 0 {
		return nil, refuse("no caller")
	}
	return self, nil
}

// The class whose bearer the request carries, or false for none or one the verify file does not hold. It hashes
// the bearer and compares the digest with every digest in constant time, with no early exit, so the time taken
// says nothing of which caller, if any, it matched.
func (self *InternalGateVerifier) Authenticate(request *http.Request) (InternalGateCaller, bool) {
	bearer, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(bearer))
	var caller InternalGateCaller
	matched := 0
	for index := range self.digests {
		if subtle.ConstantTimeCompare(digest[:], self.digests[index].digest[:]) == 1 {
			caller = self.digests[index].caller
			matched = 1
		}
	}
	return caller, matched == 1
}

// api's verify file from its vault. An error names the part, and api then refuses every internal route.
func LoadInternalGateVerifier() (*InternalGateVerifier, error) {
	resource, err := Vault.SimpleResource(InternalGateVerifyFileName)
	if err != nil {
		return nil, fmt.Errorf("%w: no %s: %v", ErrInternalGateCredentialRefused, InternalGateVerifyFileName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInternalGateCredentialRefused, InternalGateVerifyFileName, err)
	}
	return NewInternalGateVerifier(data)
}

// One caller's credential file: its current bearer and, during a rotation, the previous one, each 256 bits in
// unpadded base64url.
type internalGateCredentialYaml struct {
	Current  string `yaml:"current"`
	Previous string `yaml:"previous"`
}

// Reads a caller's credential file and returns the bearer it sends: current. Both values must be well formed, so
// a file broken in its previous half is caught before a rotation needs it.
func ParseInternalGateCredential(data []byte) (string, error) {
	settings := DefaultInternalGateSettings()
	credential := &internalGateCredentialYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(credential); err != nil {
		return "", fmt.Errorf("%w: credential file: %v", ErrInternalGateCredentialRefused, err)
	}
	for _, bearer := range []string{credential.Current, credential.Previous} {
		if bearer == "" {
			continue
		}
		raw, err := internalGateBearerEncoding.DecodeString(bearer)
		if err != nil || len(raw) != settings.BearerByteCount {
			return "", fmt.Errorf("%w: a bearer that is not 256 bits in base64url", ErrInternalGateCredentialRefused)
		}
	}
	if credential.Current == "" {
		return "", fmt.Errorf("%w: no current bearer", ErrInternalGateCredentialRefused)
	}
	return credential.Current, nil
}

// The caller's bearer from its vault. An error names the part, never the bearer, and the caller then sends
// nothing.
func LoadInternalGateCredential(caller InternalGateCaller) (string, error) {
	if !caller.known() {
		return "", fmt.Errorf("%w: %q is no caller", ErrInternalGateCredentialRefused, caller)
	}
	fileName := InternalGateCredentialFileName(caller)
	resource, err := Vault.SimpleResource(fileName)
	if err != nil {
		return "", fmt.Errorf("%w: no %s: %v", ErrInternalGateCredentialRefused, fileName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInternalGateCredentialRefused, fileName, err)
	}
	return ParseInternalGateCredential(data)
}
