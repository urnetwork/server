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

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

type appleTestCertificateChain struct {
	rootCertificate *x509.Certificate
	leafCertificate *x509.Certificate
	certificates    []string
	leafKey         *ecdsa.PrivateKey
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
	bundleId string,
	environment string,
	tamperTransaction bool,
) string {
	t.Helper()
	transaction := chain.sign(t, gojwt.MapClaims{
		"signedDate":      now.UnixMilli(),
		"bundleId":        bundleId,
		"environment":     environment,
		"appAccountToken": server.NewId().String(),
		"transactionId":   "transaction-1",
		"productId":       "supporter_monthly_26",
		"purchaseDate":    now.Add(-time.Hour).UnixMilli(),
		"expiresDate":     now.Add(30 * 24 * time.Hour).UnixMilli(),
		"price":           int64(4990),
	})
	if tamperTransaction {
		parts := strings.Split(transaction, ".")
		if parts[2][0] == 'A' {
			parts[2] = "B" + parts[2][1:]
		} else {
			parts[2] = "A" + parts[2][1:]
		}
		transaction = strings.Join(parts, ".")
	}

	return chain.sign(t, gojwt.MapClaims{
		"notificationType": "SUBSCRIBED",
		"subtype":          "INITIAL_BUY",
		"notificationUUID": server.NewId().String(),
		"version":          appleNotificationVersion,
		"signedDate":       now.UnixMilli(),
		"data": map[string]any{
			"bundleId":              bundleId,
			"environment":           environment,
			"appAppleId":            int64(6741000606),
			"signedTransactionInfo": transaction,
		},
	})
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

	happyPayload := appleTestSignedNotification(t, trustedChain, now, "network.ur", "Production", false)
	notification, err := verifier.verifyNotification(happyPayload)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, notification.TransactionInfo["transactionId"], "transaction-1")

	untrustedChain := newAppleTestCertificateChain(t, now, now.Add(24*time.Hour))
	untrustedPayload := appleTestSignedNotification(t, untrustedChain, now, "network.ur", "Production", false)
	_, err = verifier.verifyNotification(untrustedPayload)
	connect.AssertEqual(t, err != nil, true)

	tamperedPayload := appleTestSignedNotification(t, trustedChain, now, "network.ur", "Production", true)
	_, err = verifier.verifyNotification(tamperedPayload)
	connect.AssertEqual(t, err != nil, true)

	wrongBundlePayload := appleTestSignedNotification(t, trustedChain, now, "other.bundle", "Production", false)
	_, err = verifier.verifyNotification(wrongBundlePayload)
	connect.AssertEqual(t, err != nil, true)

	wrongEnvironmentPayload := appleTestSignedNotification(t, trustedChain, now, "network.ur", "Development", false)
	_, err = verifier.verifyNotification(wrongEnvironmentPayload)
	connect.AssertEqual(t, err != nil, true)
}

func TestAppleNotificationVerifierRejectsExpiredCertificate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	expiredChain := newAppleTestCertificateChain(t, now, now.Add(-time.Hour))
	verifier := appleTestVerifier(expiredChain, now)
	payload := appleTestSignedNotification(t, expiredChain, now, "network.ur", "Production", false)

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

func TestAppleNotificationBodyLimit(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/apple-notification",
		strings.NewReader(strings.Repeat("x", 1024*1024+1)),
	)
	recorder := httptest.NewRecorder()

	AppleNotification(recorder, request)
	connect.AssertEqual(t, recorder.Code, http.StatusRequestEntityTooLarge)
}
