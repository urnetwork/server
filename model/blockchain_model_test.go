package model

import (
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestBlockchainParsing(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		blockchain := "sol"
		parsedBlockchain, err := ParseBlockchain(blockchain)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsedBlockchain, SOL)

		blockchain = "solana"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsedBlockchain, SOL)

		blockchain = "matic"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsedBlockchain, MATIC)

		blockchain = "poly"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsedBlockchain, MATIC)

		blockchain = "polygon"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsedBlockchain, MATIC)

		blockchain = "eth"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsedBlockchain, ETHEREUM)

		blockchain = "ethereum"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsedBlockchain, ETHEREUM)

		blockchain = "invalid_chain"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		assert.NotEqual(t, err, nil)

	})
}
