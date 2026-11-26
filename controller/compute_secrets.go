package controller

// ComputeServerSecrets (DESIGN-COMPUTE §9.11, §14.4, compute-2; BAL-M12-07): sidecar/controller's ComputeLedgerSecrets
// on the server, the payer and promotion hash keys the ledger's keyed hashes are made under, from the process's vault
// file compute-ledger.yml:
//
//	payer_hash_key: <base64 of at least 32 bytes>
//	promo_hash_key: <base64 of at least 32 bytes, not the payer's>
//
// Each call reads the file again, as the webhooks' secrets are read (vault_secret.go), so a vault fix takes effect
// without a restart; a process without the file, or with a key missing, blank, malformed, short, or the same for both,
// answers an error, and the ledger refuses every purchase and redemption (fails closed). No key or byte of one is
// logged: the file is read without the resource's logging accessors, and every error names the file and the key's
// name alone.
//
// InstallComputeLedgerSecrets registers it with sidecar/controller once a process, for the api's compute routes, whose
// mount calls it (CSRV-M12-04); sidecarctl's commands take a ComputeServerSecrets of their own.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	sidecarcontroller "github.com/urnetwork/sidecar/controller"
)

// The vault file of the ledger's keys.
const ComputeLedgerSecretsResourceName = "compute-ledger.yml"

// The file's keys, by name.
const (
	computeLedgerPayerKeyName = "payer_hash_key"
	computeLedgerPromoKeyName = "promo_hash_key"
)

// The file's keys refused: the ledger refuses what needs them.
var ErrComputeLedgerSecrets = errors.New("compute ledger secrets")

// How the keys are checked.
type ComputeServerSecretsSettings struct {
	// The fewest bytes a key may decode to: 32, SHA-256's size, under which a keyed hash's key is weaker than its
	// output. Must be positive.
	KeyMinByteCount int
}

// The secrets' defaults, the only place one is set.
func DefaultComputeServerSecretsSettings() *ComputeServerSecretsSettings {
	return &ComputeServerSecretsSettings{
		KeyMinByteCount: 32,
	}
}

// The ledger's keys from vault (see the file header). Safe for concurrent use: it holds nothing but its settings.
type ComputeServerSecrets struct {
	settings *ComputeServerSecretsSettings
}

// The secrets over the process's vault; nil settings take the defaults.
func NewComputeServerSecrets(settings *ComputeServerSecretsSettings) *ComputeServerSecrets {
	if settings == nil {
		settings = DefaultComputeServerSecretsSettings()
	}
	return &ComputeServerSecrets{settings: settings}
}

// The file's keys, read and checked now.
func (self *ComputeServerSecrets) keys() (payerKey []byte, promoKey []byte, returnErr error) {
	read := func(name string) ([]byte, error) {
		text, err := vaultNonblankString(ComputeLedgerSecretsResourceName, name)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrComputeLedgerSecrets, err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
		if err != nil {
			return nil, fmt.Errorf("%w: %s's %s is no base64", ErrComputeLedgerSecrets, ComputeLedgerSecretsResourceName, name)
		}
		if len(key) < self.settings.KeyMinByteCount {
			return nil, fmt.Errorf("%w: %s's %s decodes to %d bytes, under %d", ErrComputeLedgerSecrets, ComputeLedgerSecretsResourceName, name, len(key), self.settings.KeyMinByteCount)
		}
		return key, nil
	}
	if self.settings.KeyMinByteCount <= 0 {
		return nil, nil, fmt.Errorf("%w: a key's least length is positive", ErrComputeLedgerSecrets)
	}
	payerKey, err := read(computeLedgerPayerKeyName)
	if err != nil {
		return nil, nil, err
	}
	promoKey, err = read(computeLedgerPromoKeyName)
	if err != nil {
		return nil, nil, err
	}
	if bytes.Equal(payerKey, promoKey) {
		return nil, nil, fmt.Errorf("%w: %s's two keys are one, which would let a code's hash stand for a payer's", ErrComputeLedgerSecrets, ComputeLedgerSecretsResourceName)
	}
	return payerKey, promoKey, nil
}

// See sidecarcontroller.ComputeLedgerSecrets.
func (self *ComputeServerSecrets) PayerHashKey() ([]byte, error) {
	payerKey, _, err := self.keys()
	return payerKey, err
}

// See sidecarcontroller.ComputeLedgerSecrets.
func (self *ComputeServerSecrets) PromoHashKey() ([]byte, error) {
	_, promoKey, err := self.keys()
	return promoKey, err
}

var installComputeLedgerSecretsOnce sync.Once

// Registers the server's secrets with sidecar/controller, once a process: a second call does nothing.
func InstallComputeLedgerSecrets() {
	installComputeLedgerSecretsOnce.Do(func() {
		sidecarcontroller.RegisterComputeLedgerSecrets(NewComputeServerSecrets(nil))
	})
}
