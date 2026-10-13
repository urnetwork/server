package work

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/route53"
)

// The two outside parties of a measurement certificate, in process
// (lag.dev/PLAN.md T2): Route 53 holding the measurement zone, and an ACME CA.
//
// Pebble is not used: it is not installed where these tests run, and the CA
// here is a deterministic stand-in with the same wire protocol that
// golang.org/x/crypto/acme speaks to Let's Encrypt (RFC 8555). It verifies
// every request's signature under the account key, validates dns-01 against
// the fake zone's records, and issues from a test root.
//
// The zone keeps two planes, as Route 53 does: what the api lists, which a
// change updates at once, and what dns answers, which a change reaches only
// when a GetChange reports it in sync. A CA that looked before the change was
// in sync would find nothing, so the issuance's wait is observable.
//
// The zone also holds the scoped IAM policy of lag.dev/ops/route53.md, and
// refuses any change batch the policy would: a batch must be A and AAAA
// records under the measurement zone, or TXT challenge records, never both,
// and never the zone's apex or its `_psl` record. So every test that writes
// to the zone also shows the writer keeps to what it owns.

// One change batch as the fake zone holds it.
type testMeasurementRoute53Change struct {
	hostedZoneId string
	changes      []*route53.Change
	polls        int
	insync       bool
}

type testMeasurementRoute53 struct {
	stateLock   sync.Mutex
	hostedZones []*route53.HostedZone
	// the measurement zone the IAM policy is scoped to, lower case, no
	// trailing dot
	policyZone string
	// the record sets the api lists, by hosted zone id
	listed map[string][]*route53.ResourceRecordSet
	// the record sets dns answers with, by hosted zone id
	served map[string][]*route53.ResourceRecordSet
	// GetChange polls before a change is in sync
	pollsUntilInsync int
	changes          map[string]*testMeasurementRoute53Change
	// the batches the policy allowed, in order
	changeInputs []*route53.ChangeResourceRecordSetsInput
	// the batches the policy refused
	deniedInputs   []*route53.ChangeResourceRecordSetsInput
	getChangeCount int
	// the names the last listing delivered, in order
	listedNames []string
}

func newTestMeasurementRoute53(policyZone string, hostedZones ...*route53.HostedZone) *testMeasurementRoute53 {
	return &testMeasurementRoute53{
		hostedZones:      hostedZones,
		policyZone:       policyZone,
		listed:           map[string][]*route53.ResourceRecordSet{},
		served:           map[string][]*route53.ResourceRecordSet{},
		pollsUntilInsync: 1,
		changes:          map[string]*testMeasurementRoute53Change{},
	}
}

func testRoute53BareZoneId(hostedZoneId string) string {
	return strings.TrimPrefix(hostedZoneId, "/hostedzone/")
}

// Seeds a record set as if it had long been in the zone, written by someone
// the policy does not bind.
func (self *testMeasurementRoute53) seed(hostedZoneId string, recordSet *route53.ResourceRecordSet) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.listed[hostedZoneId] = append(self.listed[hostedZoneId], recordSet)
	self.served[hostedZoneId] = append(self.served[hostedZoneId], recordSet)
}

func testMeasurementRecordSetKey(recordSet *route53.ResourceRecordSet) string {
	return extenderDnsFqdn(aws.StringValue(recordSet.Name)) + " " +
		aws.StringValue(recordSet.Type) + " " +
		aws.StringValue(recordSet.SetIdentifier)
}

// The order Route 53 lists names in: by the labels reversed, with the
// trailing dot, as plain ascii.
func testRoute53NameOrderKey(name string) string {
	labels := strings.Split(strings.TrimSuffix(extenderDnsFqdn(name), "."), ".")
	slices.Reverse(labels)
	return strings.Join(labels, ".") + "."
}

func testMeasurementRecordSetValues(recordSet *route53.ResourceRecordSet) []string {
	values := []string{}
	for _, resourceRecord := range recordSet.ResourceRecords {
		values = append(values, aws.StringValue(resourceRecord.Value))
	}
	slices.Sort(values)
	return values
}

func testMeasurementRecordSetsEqual(a *route53.ResourceRecordSet, b *route53.ResourceRecordSet) bool {
	return testMeasurementRecordSetKey(a) == testMeasurementRecordSetKey(b) &&
		aws.Int64Value(a.TTL) == aws.Int64Value(b.TTL) &&
		slices.Equal(testMeasurementRecordSetValues(a), testMeasurementRecordSetValues(b))
}

// Applies a batch to one plane, all or nothing, as Route 53 does.
func testMeasurementApplyChanges(
	recordSets []*route53.ResourceRecordSet,
	changes []*route53.Change,
) ([]*route53.ResourceRecordSet, error) {
	applied := slices.Clone(recordSets)
	for _, change := range changes {
		key := testMeasurementRecordSetKey(change.ResourceRecordSet)
		index := slices.IndexFunc(applied, func(recordSet *route53.ResourceRecordSet) bool {
			return testMeasurementRecordSetKey(recordSet) == key
		})
		name := extenderDnsFqdn(aws.StringValue(change.ResourceRecordSet.Name))
		switch aws.StringValue(change.Action) {
		case route53.ChangeActionUpsert, route53.ChangeActionCreate:
			// a cname shares its name with nothing
			for _, recordSet := range applied {
				if extenderDnsFqdn(aws.StringValue(recordSet.Name)) != name {
					continue
				}
				if (aws.StringValue(recordSet.Type) == route53.RRTypeCname) != (aws.StringValue(change.ResourceRecordSet.Type) == route53.RRTypeCname) {
					return nil, awserr.New(route53.ErrCodeInvalidChangeBatch, "a cname cannot share the name "+name, nil)
				}
			}
			if 0 <= index {
				if aws.StringValue(change.Action) == route53.ChangeActionCreate {
					return nil, awserr.New(route53.ErrCodeInvalidChangeBatch, "the record set exists: "+key, nil)
				}
				applied[index] = change.ResourceRecordSet
			} else {
				applied = append(applied, change.ResourceRecordSet)
			}
		case route53.ChangeActionDelete:
			// a delete must name the set exactly as it exists
			if index < 0 || !testMeasurementRecordSetsEqual(applied[index], change.ResourceRecordSet) {
				return nil, awserr.New(route53.ErrCodeInvalidChangeBatch, "no such record set: "+key, nil)
			}
			applied = slices.Delete(applied, index, index+1)
		default:
			return nil, fmt.Errorf("unknown action %s", aws.StringValue(change.Action))
		}
	}
	return applied, nil
}

// Whether the scoped policy allows a batch (lag.dev/ops/route53.md 5.1): every
// change an A or AAAA record under the measurement zone, or every change a
// TXT record at `_acme-challenge.*` under it; never the apex or `_psl`.
func (self *testMeasurementRoute53) policyAllows(changes []*route53.Change) bool {
	addressBatch := true
	challengeBatch := true
	for _, change := range changes {
		name := strings.TrimSuffix(extenderDnsFqdn(aws.StringValue(change.ResourceRecordSet.Name)), ".")
		recordType := aws.StringValue(change.ResourceRecordSet.Type)
		if name == self.policyZone || name == "_psl."+self.policyZone {
			return false
		}
		under := strings.HasSuffix(name, "."+self.policyZone)
		addressBatch = addressBatch && under && !strings.Contains(name, "\\052") &&
			(recordType == route53.RRTypeA || recordType == route53.RRTypeAaaa)
		challengeBatch = challengeBatch && under && strings.HasPrefix(name, "_acme-challenge.") &&
			recordType == route53.RRTypeTxt
	}
	return 0 < len(changes) && (addressBatch || challengeBatch)
}

func (self *testMeasurementRoute53) ListHostedZonesByNameWithContext(
	_ aws.Context,
	input *route53.ListHostedZonesByNameInput,
	_ ...request.Option,
) (*route53.ListHostedZonesByNameOutput, error) {
	dnsName := extenderDnsFqdn(aws.StringValue(input.DNSName))
	hostedZones := []*route53.HostedZone{}
	for _, hostedZone := range self.hostedZones {
		if extenderDnsFqdn(aws.StringValue(hostedZone.Name)) < dnsName {
			continue
		}
		hostedZones = append(hostedZones, hostedZone)
	}
	return &route53.ListHostedZonesByNameOutput{HostedZones: hostedZones, IsTruncated: aws.Bool(false)}, nil
}

func (self *testMeasurementRoute53) GetHostedZoneWithContext(
	_ aws.Context,
	input *route53.GetHostedZoneInput,
	_ ...request.Option,
) (*route53.GetHostedZoneOutput, error) {
	for _, hostedZone := range self.hostedZones {
		if testRoute53BareZoneId(aws.StringValue(hostedZone.Id)) == testRoute53BareZoneId(aws.StringValue(input.Id)) {
			return &route53.GetHostedZoneOutput{HostedZone: hostedZone}, nil
		}
	}
	return nil, awserr.New(route53.ErrCodeNoSuchHostedZone, "no such hosted zone", nil)
}

// The zone in Route 53's order, from the start name on, two record sets a
// page, so the pager and its early stop are exercised.
func (self *testMeasurementRoute53) ListResourceRecordSetsPagesWithContext(
	_ aws.Context,
	input *route53.ListResourceRecordSetsInput,
	fn func(*route53.ListResourceRecordSetsOutput, bool) bool,
	_ ...request.Option,
) error {
	recordSets := func() []*route53.ResourceRecordSet {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.listedNames = []string{}
		recordSets := self.orderedWithLock(testRoute53BareZoneId(aws.StringValue(input.HostedZoneId)))
		if input.StartRecordName != nil {
			startKey := testRoute53NameOrderKey(aws.StringValue(input.StartRecordName))
			recordSets = slices.DeleteFunc(recordSets, func(recordSet *route53.ResourceRecordSet) bool {
				return testRoute53NameOrderKey(aws.StringValue(recordSet.Name)) < startKey
			})
		}
		return recordSets
	}()
	for start := 0; start < len(recordSets); start += 2 {
		end := min(start+2, len(recordSets))
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for _, recordSet := range recordSets[start:end] {
				self.listedNames = append(self.listedNames, extenderDnsFqdn(aws.StringValue(recordSet.Name)))
			}
		}()
		if !fn(&route53.ListResourceRecordSetsOutput{ResourceRecordSets: recordSets[start:end]}, end == len(recordSets)) {
			return nil
		}
	}
	return nil
}

func (self *testMeasurementRoute53) orderedWithLock(hostedZoneId string) []*route53.ResourceRecordSet {
	recordSets := slices.Clone(self.listed[hostedZoneId])
	slices.SortFunc(recordSets, func(a *route53.ResourceRecordSet, b *route53.ResourceRecordSet) int {
		if c := strings.Compare(testRoute53NameOrderKey(aws.StringValue(a.Name)), testRoute53NameOrderKey(aws.StringValue(b.Name))); c != 0 {
			return c
		}
		return strings.Compare(testMeasurementRecordSetKey(a), testMeasurementRecordSetKey(b))
	})
	return recordSets
}

func (self *testMeasurementRoute53) ChangeResourceRecordSetsWithContext(
	_ aws.Context,
	input *route53.ChangeResourceRecordSetsInput,
	_ ...request.Option,
) (*route53.ChangeResourceRecordSetsOutput, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.policyAllows(input.ChangeBatch.Changes) {
		self.deniedInputs = append(self.deniedInputs, input)
		return nil, awserr.New("AccessDenied", "the policy does not allow this change batch", nil)
	}
	hostedZoneId := testRoute53BareZoneId(aws.StringValue(input.HostedZoneId))
	applied, err := testMeasurementApplyChanges(self.listed[hostedZoneId], input.ChangeBatch.Changes)
	if err != nil {
		return nil, err
	}
	self.changeInputs = append(self.changeInputs, input)
	self.listed[hostedZoneId] = applied
	changeId := fmt.Sprintf("/change/C%04d", len(self.changes)+1)
	change := &testMeasurementRoute53Change{
		hostedZoneId: hostedZoneId,
		changes:      input.ChangeBatch.Changes,
	}
	self.changes[changeId] = change
	status := route53.ChangeStatusPending
	if self.pollsUntilInsync <= 0 {
		self.insyncWithLock(change)
		status = route53.ChangeStatusInsync
	}
	return &route53.ChangeResourceRecordSetsOutput{
		ChangeInfo: &route53.ChangeInfo{Id: aws.String(changeId), Status: aws.String(status)},
	}, nil
}

// Puts a change on the served plane.
func (self *testMeasurementRoute53) insyncWithLock(change *testMeasurementRoute53Change) {
	if change.insync {
		return
	}
	change.insync = true
	served := self.served[change.hostedZoneId]
	for _, c := range change.changes {
		key := testMeasurementRecordSetKey(c.ResourceRecordSet)
		served = slices.DeleteFunc(slices.Clone(served), func(recordSet *route53.ResourceRecordSet) bool {
			return testMeasurementRecordSetKey(recordSet) == key
		})
		if aws.StringValue(c.Action) != route53.ChangeActionDelete {
			served = append(served, c.ResourceRecordSet)
		}
	}
	self.served[change.hostedZoneId] = served
}

func (self *testMeasurementRoute53) GetChangeWithContext(
	_ aws.Context,
	input *route53.GetChangeInput,
	_ ...request.Option,
) (*route53.GetChangeOutput, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.getChangeCount += 1
	change, ok := self.changes[aws.StringValue(input.Id)]
	if !ok {
		return nil, awserr.New(route53.ErrCodeNoSuchChange, "no such change", nil)
	}
	change.polls += 1
	if self.pollsUntilInsync <= change.polls {
		self.insyncWithLock(change)
	}
	status := route53.ChangeStatusPending
	if change.insync {
		status = route53.ChangeStatusInsync
	}
	return &route53.GetChangeOutput{
		ChangeInfo: &route53.ChangeInfo{Id: input.Id, Status: aws.String(status)},
	}, nil
}

// The TXT values dns answers for a name, each joined back from its strings.
func (self *testMeasurementRoute53) servedTxt(name string) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	values := []string{}
	for _, recordSets := range self.served {
		for _, recordSet := range recordSets {
			if extenderDnsFqdn(aws.StringValue(recordSet.Name)) != extenderDnsFqdn(name) || aws.StringValue(recordSet.Type) != route53.RRTypeTxt {
				continue
			}
			for _, resourceRecord := range recordSet.ResourceRecords {
				value := aws.StringValue(resourceRecord.Value)
				value = strings.ReplaceAll(value, "\" \"", "")
				values = append(values, strings.Trim(value, "\""))
			}
		}
	}
	return values
}

// The address values dns answers for a name, sorted, by record type.
func (self *testMeasurementRoute53) servedAddresses(name string) map[string][]string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	addresses := map[string][]string{}
	for _, recordSets := range self.served {
		for _, recordSet := range recordSets {
			recordType := aws.StringValue(recordSet.Type)
			if extenderDnsFqdn(aws.StringValue(recordSet.Name)) != extenderDnsFqdn(name) ||
				(recordType != route53.RRTypeA && recordType != route53.RRTypeAaaa) {
				continue
			}
			addresses[recordType] = append(addresses[recordType], testMeasurementRecordSetValues(recordSet)...)
		}
	}
	for recordType := range addresses {
		slices.Sort(addresses[recordType])
	}
	return addresses
}

// Takes every record set at a name out of both planes, as an operator's hand
// in the console would.
func (self *testMeasurementRoute53) remove(hostedZoneId string, name string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	atName := func(recordSet *route53.ResourceRecordSet) bool {
		return extenderDnsFqdn(aws.StringValue(recordSet.Name)) == extenderDnsFqdn(name)
	}
	self.listed[hostedZoneId] = slices.DeleteFunc(slices.Clone(self.listed[hostedZoneId]), atName)
	self.served[hostedZoneId] = slices.DeleteFunc(slices.Clone(self.served[hostedZoneId]), atName)
}

// The record sets the api lists for one zone, in the zone's order.
func (self *testMeasurementRoute53) listedRecordSets(hostedZoneId string) []*route53.ResourceRecordSet {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.orderedWithLock(hostedZoneId)
}

// The batches the policy allowed and refused so far.
func (self *testMeasurementRoute53) batches() (allowed []*route53.ChangeResourceRecordSetsInput, denied []*route53.ChangeResourceRecordSetsInput) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.changeInputs), slices.Clone(self.deniedInputs)
}

// The ACME CA.

type testAcmeAccount struct {
	url        string
	publicKey  *ecdsa.PublicKey
	thumbprint string
}

type testAcmeAuthz struct {
	id          string
	account     string
	identifier  string
	status      string
	token       string
	challengeId string
}

type testAcmeOrder struct {
	id          string
	account     string
	identifiers []string
	authzIds    []string
	status      string
	certId      string
}

type testAcmeCa struct {
	t      testing.TB
	server *httptest.Server
	zone   *testMeasurementRoute53

	stateLock sync.Mutex
	rootKey   *ecdsa.PrivateKey
	rootCert  *x509.Certificate
	rootDer   []byte
	nextId    int
	nonces    map[string]bool
	accounts  map[string]*testAcmeAccount
	orders    map[string]*testAcmeOrder
	authzs    map[string]*testAcmeAuthz
	chains    map[string][][]byte
	// every leaf issued, oldest first
	leaves      []*x509.Certificate
	revocations map[string]int
	// the lifetime of a leaf
	lifetime time.Duration
	// whether a valid authorization of the account is reused by a later
	// order, as the CA does for a while after a validation
	reuseAuthorizations bool
	// validations that found no matching record in dns
	failedValidations int
	orderCount        int
	// orders that carried a notBefore or a notAfter, which the CA rejects
	validityOrders int
}

func newTestAcmeCa(t testing.TB, zone *testMeasurementRoute53) *testAcmeCa {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test measurement root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	rootDer, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCert, err := x509.ParseCertificate(rootDer)
	if err != nil {
		t.Fatal(err)
	}
	ca := &testAcmeCa{
		t:           t,
		zone:        zone,
		rootKey:     rootKey,
		rootCert:    rootCert,
		rootDer:     rootDer,
		nonces:      map[string]bool{},
		accounts:    map[string]*testAcmeAccount{},
		orders:      map[string]*testAcmeOrder{},
		authzs:      map[string]*testAcmeAuthz{},
		chains:      map[string][][]byte{},
		revocations: map[string]int{},
		lifetime:    90 * 24 * time.Hour,
	}
	ca.server = httptest.NewServer(http.HandlerFunc(ca.serve))
	t.Cleanup(ca.server.Close)
	return ca
}

func (self *testAcmeCa) directoryUrl() string {
	return self.server.URL + "/directory"
}

func (self *testAcmeCa) url(path string) string {
	return self.server.URL + path
}

func (self *testAcmeCa) newIdWithLock() string {
	self.nextId += 1
	return fmt.Sprintf("%d", self.nextId)
}

func (self *testAcmeCa) newNonceWithLock() string {
	nonce := fmt.Sprintf("nonce-%s", self.newIdWithLock())
	self.nonces[nonce] = true
	return nonce
}

func (self *testAcmeCa) problem(w http.ResponseWriter, status int, problemType string, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":   "urn:ietf:params:acme:error:" + problemType,
		"detail": detail,
		"status": status,
	})
}

func (self *testAcmeCa) reply(w http.ResponseWriter, status int, location string, body any) {
	if location != "" {
		w.Header().Set("Location", location)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// The RFC 7638 thumbprint of a P-256 jwk.
func testAcmeThumbprint(x string, y string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

type testAcmeJwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (self testAcmeJwk) publicKey() (*ecdsa.PublicKey, error) {
	if self.Kty != "EC" || self.Crv != "P-256" {
		return nil, fmt.Errorf("unsupported jwk %s %s", self.Kty, self.Crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(self.X)
	if err != nil {
		return nil, err
	}
	y, err := base64.RawURLEncoding.DecodeString(self.Y)
	if err != nil {
		return nil, err
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
}

// Reads and verifies one JWS request (RFC 8555 §6.2): the nonce is one this
// CA issued and unused, the url is the request's own, and the signature
// verifies under the account's key, or under the embedded jwk for a new
// account. Returns the payload, the account (nil for a new one) and the jwk.
func (self *testAcmeCa) readJwsWithLock(r *http.Request) ([]byte, *testAcmeAccount, *testAcmeJwk, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, nil, err
	}
	var jws struct {
		Protected string `json:"protected"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(body, &jws); err != nil {
		return nil, nil, nil, err
	}
	protectedJson, err := base64.RawURLEncoding.DecodeString(jws.Protected)
	if err != nil {
		return nil, nil, nil, err
	}
	var protected struct {
		Alg   string       `json:"alg"`
		Nonce string       `json:"nonce"`
		Url   string       `json:"url"`
		Kid   string       `json:"kid"`
		Jwk   *testAcmeJwk `json:"jwk"`
	}
	if err := json.Unmarshal(protectedJson, &protected); err != nil {
		return nil, nil, nil, err
	}
	if protected.Alg != "ES256" {
		return nil, nil, nil, fmt.Errorf("alg %s", protected.Alg)
	}
	if !self.nonces[protected.Nonce] {
		return nil, nil, nil, fmt.Errorf("bad nonce")
	}
	delete(self.nonces, protected.Nonce)
	if protected.Url != self.url(r.URL.Path) {
		return nil, nil, nil, fmt.Errorf("url %s is not %s", protected.Url, self.url(r.URL.Path))
	}

	var account *testAcmeAccount
	var publicKey *ecdsa.PublicKey
	switch {
	case protected.Kid != "":
		account = self.accounts[protected.Kid]
		if account == nil {
			return nil, nil, nil, fmt.Errorf("no account %s", protected.Kid)
		}
		publicKey = account.publicKey
	case protected.Jwk != nil:
		publicKey, err = protected.Jwk.publicKey()
		if err != nil {
			return nil, nil, nil, err
		}
	default:
		return nil, nil, nil, fmt.Errorf("neither kid nor jwk")
	}
	signature, err := base64.RawURLEncoding.DecodeString(jws.Signature)
	if err != nil || len(signature) != 64 {
		return nil, nil, nil, fmt.Errorf("signature is not readable")
	}
	digest := sha256.Sum256([]byte(jws.Protected + "." + jws.Payload))
	if !ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return nil, nil, nil, fmt.Errorf("signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		return nil, nil, nil, err
	}
	return payload, account, protected.Jwk, nil
}

func (self *testAcmeCa) orderJsonWithLock(order *testAcmeOrder) map[string]any {
	identifiers := []map[string]string{}
	for _, identifier := range order.identifiers {
		identifiers = append(identifiers, map[string]string{"type": "dns", "value": identifier})
	}
	authorizations := []string{}
	for _, authzId := range order.authzIds {
		authorizations = append(authorizations, self.url("/authz/"+authzId))
	}
	orderJson := map[string]any{
		"status":         order.status,
		"expires":        time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"identifiers":    identifiers,
		"authorizations": authorizations,
		"finalize":       self.url("/finalize/" + order.id),
	}
	if order.certId != "" {
		orderJson["certificate"] = self.url("/cert/" + order.certId)
	}
	return orderJson
}

func (self *testAcmeCa) authzJsonWithLock(authz *testAcmeAuthz) map[string]any {
	return map[string]any{
		"status":     authz.status,
		"expires":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"identifier": map[string]string{"type": "dns", "value": authz.identifier},
		"challenges": []map[string]string{{
			"type":   "dns-01",
			"url":    self.url("/chall/" + authz.challengeId),
			"token":  authz.token,
			"status": authz.status,
		}},
	}
}

func (self *testAcmeCa) serve(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	w.Header().Set("Replay-Nonce", self.newNonceWithLock())
	w.Header().Set("Cache-Control", "no-store")

	path := r.URL.Path
	if r.Method == http.MethodGet && path == "/directory" {
		self.reply(w, http.StatusOK, "", map[string]any{
			"newNonce":   self.url("/new-nonce"),
			"newAccount": self.url("/new-account"),
			"newOrder":   self.url("/new-order"),
			"revokeCert": self.url("/revoke-cert"),
			"keyChange":  self.url("/key-change"),
			"meta":       map[string]any{"termsOfService": "https://ca.example/terms"},
		})
		return
	}
	if path == "/new-nonce" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		self.problem(w, http.StatusMethodNotAllowed, "malformed", "method")
		return
	}

	payload, account, jwk, err := self.readJwsWithLock(r)
	if err != nil {
		if err.Error() == "bad nonce" {
			self.problem(w, http.StatusBadRequest, "badNonce", err.Error())
		} else {
			self.problem(w, http.StatusUnauthorized, "unauthorized", err.Error())
		}
		return
	}

	switch {
	case path == "/new-account":
		if jwk == nil {
			self.problem(w, http.StatusBadRequest, "malformed", "a new account carries its jwk")
			return
		}
		thumbprint := testAcmeThumbprint(jwk.X, jwk.Y)
		for _, existing := range self.accounts {
			if existing.thumbprint == thumbprint {
				self.reply(w, http.StatusOK, existing.url, map[string]any{"status": "valid"})
				return
			}
		}
		publicKey, _ := jwk.publicKey()
		accountUrl := self.url("/account/" + self.newIdWithLock())
		self.accounts[accountUrl] = &testAcmeAccount{url: accountUrl, publicKey: publicKey, thumbprint: thumbprint}
		self.reply(w, http.StatusCreated, accountUrl, map[string]any{"status": "valid"})

	case path == "/new-order":
		var newOrder struct {
			Identifiers []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"identifiers"`
			NotBefore string `json:"notBefore"`
			NotAfter  string `json:"notAfter"`
		}
		if account == nil || json.Unmarshal(payload, &newOrder) != nil {
			self.problem(w, http.StatusBadRequest, "malformed", "order")
			return
		}
		if newOrder.NotBefore != "" || newOrder.NotAfter != "" {
			// as the CA does (lag.dev/PLAN.md C6)
			self.validityOrders += 1
			self.problem(w, http.StatusBadRequest, "malformed", "notBefore and notAfter are not supported")
			return
		}
		self.orderCount += 1
		order := &testAcmeOrder{id: self.newIdWithLock(), account: account.url, status: "pending"}
		for _, identifier := range newOrder.Identifiers {
			order.identifiers = append(order.identifiers, identifier.Value)
			var authz *testAcmeAuthz
			if self.reuseAuthorizations {
				for _, existing := range self.authzs {
					if existing.account == account.url && existing.identifier == identifier.Value && existing.status == "valid" {
						authz = existing
					}
				}
			}
			if authz == nil {
				token := make([]byte, 16)
				rand.Read(token)
				authz = &testAcmeAuthz{
					id:          self.newIdWithLock(),
					account:     account.url,
					identifier:  identifier.Value,
					status:      "pending",
					token:       base64.RawURLEncoding.EncodeToString(token),
					challengeId: self.newIdWithLock(),
				}
				self.authzs[authz.id] = authz
			}
			order.authzIds = append(order.authzIds, authz.id)
		}
		self.updateOrderWithLock(order)
		self.orders[order.id] = order
		self.reply(w, http.StatusCreated, self.url("/order/"+order.id), self.orderJsonWithLock(order))

	case strings.HasPrefix(path, "/order/"):
		order := self.orders[strings.TrimPrefix(path, "/order/")]
		if order == nil {
			self.problem(w, http.StatusNotFound, "malformed", "no such order")
			return
		}
		self.updateOrderWithLock(order)
		self.reply(w, http.StatusOK, self.url(path), self.orderJsonWithLock(order))

	case strings.HasPrefix(path, "/authz/"):
		authz := self.authzs[strings.TrimPrefix(path, "/authz/")]
		if authz == nil {
			self.problem(w, http.StatusNotFound, "malformed", "no such authorization")
			return
		}
		self.reply(w, http.StatusOK, "", self.authzJsonWithLock(authz))

	case strings.HasPrefix(path, "/chall/"):
		challengeId := strings.TrimPrefix(path, "/chall/")
		var authz *testAcmeAuthz
		for _, existing := range self.authzs {
			if existing.challengeId == challengeId {
				authz = existing
			}
		}
		if authz == nil || account == nil || authz.account != account.url {
			self.problem(w, http.StatusNotFound, "malformed", "no such challenge")
			return
		}
		if authz.status == "pending" {
			// dns-01 (RFC 8555 §8.4): the digest of the key authorization in
			// a TXT record that dns answers with now
			digest := sha256.Sum256([]byte(authz.token + "." + account.thumbprint))
			expected := base64.RawURLEncoding.EncodeToString(digest[:])
			if slices.Contains(self.zone.servedTxt("_acme-challenge."+authz.identifier), expected) {
				authz.status = "valid"
			} else {
				self.failedValidations += 1
				authz.status = "invalid"
			}
		}
		self.reply(w, http.StatusOK, "", map[string]string{
			"type":   "dns-01",
			"url":    self.url(path),
			"token":  authz.token,
			"status": authz.status,
		})

	case strings.HasPrefix(path, "/finalize/"):
		order := self.orders[strings.TrimPrefix(path, "/finalize/")]
		if order == nil {
			self.problem(w, http.StatusNotFound, "malformed", "no such order")
			return
		}
		self.updateOrderWithLock(order)
		if order.status != "ready" {
			self.problem(w, http.StatusForbidden, "orderNotReady", order.status)
			return
		}
		var finalize struct {
			Csr string `json:"csr"`
		}
		if json.Unmarshal(payload, &finalize) != nil {
			self.problem(w, http.StatusBadRequest, "malformed", "finalize")
			return
		}
		csrDer, err := base64.RawURLEncoding.DecodeString(finalize.Csr)
		if err != nil {
			self.problem(w, http.StatusBadRequest, "badCSR", err.Error())
			return
		}
		csr, err := x509.ParseCertificateRequest(csrDer)
		if err != nil || csr.CheckSignature() != nil {
			self.problem(w, http.StatusBadRequest, "badCSR", "the request does not verify")
			return
		}
		csrNames := slices.Clone(csr.DNSNames)
		slices.Sort(csrNames)
		orderNames := slices.Clone(order.identifiers)
		slices.Sort(orderNames)
		if !slices.Equal(csrNames, orderNames) {
			self.problem(w, http.StatusBadRequest, "badCSR", "the request names other identifiers")
			return
		}
		serialBytes := make([]byte, 16)
		rand.Read(serialBytes)
		notBefore := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
		leafTemplate := &x509.Certificate{
			SerialNumber: new(big.Int).SetBytes(serialBytes),
			Subject:      pkix.Name{CommonName: orderNames[0]},
			DNSNames:     orderNames,
			NotBefore:    notBefore,
			NotAfter:     notBefore.Add(self.lifetime),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		leafDer, err := x509.CreateCertificate(rand.Reader, leafTemplate, self.rootCert, csr.PublicKey, self.rootKey)
		if err != nil {
			self.problem(w, http.StatusInternalServerError, "serverInternal", err.Error())
			return
		}
		leaf, _ := x509.ParseCertificate(leafDer)
		self.leaves = append(self.leaves, leaf)
		order.certId = self.newIdWithLock()
		self.chains[order.certId] = [][]byte{leafDer, self.rootDer}
		order.status = "valid"
		self.reply(w, http.StatusOK, self.url("/order/"+order.id), self.orderJsonWithLock(order))

	case strings.HasPrefix(path, "/cert/"):
		chain := self.chains[strings.TrimPrefix(path, "/cert/")]
		if chain == nil {
			self.problem(w, http.StatusNotFound, "malformed", "no such certificate")
			return
		}
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.WriteHeader(http.StatusOK)
		for _, der := range chain {
			pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		}

	case path == "/revoke-cert":
		var revoke struct {
			Certificate string `json:"certificate"`
			Reason      int    `json:"reason"`
		}
		if account == nil || json.Unmarshal(payload, &revoke) != nil {
			self.problem(w, http.StatusBadRequest, "malformed", "revoke")
			return
		}
		der, err := base64.RawURLEncoding.DecodeString(revoke.Certificate)
		if err != nil {
			self.problem(w, http.StatusBadRequest, "malformed", err.Error())
			return
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil || !slices.ContainsFunc(self.leaves, func(issued *x509.Certificate) bool { return bytes.Equal(issued.Raw, der) }) {
			self.problem(w, http.StatusNotFound, "malformed", "not a certificate this ca issued")
			return
		}
		serialHex := hex.EncodeToString(leaf.SerialNumber.Bytes())
		if _, ok := self.revocations[serialHex]; ok {
			self.problem(w, http.StatusBadRequest, "alreadyRevoked", "already revoked")
			return
		}
		self.revocations[serialHex] = revoke.Reason
		w.WriteHeader(http.StatusOK)

	default:
		self.problem(w, http.StatusNotFound, "malformed", "no such resource "+path)
	}
}

// An order is ready once every authorization is valid, and invalid once any
// is.
func (self *testAcmeCa) updateOrderWithLock(order *testAcmeOrder) {
	if order.status != "pending" {
		return
	}
	ready := true
	for _, authzId := range order.authzIds {
		switch self.authzs[authzId].status {
		case "invalid":
			order.status = "invalid"
			return
		case "valid":
		default:
			ready = false
		}
	}
	if ready {
		order.status = "ready"
	}
}

func (self *testAcmeCa) setLifetime(lifetime time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lifetime = lifetime
}

func (self *testAcmeCa) setReuseAuthorizations(reuseAuthorizations bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reuseAuthorizations = reuseAuthorizations
}

func (self *testAcmeCa) validationFailures() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.failedValidations
}

func (self *testAcmeCa) orderTotal() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.orderCount
}

func (self *testAcmeCa) validityOrderTotal() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.validityOrders
}

// The reason each issued leaf was revoked with, by serial, hex.
func (self *testAcmeCa) revokedReasons() map[string]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reasons := map[string]int{}
	for serialHex, reason := range self.revocations {
		reasons[serialHex] = reason
	}
	return reasons
}

func (self *testAcmeCa) issuedCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.leaves)
}

func (self *testAcmeCa) roots() *x509.CertPool {
	roots := x509.NewCertPool()
	roots.AddCert(self.rootCert)
	return roots
}
