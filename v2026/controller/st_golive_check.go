// Offline go-live review of a candidate st.yml before it is installed or
// enabled. It applies the live loader's decoding and profile validation and
// signing admission's deployment binding of the embedded operator gas policy.
// Nothing here reads the vault, the process environment, a database or the
// network.
package controller

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026"
	stconn "github.com/urnetwork/server/v2026/st"
)

// A candidate configuration after the live validation. Config holds the
// private role keys; callers must never print, log or persist it.
type StGoLiveConfigCheck struct {
	Config *StConfig
	// The file's own flag. Validation always runs as if the selected profile
	// were enabled, because a disabled profile skips every field check.
	EnabledInFile bool
	// The top-level keys the live loader decodes. It ignores every other key.
	LoaderKeys []string
	// The identity stPayoutAdmission compares with the sn.yml schedule.
	PayoutIdentity server.ProviderPayoutMainnet
	// Nil only when the embedded policy is valid and names this deployment and
	// these deposit and root keys; otherwise mainnet signing admission refuses
	// every new EVM transaction.
	OperatorGasPolicyErr error
}

// CheckStConfigBytes decodes explicit st.yml bytes exactly as the live loader
// does and validates them with stConfigForProfile as if the profile were
// enabled, so a file kept disabled until operator registration is checked as
// it will run. Unlike InspectStConfigBytes, which redacts every reason, it
// returns the validator's field-level reason. A YAML type error can quote a
// short prefix of the offending scalar, so show errors only where the file
// itself may be read.
func CheckStConfigBytes(raw []byte, profile string, rpcUrls []string) (*StGoLiveConfigCheck, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("st.yml configuration is absent")
	}
	var file stVaultFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("st.yml cannot be decoded: %w", err)
	}
	if file.Profile != "" && file.Profile != profile {
		return nil, fmt.Errorf("st.yml profile %q does not match the selected profile %q", file.Profile, profile)
	}
	check := &StGoLiveConfigCheck{LoaderKeys: stVaultFileKeys()}
	switch profile {
	case stconn.ProfileMainnet:
		check.EnabledInFile, file.Enabled = file.Enabled, true
	case stconn.ProfileTestnet:
		check.EnabledInFile, file.TestnetEnabled = file.TestnetEnabled, true
	}
	cfg, err := stConfigForProfile(profile, file, rpcUrls)
	if err != nil {
		return nil, err
	}
	check.Config = cfg
	check.PayoutIdentity = stPayoutIdentity(cfg)
	if policy := cfg.OperatorGasPolicy.Clone(); policy == nil {
		check.OperatorGasPolicyErr = errors.New("operator gas policy is absent")
	} else if err := policy.Validate(); err != nil {
		check.OperatorGasPolicyErr = err
	} else {
		check.OperatorGasPolicyErr = stOperatorGasPolicyBinding(policy, cfg)
	}
	return check, nil
}

func stVaultFileKeys() []string {
	fileType := reflect.TypeOf(stVaultFile{})
	keys := make([]string, 0, fileType.NumField())
	for i := 0; i < fileType.NumField(); i++ {
		if key, _, _ := strings.Cut(fileType.Field(i).Tag.Get("yaml"), ","); key != "" && key != "-" {
			keys = append(keys, key)
		}
	}
	return keys
}
