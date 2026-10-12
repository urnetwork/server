package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/urnetwork/connect"
)

// Defaults and readers of the measurement block of `extender.yml`
// (lag.dev/PLAN.md C). Every reader takes the block as configured and answers
// with the default for an unset or nonsense value, so a caller never compares
// against a zero value itself.

const (
	// The CA of decision 3, Let's Encrypt's production directory.
	ExtenderMeasurementDefaultAcmeDirectoryUrl = "https://acme-v02.api.letsencrypt.org/directory"

	// Seconds a measurement name's address records may be cached (C1). Short,
	// because the records follow the extender's addresses (N3): an address
	// change shows within a reconcile pass plus this.
	ExtenderMeasurementDefaultDnsTtl = 60

	// New certificates per seven days (C7). Every new name under the
	// registered domain shares one budget of 50 a week at the CA, so this
	// leaves room for everything else issued under it. An extender has one
	// name, so only a new extender spends it; a renewal does not.
	ExtenderMeasurementDefaultIssueCapPerWeek = 30
)

// MeasurementZone is the zone measurement names are under (N1), lower case and
// without a trailing dot, which is the form a certificate name takes. Unset is
// connect's default zone.
func (self *ExtenderMeasurementConfig) MeasurementZone() string {
	zone := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(self.ZoneName), "."))
	if zone == "" {
		return connect.ExtenderMeasurementZone
	}
	return zone
}

// AcmeDirectory is the ACME directory url orders go to (C5).
func (self *ExtenderMeasurementConfig) AcmeDirectory() string {
	directoryUrl := strings.TrimSpace(self.AcmeDirectoryUrl)
	if directoryUrl == "" {
		return ExtenderMeasurementDefaultAcmeDirectoryUrl
	}
	return directoryUrl
}

// RecordTtl is the ttl of a measurement name's A and AAAA records (C1, C8).
func (self *ExtenderMeasurementConfig) RecordTtl() int {
	if self.DnsTtl <= 0 {
		return ExtenderMeasurementDefaultDnsTtl
	}
	return self.DnsTtl
}

// WeeklyIssueCap is the most new certificates any seven days may issue (C7).
func (self *ExtenderMeasurementConfig) WeeklyIssueCap() int {
	if self.IssueCapPerWeek <= 0 {
		return ExtenderMeasurementDefaultIssueCapPerWeek
	}
	return self.IssueCapPerWeek
}

// AcmeContacts is the account's contact list, empty when none is configured.
// A bare address is taken as a mailto: contact.
func (self *ExtenderMeasurementConfig) AcmeContacts() []string {
	contact := strings.TrimSpace(self.AcmeContact)
	if contact == "" {
		return []string{}
	}
	if !strings.Contains(contact, ":") {
		contact = "mailto:" + contact
	}
	return []string{contact}
}

// AcmeAccountPrivateKey parses the ACME account key (X3): PEM, an ECDSA P-256
// key in either the SEC 1 or the PKCS #8 form. Any other key is refused rather
// than used, since the account's key type is part of what the CAA record pins
// through the account url.
func (self *ExtenderMeasurementConfig) AcmeAccountPrivateKey() (*ecdsa.PrivateKey, error) {
	keyPem := strings.TrimSpace(self.AcmeAccountKey)
	if keyPem == "" {
		return nil, fmt.Errorf("the measurement block has no acme_account_key")
	}
	block, _ := pem.Decode([]byte(keyPem))
	if block == nil {
		return nil, fmt.Errorf("the measurement acme_account_key is not pem")
	}
	var key any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("the measurement acme_account_key is a %s, not an ecdsa key", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("the measurement acme_account_key is not readable: %w", err)
	}
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok || ecdsaKey.Curve != elliptic.P256() {
		return nil, fmt.Errorf("the measurement acme_account_key must be an ecdsa p-256 key")
	}
	return ecdsaKey, nil
}
