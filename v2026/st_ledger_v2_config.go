package server

import (
	"errors"
	"regexp"
	"strings"
)

var ledgerV2AddressPattern = regexp.MustCompile(`^0x[0-9a-f]{40}$`)

// ValidateLedgerV2Addresses checks the optional settlement v2 identity that
// sn.yml and st.yml declare (VERSION2-FINAL.md §13): both absent, which keeps
// v2 off, or two distinct nonzero lowercase 0x addresses.
func ValidateLedgerV2Addresses(settlement string, vault string) error {
	if settlement == "" && vault == "" {
		return nil
	}
	if !ledgerV2Address(settlement) {
		return errors.New("settlement_v2_address must be a nonzero lowercase 0x address")
	}
	if !ledgerV2Address(vault) {
		return errors.New("vault_v2_address must be a nonzero lowercase 0x address")
	}
	if settlement == vault {
		return errors.New("settlement_v2_address and vault_v2_address must differ")
	}
	return nil
}

func ledgerV2Address(value string) bool {
	return ledgerV2AddressPattern.MatchString(value) && value != "0x"+strings.Repeat("0", 40)
}
