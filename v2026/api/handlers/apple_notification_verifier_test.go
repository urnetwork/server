package handlers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

type appleTestCertificateChain struct {
	rootCertificate *x509.Certificate
	leafCertificate *x509.Certificate
	certificates    []string
	leafKey         *ecdsa.PrivateKey
}

// Selects notification identities and independent nested-signature corruption.
type appleTestNotificationOptions struct {
	bundleId          string
	environment       string
	appAppleId        int64
	nestedBundleId    string
	tamperTransaction bool
	tamperRenewal     bool
}

func newAppleTestCertificateChain(t testing.TB, now time.Time, leafNotAfter time.Time) *appleTestCertificateChain {
	t.Helper()

	newKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate test key: %v", err)
		}
		return key
	}
	newSerial := func() *big.Int {
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
		if err != nil {
			t.Fatalf("generate test serial: %v", err)
		}
		return serial
	}
	createCertificate := func(template *x509.Certificate, parent *x509.Certificate, publicKey any, parentKey any) ([]byte, *x509.Certificate) {
		certificateDer, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, parentKey)
		if err != nil {
			t.Fatalf("create test certificate: %v", err)
		}
		certificate, err := x509.ParseCertificate(certificateDer)
		if err != nil {
			t.Fatalf("parse test certificate: %v", err)
		}
		return certificateDer, certificate
	}

	rootKey := newKey()
	rootTemplate := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "Apple Test Root", Organization: []string{"Apple Inc."}},
		NotBefore:             now.Add(-48 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDer, rootCertificate := createCertificate(rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)

	intermediateKey := newKey()
	intermediateTemplate := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "Apple Test Intermediate", Organization: []string{"Apple Inc."}},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		ExtraExtensions: []pkix.Extension{{
			Id:    appleNotificationIntermediateOid,
			Value: []byte{5, 0},
		}},
	}
	intermediateDer, intermediateCertificate := createCertificate(
		intermediateTemplate,
		rootCertificate,
		&intermediateKey.PublicKey,
		rootKey,
	)

	leafKey := newKey()
	leafTemplate := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: "Apple Test Notification Signer", Organization: []string{"Apple Inc."}},
		NotBefore:    now.Add(-12 * time.Hour),
		NotAfter:     leafNotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{
			Id:    appleNotificationLeafOid,
			Value: []byte{5, 0},
		}},
	}
	leafDer, leafCertificate := createCertificate(
		leafTemplate,
		intermediateCertificate,
		&leafKey.PublicKey,
		intermediateKey,
	)

	return &appleTestCertificateChain{
		rootCertificate: rootCertificate,
		leafCertificate: leafCertificate,
		certificates: []string{
			base64.StdEncoding.EncodeToString(leafDer),
			base64.StdEncoding.EncodeToString(intermediateDer),
			base64.StdEncoding.EncodeToString(rootDer),
		},
		leafKey: leafKey,
	}
}

func (self *appleTestCertificateChain) sign(t testing.TB, claims gojwt.MapClaims) string {
	t.Helper()
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	token.Header["x5c"] = self.certificates
	signed, err := token.SignedString(self.leafKey)
	if err != nil {
		t.Fatalf("sign test JWS: %v", err)
	}
	return signed
}

func appleTestSignedNotification(
	t testing.TB,
	chain *appleTestCertificateChain,
	now time.Time,
	options appleTestNotificationOptions,
) string {
	t.Helper()
	if options.appAppleId == 0 {
		options.appAppleId = 6741000606
	}
	nestedBundleId := options.nestedBundleId
	if nestedBundleId == "" {
		nestedBundleId = options.bundleId
	}
	transaction := chain.sign(t, gojwt.MapClaims{
		"signedDate":      now.UnixMilli(),
		"bundleId":        nestedBundleId,
		"environment":     options.environment,
		"appAccountToken": server.NewId().String(),
		"transactionId":   "transaction-1",
		"productId":       "supporter_monthly_26",
		"purchaseDate":    now.Add(-time.Hour).UnixMilli(),
		"expiresDate":     now.Add(30 * 24 * time.Hour).UnixMilli(),
		"price":           int64(4990),
	})
	if options.tamperTransaction {
		transaction = tamperAppleTestJws(transaction)
	}
	renewal := chain.sign(t, gojwt.MapClaims{
		"signedDate":  now.UnixMilli(),
		"bundleId":    nestedBundleId,
		"environment": options.environment,
		"productId":   "supporter_monthly_26",
	})
	if options.tamperRenewal {
		renewal = tamperAppleTestJws(renewal)
	}

	return chain.sign(t, gojwt.MapClaims{
		"notificationType": "SUBSCRIBED",
		"subtype":          "INITIAL_BUY",
		"notificationUUID": server.NewId().String(),
		"version":          appleNotificationVersion,
		"signedDate":       now.UnixMilli(),
		"data": map[string]any{
			"bundleId":              options.bundleId,
			"environment":           options.environment,
			"appAppleId":            options.appAppleId,
			"signedTransactionInfo": transaction,
			"signedRenewalInfo":     renewal,
		},
	})
}

// Corrupts a generated compact JWS without changing its signed claims.
func tamperAppleTestJws(signed string) string {
	parts := strings.Split(signed, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	return strings.Join(parts, ".")
}

func appleTestVerifier(chain *appleTestCertificateChain, now time.Time) *appleNotificationVerifier {
	return newAppleNotificationVerifier(
		&appleNotificationConfig{
			BundleId:     "network.ur",
			AppAppleId:   6741000606,
			Environments: []string{"Production", "Sandbox"},
			ProductIds:   []string{"supporter_monthly_26"},
		},
		[]*x509.Certificate{chain.rootCertificate},
		func() time.Time { return now },
	)
}

func TestAppleNotificationVerifierTrustAndIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	trustedChain := newAppleTestCertificateChain(t, now, now.Add(24*time.Hour))
	verifier := appleTestVerifier(trustedChain, now)

	happyPayload := appleTestSignedNotification(t, trustedChain, now, appleTestNotificationOptions{
		bundleId:    "network.ur",
		environment: "Production",
	})
	notification, err := verifier.verifyNotification(happyPayload)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, notification.TransactionInfo["transactionId"], "transaction-1")
	connect.AssertEqual(t, notification.RenewalInfo["productId"], "supporter_monthly_26")

	untrustedChain := newAppleTestCertificateChain(t, now, now.Add(24*time.Hour))
	untrustedPayload := appleTestSignedNotification(t, untrustedChain, now, appleTestNotificationOptions{
		bundleId:    "network.ur",
		environment: "Production",
	})
	_, err = verifier.verifyNotification(untrustedPayload)
	connect.AssertEqual(t, err != nil, true)

	tamperedPayload := appleTestSignedNotification(t, trustedChain, now, appleTestNotificationOptions{
		bundleId:          "network.ur",
		environment:       "Production",
		tamperTransaction: true,
	})
	_, err = verifier.verifyNotification(tamperedPayload)
	connect.AssertEqual(t, err != nil, true)
	tamperedRenewalPayload := appleTestSignedNotification(t, trustedChain, now, appleTestNotificationOptions{
		bundleId:      "network.ur",
		environment:   "Production",
		tamperRenewal: true,
	})
	_, err = verifier.verifyNotification(tamperedRenewalPayload)
	connect.AssertEqual(t, err != nil, true)

	wrongBundlePayload := appleTestSignedNotification(t, trustedChain, now, appleTestNotificationOptions{
		bundleId:    "other.bundle",
		environment: "Production",
	})
	_, err = verifier.verifyNotification(wrongBundlePayload)
	connect.AssertEqual(t, err != nil, true)

	wrongEnvironmentPayload := appleTestSignedNotification(t, trustedChain, now, appleTestNotificationOptions{
		bundleId:    "network.ur",
		environment: "Development",
	})
	_, err = verifier.verifyNotification(wrongEnvironmentPayload)
	connect.AssertEqual(t, err != nil, true)

	wrongAppPayload := appleTestSignedNotification(t, trustedChain, now, appleTestNotificationOptions{
		bundleId:    "network.ur",
		environment: "Production",
		appAppleId:  1,
	})
	_, err = verifier.verifyNotification(wrongAppPayload)
	connect.AssertEqual(t, err != nil, true)

	nestedIdentityPayload := appleTestSignedNotification(t, trustedChain, now, appleTestNotificationOptions{
		bundleId:       "network.ur",
		environment:    "Production",
		nestedBundleId: "other.bundle",
	})
	_, err = verifier.verifyNotification(nestedIdentityPayload)
	connect.AssertEqual(t, err != nil, true)
}

func TestAppleNotificationVerifierRejectsExpiredCertificate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	expiredChain := newAppleTestCertificateChain(t, now, now.Add(-time.Hour))
	verifier := appleTestVerifier(expiredChain, now)
	payload := appleTestSignedNotification(t, expiredChain, now, appleTestNotificationOptions{
		bundleId:    "network.ur",
		environment: "Production",
	})

	_, err := verifier.verifyNotification(payload)
	connect.AssertEqual(t, err != nil, true)
}

func TestParseAppleRootCertificates(t *testing.T) {
	now := time.Now().UTC()
	chain := newAppleTestCertificateChain(t, now, now.Add(24*time.Hour))
	rootPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain.rootCertificate.Raw})

	certificates, err := parseAppleRootCertificates(rootPem)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(certificates), 1)
}

func TestConfiguredAppleRootCertificates(t *testing.T) {
	certificates, err := parseAppleRootCertificates(server.Config.RequireBytes("apple_roots.pem"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(certificates), 3)

	expectedCommonNames := map[string]bool{
		"Apple Root CA":      true,
		"Apple Root CA - G2": true,
		"Apple Root CA - G3": true,
	}
	for _, certificate := range certificates {
		commonName := certificate.Subject.CommonName
		if !expectedCommonNames[commonName] {
			t.Fatalf("unexpected configured Apple root %q", certificate.Subject.CommonName)
		}
		delete(expectedCommonNames, commonName)
		if !certificate.IsCA {
			t.Fatalf("configured Apple root %q is not a CA", certificate.Subject.CommonName)
		}
		if err := verifyAppleRootSelfSignature(certificate); err != nil {
			t.Fatalf("configured Apple root %q is not self-signed: %v", certificate.Subject.CommonName, err)
		}
	}
	if len(expectedCommonNames) != 0 {
		t.Fatalf("configured Apple roots are missing common names: %v", expectedCommonNames)
	}

	tamperedRoot := *certificates[0]
	tamperedRoot.Signature = append([]byte(nil), tamperedRoot.Signature...)
	tamperedRoot.Signature[0] ^= 1
	if err := validateAppleRootCertificate(&tamperedRoot); err == nil {
		t.Fatal("configured Apple root validation accepted a tampered self-signature")
	}
}

func TestAppleNotificationBodyLimit(t *testing.T) {
	const maxAppleNotificationBytes = 1024 * 1024
	request := httptest.NewRequest(
		http.MethodPost,
		"/apple-notification",
		strings.NewReader(strings.Repeat("x", maxAppleNotificationBytes+1)),
	)
	recorder := httptest.NewRecorder()

	AppleNotification(recorder, request)
	connect.AssertEqual(t, recorder.Code, http.StatusRequestEntityTooLarge)
}
