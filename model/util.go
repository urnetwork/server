package model

import (
	"crypto/rand"
	"fmt"
	"math/big"
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

func newCodeBase32() (string, error) {
	return rand.Text(), nil
}

func randomBip39Word() (string, error) {
	wordList := bip39.GetWordList()
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(wordList))))
	if err != nil {
		return "", err
	}
	return wordList[n.Int64()], nil
}

func generateRandomNetworkName() (string, error) {
	for {
		w1, err := randomBip39Word()
		if err != nil {
			return "", err
		}
		w2, err := randomBip39Word()
		if err != nil {
			return "", err
		}
		r, err := rand.Int(rand.Reader, big.NewInt(100))
		if err != nil {
			return "", err
		}
		num := r.Int64()
		name := fmt.Sprintf("%s-%s-%d", w1, w2, num)
		if len(name) >= 5 && len(name) <= 50 {
			return name, nil
		}
	}
}
