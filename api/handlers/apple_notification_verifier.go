package handlers

// App Store Server Notification V2 verification. Every JWS is independently
// verified against pinned Apple roots; the notification identity is checked
// before any controller sees claims.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

var (
	appleNotificationLeafOid         = []int{1, 2, 840, 113635, 100, 6, 11, 1}
	appleNotificationIntermediateOid = []int{1, 2, 840, 113635, 100, 6, 2, 1}
)

const (
	appleNotificationVersion   = "2.0"
	appleNotificationMaxAge    = 24 * time.Hour
	appleNotificationMaxFuture = 5 * time.Minute
)

type appleNotificationConfig struct {
	BundleId     string   `yaml:"bundle_id"`
	AppAppleId   int64    `yaml:"app_apple_id"`
	Environments []string `yaml:"environments"`
	ProductIds   []string `yaml:"product_ids"`
}

type appleConfig struct {
	AppStoreNotifications *appleNotificationConfig `yaml:"app_store_notifications"`
}

type appleJwsHeader struct {
	Algorithm string   `json:"alg"`
	X5c       []string `json:"x5c"`
}

type appleSignedDate struct {
	SignedDate int64 `json:"signedDate"`
}

type appleNotificationVerifier struct {
	config           *appleNotificationConfig
	rootCertificates func() []*x509.Certificate
	now              func() time.Time
}

var defaultAppleNotificationVerifier = sync.OnceValue(func() *appleNotificationVerifier {
	var config appleConfig
	server.Vault.RequireSimpleResource("apple.yml").UnmarshalYaml(&config)
	if config.AppStoreNotifications == nil {
		panic(errors.New("apple.yml app_store_notifications is required"))
	}
	if config.AppStoreNotifications.BundleId == "" || config.AppStoreNotifications.AppAppleId <= 0 ||
		len(config.AppStoreNotifications.Environments) == 0 || len(config.AppStoreNotifications.ProductIds) == 0 {
		panic(errors.New("apple.yml app_store_notifications is incomplete"))
	}

	return &appleNotificationVerifier{
		config:           config.AppStoreNotifications,
		rootCertificates: defaultAppleRootStore().certificates,
		now:              server.NowUtc,
	}
})

func newAppleNotificationVerifier(
	config *appleNotificationConfig,
	rootCertificates []*x509.Certificate,
	now func() time.Time,
) *appleNotificationVerifier {
	return &appleNotificationVerifier{
		config: config,
		rootCertificates: func() []*x509.Certificate {
			return append([]*x509.Certificate(nil), rootCertificates...)
		},
		now: now,
	}
}

func (self *appleNotificationVerifier) verifyNotification(
	signedPayload string,
) (*controller.AppleNotificationDecodedPayload, error) {
	outerClaims, err := self.verifyJws(signedPayload, true)
	if err != nil {
		return nil, fmt.Errorf("verify notification JWS: %w", err)
	}

	notificationBytes, err := json.Marshal(outerClaims)
	if err != nil {
		return nil, fmt.Errorf("marshal notification claims: %w", err)
	}
	var notification controller.AppleNotificationDecodedPayload
	if err := json.Unmarshal(notificationBytes, &notification); err != nil {
		return nil, fmt.Errorf("decode notification claims: %w", err)
	}
	if notification.NotificationVersion != appleNotificationVersion {
		return nil, fmt.Errorf("unexpected notification version %q", notification.NotificationVersion)
	}
	if _, err := server.ParseId(notification.NotificationUUID); err != nil {
		return nil, errors.New("invalid notification UUID")
	}

	bundleId, _ := appleStringClaim(notification.Data, "bundleId")
	environment, _ := appleStringClaim(notification.Data, "environment")
	appAppleId, _ := appleInt64Claim(notification.Data, "appAppleId")
	if bundleId != self.config.BundleId {
		return nil, errors.New("notification bundle does not match this app")
	}
	if !slices.Contains(self.config.Environments, environment) {
		return nil, errors.New("notification environment is not allowed")
	}
	if environment == "Production" && appAppleId != self.config.AppAppleId {
		return nil, errors.New("notification app Apple ID does not match this app")
	}
	if appAppleId != 0 && appAppleId != self.config.AppAppleId {
		return nil, errors.New("notification app Apple ID does not match this app")
	}

	signedTransactionInfo, _ := appleStringClaim(notification.Data, "signedTransactionInfo")
	signedRenewalInfo, _ := appleStringClaim(notification.Data, "signedRenewalInfo")
	if signedTransactionInfo != "" {
		notification.TransactionInfo, err = self.verifyJws(signedTransactionInfo, false)
		if err != nil {
			return nil, fmt.Errorf("verify signed transaction info: %w", err)
		}
		if err := self.verifyNestedIdentity(notification.TransactionInfo, bundleId, environment); err != nil {
			return nil, fmt.Errorf("transaction identity: %w", err)
		}
	}
	if signedRenewalInfo != "" {
		notification.RenewalInfo, err = self.verifyJws(signedRenewalInfo, false)
		if err != nil {
			return nil, fmt.Errorf("verify signed renewal info: %w", err)
		}
		if err := self.verifyNestedIdentity(notification.RenewalInfo, bundleId, environment); err != nil {
			return nil, fmt.Errorf("renewal identity: %w", err)
		}
	}

	switch notification.NotificationType {
	case "SUBSCRIBED", "DID_RENEW", "EXPIRED":
		if notification.TransactionInfo == nil {
			return nil, errors.New("notification has no signed transaction info")
		}
	}
	return &notification, nil
}

// verifyTransaction verifies a client-reported StoreKit transaction JWS
// (Transaction.jwsRepresentation) with the SAME pinned-root machinery as
// notification JWS: ES256, three-certificate x5c chain with the Apple policy
// extensions, terminating at a pinned Apple root, chain validity judged at the
// JWS's own signedDate. A client report is an unauthenticated-content push --
// unlike the payment reconciler's authenticated TLS pulls from Apple, which
// may decode without re-verifying -- so it gets webhook-grade verification.
//
// The signedDate freshness window is intentionally NOT applied (matching the
// nested-JWS handling in verifyNotification): the report may legitimately be
// retried days after purchase, that being the whole point of the retryable
// reporting path. The transaction's identity (bundle, environment) must match
// this app's notification config.
func (self *appleNotificationVerifier) verifyTransaction(
	signedTransaction string,
) (map[string]any, error) {
	claims, err := self.verifyJws(signedTransaction, false)
	if err != nil {
		return nil, fmt.Errorf("verify transaction JWS: %w", err)
	}
	bundleId, _ := appleStringClaim(claims, "bundleId")
	if bundleId != self.config.BundleId {
		return nil, errors.New("transaction bundle does not match this app")
	}
	environment, _ := appleStringClaim(claims, "environment")
	if !slices.Contains(self.config.Environments, environment) {
		return nil, errors.New("transaction environment is not allowed")
	}
	return claims, nil
}

func (self *appleNotificationVerifier) verifyNestedIdentity(
	claims map[string]any,
	bundleId string,
	environment string,
) error {
	if nestedBundleId, exists := appleStringClaim(claims, "bundleId"); exists && nestedBundleId != bundleId {
		return errors.New("bundle does not match outer notification")
	}
	if nestedEnvironment, exists := appleStringClaim(claims, "environment"); exists && nestedEnvironment != environment {
		return errors.New("environment does not match outer notification")
	}
	return nil
}

func (self *appleNotificationVerifier) verifyJws(signedPayload string, requireCurrent bool) (map[string]any, error) {
	parts := strings.Split(signedPayload, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid compact JWS")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("invalid JWS header encoding")
	}
	var header appleJwsHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, errors.New("invalid JWS header")
	}
	if header.Algorithm != gojwt.SigningMethodES256.Alg() || len(header.X5c) != 3 {
		return nil, errors.New("JWS must use ES256 and a three-certificate x5c chain")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("invalid JWS payload encoding")
	}
	var signedDate appleSignedDate
	if err := json.Unmarshal(payloadBytes, &signedDate); err != nil || signedDate.SignedDate <= 0 {
		return nil, errors.New("JWS has no valid signed date")
	}
	verificationTime := time.UnixMilli(signedDate.SignedDate)
	if requireCurrent {
		now := self.now()
		if verificationTime.Before(now.Add(-appleNotificationMaxAge)) ||
			verificationTime.After(now.Add(appleNotificationMaxFuture)) {
			return nil, errors.New("notification signed date is outside the accepted window")
		}
	}

	certificates := make([]*x509.Certificate, 0, len(header.X5c))
	for _, encodedCertificate := range header.X5c {
		certificateDer, err := base64.StdEncoding.DecodeString(encodedCertificate)
		if err != nil {
			return nil, errors.New("invalid x5c certificate encoding")
		}
		certificate, err := x509.ParseCertificate(certificateDer)
		if err != nil {
			return nil, errors.New("invalid x5c certificate")
		}
		certificates = append(certificates, certificate)
	}
	if !certificateHasExtension(certificates[0], appleNotificationLeafOid) ||
		!certificateHasExtension(certificates[1], appleNotificationIntermediateOid) {
		return nil, errors.New("x5c chain lacks Apple notification certificate policy extensions")
	}
	rootCertificates := self.rootCertificates()
	trustedRoot := false
	for _, rootCertificate := range rootCertificates {
		if bytes.Equal(rootCertificate.Raw, certificates[2].Raw) {
			trustedRoot = true
			break
		}
	}
	if !trustedRoot {
		return nil, errors.New("x5c chain does not terminate at a pinned Apple root")
	}

	intermediates := x509.NewCertPool()
	intermediates.AddCert(certificates[1])
	roots := x509.NewCertPool()
	for _, rootCertificate := range rootCertificates {
		roots.AddCert(rootCertificate)
	}
	if _, err := certificates[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   verificationTime,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return nil, fmt.Errorf("verify x5c certificate chain: %w", err)
	}
	publicKey, ok := certificates[0].PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve.Params().Name != "P-256" {
		return nil, errors.New("JWS leaf key is not ECDSA P-256")
	}

	claims := gojwt.MapClaims{}
	token, err := gojwt.ParseWithClaims(
		signedPayload,
		claims,
		func(token *gojwt.Token) (any, error) {
			if token.Method != gojwt.SigningMethodES256 {
				return nil, errors.New("unexpected JWS signing method")
			}
			return publicKey, nil
		},
		gojwt.WithValidMethods([]string{gojwt.SigningMethodES256.Alg()}),
		gojwt.WithoutClaimsValidation(),
	)
	if err != nil || !token.Valid {
		return nil, errors.New("invalid JWS signature")
	}
	return map[string]any(claims), nil
}

func certificateHasExtension(certificate *x509.Certificate, oid []int) bool {
	for _, extension := range certificate.Extensions {
		if extension.Id.Equal(oid) {
			return true
		}
	}
	return false
}

func appleStringClaim(claims map[string]any, key string) (string, bool) {
	value, exists := claims[key]
	if !exists {
		return "", false
	}
	stringValue, ok := value.(string)
	return stringValue, ok
}

func appleInt64Claim(claims map[string]any, key string) (int64, bool) {
	value, exists := claims[key]
	if !exists {
		return 0, false
	}
	switch value := value.(type) {
	case float64:
		if value != float64(int64(value)) {
			return 0, false
		}
		return int64(value), true
	case json.Number:
		intValue, err := value.Int64()
		return intValue, err == nil
	default:
		return 0, false
	}
}
