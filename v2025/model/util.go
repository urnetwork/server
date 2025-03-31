package model

import (
	"strings"

	// TODO replace this with fewer works from a larger dictionary
	bip39 "github.com/tyler-smith/go-bip39"
)

func newCode() (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", err
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", err
	}

	words := strings.Split(mnemonic, " ")
	code := strings.Join(words[0:6], " ")
	return code, nil
}
