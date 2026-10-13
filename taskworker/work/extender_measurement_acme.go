package work

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The ACME side of lag.dev measurement certificates (lag.dev/PLAN.md C5, C6,
// C9): one account, orders validated by DNS-01 in the measurement zone, and
// revocation, through golang.org/x/crypto/acme.
//
// That library speaks RFC 8555 and has no renewal information (ARI), so there
// is no CA-suggested window: a certificate renews at two thirds of its
// lifetime (C6). An order sets no notBefore or notAfter, which Let's Encrypt
// rejects, and names its identifiers only, so a change of the name shape
// (PLAN.md Q5) changes nothing here.
//
// One client is kept per directory and account key for the process, since
// the account url and the directory are worth one lookup, not one per pass.
// A client's methods are safe for concurrent use.

const (
	// A whole order, from the order to the chain, including the wait for the
	// challenge records to be in sync. Route 53 is in sync within about a
	// minute.
	ExtenderMeasurementOrderTimeout = 5 * time.Minute

	// One request to the CA outside an order, a revocation, and the removal
	// of an order's challenge records.
	ExtenderMeasurementAcmeRequestTimeout = 30 * time.Second

	extenderMeasurementAcmeUserAgent = "urnetwork-extender-measurement"
)

// One TXT record a DNS-01 challenge needs (C5): the name is
// `_acme-challenge.<the identifier>`, without the trailing dot.
type extenderMeasurementChallengeRecord struct {
	name  string
	value string
}

// Writes the challenge records of one order into the measurement zone.
// present returns once the records are in sync on every authoritative server,
// and remove takes them out again.
type extenderMeasurementChallenges interface {
	present(ctx context.Context, records []extenderMeasurementChallengeRecord) error
	remove(ctx context.Context, records []extenderMeasurementChallengeRecord) error
}

type extenderMeasurementAcme struct {
	client   *acme.Client
	contacts []string

	stateLock sync.Mutex
	// whether the account is known to the CA, which every order needs
	registered bool
}

// The clients of the process, by directory and account key.
var extenderMeasurementAcmeStateLock sync.Mutex
var extenderMeasurementAcmes = map[string]*extenderMeasurementAcme{}

// Testing_ResetExtenderMeasurementAcme drops every kept client, so a test
// starts with an unregistered account against its own CA.
func Testing_ResetExtenderMeasurementAcme() {
	extenderMeasurementAcmeStateLock.Lock()
	defer extenderMeasurementAcmeStateLock.Unlock()
	clear(extenderMeasurementAcmes)
}

// The client of the configured directory and account (X3).
func extenderMeasurementAcmeForConfig(config *controller.ExtenderConfig) (*extenderMeasurementAcme, error) {
	accountKey, err := config.Measurement.AcmeAccountPrivateKey()
	if err != nil {
		return nil, err
	}
	directoryUrl := config.Measurement.AcmeDirectory()
	keyDer, err := x509.MarshalECPrivateKey(accountKey)
	if err != nil {
		return nil, err
	}
	keyHash := sha256.Sum256(keyDer)
	cacheKey := directoryUrl + "\x00" + hex.EncodeToString(keyHash[:])

	extenderMeasurementAcmeStateLock.Lock()
	defer extenderMeasurementAcmeStateLock.Unlock()
	if client, ok := extenderMeasurementAcmes[cacheKey]; ok {
		return client, nil
	}
	client := newExtenderMeasurementAcme(directoryUrl, accountKey, config.Measurement.AcmeContacts())
	extenderMeasurementAcmes[cacheKey] = client
	return client, nil
}

func newExtenderMeasurementAcme(directoryUrl string, accountKey *ecdsa.PrivateKey, contacts []string) *extenderMeasurementAcme {
	return &extenderMeasurementAcme{
		client: &acme.Client{
			Key:          accountKey,
			DirectoryURL: directoryUrl,
			HTTPClient: &http.Client{
				Timeout: ExtenderMeasurementAcmeRequestTimeout,
			},
			UserAgent: extenderMeasurementAcmeUserAgent,
		},
		contacts: slices.Clone(contacts),
	}
}

// Makes sure the account exists. Operations create the account the CAA
// record pins (X3), and registering its key again only finds it; a new key,
// in a test or a fresh environment, is registered, accepting the terms of
// service, which configuring the key is the operator's consent to.
func (self *extenderMeasurementAcme) register(ctx context.Context) error {
	registered := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.registered
	}()
	if registered {
		return nil
	}
	_, err := self.client.Register(ctx, &acme.Account{Contact: self.contacts}, acme.AcceptTOS)
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return fmt.Errorf("the acme account could not be registered: %w", err)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.registered = true
	}()
	return nil
}

// issue orders one certificate for exactly `names` with the request `csr`
// (C5) and returns its chain, DER, the leaf first.
//
// Each authorization the CA still needs is answered with DNS-01: every
// challenge record goes into the zone in one change, and only once Route 53
// reports the change in sync are the challenges accepted, so the CA never
// looks before the records are on every authoritative server. The records
// are removed again however the order ends. An authorization the CA already
// holds as valid is not asked again.
func (self *extenderMeasurementAcme) issue(
	ctx context.Context,
	names []string,
	csr []byte,
	challenges extenderMeasurementChallenges,
) ([][]byte, error) {
	if err := self.register(ctx); err != nil {
		return nil, err
	}
	order, err := self.client.AuthorizeOrder(ctx, acme.DomainIDs(names...))
	if err != nil {
		return nil, fmt.Errorf("the order was not created: %w", err)
	}

	records := []extenderMeasurementChallengeRecord{}
	pendingChallenges := []*acme.Challenge{}
	pendingAuthzUrls := []string{}
	for _, authzUrl := range order.AuthzURLs {
		authz, err := self.client.GetAuthorization(ctx, authzUrl)
		if err != nil {
			return nil, fmt.Errorf("the authorization was not read: %w", err)
		}
		switch authz.Status {
		case acme.StatusValid:
			continue
		case acme.StatusPending:
		default:
			return nil, fmt.Errorf("the authorization of %s is %s", authz.Identifier.Value, authz.Status)
		}
		var challenge *acme.Challenge
		for _, c := range authz.Challenges {
			if c.Type == "dns-01" {
				challenge = c
				break
			}
		}
		if challenge == nil {
			return nil, fmt.Errorf("the ca offers no dns-01 challenge for %s", authz.Identifier.Value)
		}
		value, err := self.client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return nil, err
		}
		records = append(records, extenderMeasurementChallengeRecord{
			name:  "_acme-challenge." + authz.Identifier.Value,
			value: value,
		})
		pendingChallenges = append(pendingChallenges, challenge)
		pendingAuthzUrls = append(pendingAuthzUrls, authz.URI)
	}

	if 0 < len(records) {
		defer func() {
			// the records go however the order ended, on a context of their
			// own, since the order's may be what ended it
			removeCtx, removeCancel := context.WithTimeout(context.WithoutCancel(ctx), ExtenderMeasurementAcmeRequestTimeout)
			defer removeCancel()
			if err := challenges.remove(removeCtx, records); err != nil {
				glog.Errorf("[extendermeasurement]challenge records were not removed: %s\n", err)
			}
		}()
		if err := challenges.present(ctx, records); err != nil {
			return nil, fmt.Errorf("the challenge records were not published: %w", err)
		}
		for _, challenge := range pendingChallenges {
			if _, err := self.client.Accept(ctx, challenge); err != nil {
				return nil, fmt.Errorf("the challenge was not accepted: %w", err)
			}
		}
		for _, authzUrl := range pendingAuthzUrls {
			if _, err := self.client.WaitAuthorization(ctx, authzUrl); err != nil {
				return nil, fmt.Errorf("the authorization did not validate: %w", err)
			}
		}
	}

	order, err = self.client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("the order did not become ready: %w", err)
	}
	chain, _, err := self.client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("the order was not finalized: %w", err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("the ca returned no certificate")
	}
	return chain, nil
}

// revoke revokes one certificate with the account key that it was issued to,
// for the cessation of operation (C9). A certificate the CA already revoked
// is no error.
func (self *extenderMeasurementAcme) revoke(ctx context.Context, leaf []byte) error {
	if err := self.register(ctx); err != nil {
		return err
	}
	return self.client.RevokeCert(ctx, nil, leaf, acme.CRLReasonCessationOfOperation)
}

// When a certificate renews (C6): two thirds of the way through its lifetime.
func extenderMeasurementRenewTime(notBefore time.Time, notAfter time.Time) time.Time {
	return notBefore.Add(2 * notAfter.Sub(notBefore) / 3)
}

// newExtenderMeasurementCert checks a chain the CA returned against what was
// asked and builds its row (C5): the leaf must carry the request's key and
// exactly its names, since a certificate for anything else is not one this
// operator asked for. The renewal is set at two thirds of the lifetime (C6).
func newExtenderMeasurementCert(
	extenderId server.Id,
	names []string,
	csr []byte,
	publicKey []byte,
	chain [][]byte,
	capCounted bool,
	now time.Time,
) (*model.NetworkExtenderMeasurementCert, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("the chain is empty")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, fmt.Errorf("the certificate is not readable: %w", err)
	}
	if !bytes.Equal(leaf.RawSubjectPublicKeyInfo, publicKey) {
		return nil, fmt.Errorf("the certificate is not for the requested key")
	}
	leafNames := slices.Clone(leaf.DNSNames)
	slices.Sort(leafNames)
	wantNames := slices.Clone(names)
	slices.Sort(wantNames)
	if !slices.Equal(leafNames, wantNames) || 0 < len(leaf.IPAddresses) {
		return nil, fmt.Errorf("the certificate names %s, not %s", strings.Join(leafNames, ","), strings.Join(wantNames, ","))
	}
	if !leaf.NotBefore.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate lifetime is empty")
	}

	chainPem := &bytes.Buffer{}
	for _, der := range chain {
		if err := pem.Encode(chainPem, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			return nil, err
		}
	}
	return &model.NetworkExtenderMeasurementCert{
		CertId:     server.NewId(),
		ExtenderId: extenderId,
		Names:      wantNames,
		Csr:        csr,
		PublicKey:  publicKey,
		ChainPem:   chainPem.String(),
		SerialHex:  leaf.SerialNumber.Text(16),
		NotBefore:  leaf.NotBefore,
		NotAfter:   leaf.NotAfter,
		IssueTime:  now,
		CapCounted: capCounted,
		RenewTime:  extenderMeasurementRenewTime(leaf.NotBefore, leaf.NotAfter),
	}, nil
}

// The leaf of a stored chain.
func extenderMeasurementLeaf(chainPem string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(chainPem))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("the stored chain has no certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
