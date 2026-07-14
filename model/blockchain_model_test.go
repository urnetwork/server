package model

import (
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

func TestBlockchainParsing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		blockchain := "sol"
		parsedBlockchain, err := ParseBlockchain(blockchain)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, parsedBlockchain, SOL)

		blockchain = "solana"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, parsedBlockchain, SOL)

		blockchain = "matic"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, parsedBlockchain, MATIC)

		blockchain = "poly"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, parsedBlockchain, MATIC)

		blockchain = "polygon"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, parsedBlockchain, MATIC)

		blockchain = "eth"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, parsedBlockchain, ETHEREUM)

		blockchain = "ethereum"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, parsedBlockchain, ETHEREUM)

		blockchain = "invalid_chain"
		parsedBlockchain, err = ParseBlockchain(blockchain)
		connect.AssertNotEqual(t, err, nil)

	})
}
