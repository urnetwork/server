package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	// "sync"
	"encoding/hex"
	mathrand "math/rand"
	"runtime"

	"maps"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func TestConnectNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnect\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
			})
	})
}

func TestConnectWithSymmetricContractsNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContracts\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
			})
	})
}

func TestConnectWithAsymmetricContractsNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContracts\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{})
	})
}

func TestConnectWithChaosNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaos\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaos\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaos\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
			})
	})
}

func TestConnectNoTransportReformNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{})
	})
}

func TestConnectWithChaosNoTransportReformNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos: true,
			})
	})
}

// note large nack objects can be excessively dropped
// due to the arbitrary order of receipt
// the nack object must align with the contract it was sent under
// see the notes on how contracts work for acks in connect/transfer

func TestConnectH1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnect[h1]\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH1,
			})
	})
}

func TestConnectAuto(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnect[auto]\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeAuto,
			})
	})
}

func TestConnectH3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnect[h3]\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3,
			})
	})
}

func TestConnectDns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnect[dns]\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3Dns,
			})
	})
}

func TestConnectDnsPump(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnect[dnspump]\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3DnsPump,
			})
	})
}

func TestConnectWithSymmetricContracts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContracts\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithSymmetricContractsWithForceStream(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithForceStream\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				forceStream:           true,
			})
	})
}

func TestConnectWithAsymmetricContracts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContracts\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithForceStream(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithForceStream\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				forceStream:           true,
			})
	})
}

func TestConnectWithChaosH1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaos[h1]\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH1,
			})
	})
}

func TestConnectWithChaosH3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaos[h3]\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaos(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaos\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaos(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaos\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectNoTransportReform(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack: true,
			})
	})
}

func TestConnectWithChaosNoTransportReform(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos: true,
				enableNack:  true,
			})
	})
}

// new instance testing
// this tests that the sender/receiver can reconnect with a different instance
// e.g. logout and login

func TestConnectWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnect\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithSymmetricContractsWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContracts\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContracts\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithChaosWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaos\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaos\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaos\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectNoTransportReformWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack:        true,
				enableNewInstance: true,
			})
	})
}

func TestConnectWithChaosNoTransportReformWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:       true,
				enableNack:        true,
				enableNewInstance: true,
			})
	})
}

// -----------------------------------------------------------------------
// Encrypted-sequence variants.
//
// The matrix above mirrored with `enableEncryption: true`, exercising the
// SendSequence <-> ReceiveSequence TLS session end-to-end through the server.
// -----------------------------------------------------------------------

func TestConnectNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoNackEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithSymmetricContractsNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsNoNackEncrypted\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithAsymmetricContractsNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsNoNackEncrypted\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableEncryption: true,
			})
	})
}

func TestConnectWithChaosNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoNackEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaosNoNackEncrypted\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaosNoNackEncrypted\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableEncryption:      true,
			})
	})
}

func TestConnectNoTransportReformNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReformNoNackEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableEncryption: true,
			})
	})
}

func TestConnectWithChaosNoTransportReformNoNackEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReformNoNackEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:      true,
				enableEncryption: true,
			})
	})
}

func TestConnectH1Encrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectH1Encrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH1,
				enableEncryption:      true,
			})
	})
}

func TestConnectAutoEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectAutoEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeAuto,
				enableEncryption:      true,
			})
	})
}

func TestConnectH3Encrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectH3Encrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3,
				enableEncryption:      true,
			})
	})
}

func TestConnectDnsEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectDnsEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3Dns,
				enableEncryption:      true,
			})
	})
}

func TestConnectDnsPumpEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectDnsPumpEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3DnsPump,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithSymmetricContractsEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsEncrypted\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithSymmetricContractsWithForceStreamEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithForceStreamEncrypted\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				forceStream:           true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithAsymmetricContractsEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsEncrypted\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithForceStreamEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithForceStreamEncrypted\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				forceStream:           true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithChaosH1Encrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosH1Encrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH1,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithChaosH3Encrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosH3Encrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaosEncrypted\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaosEncrypted\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
			})
	})
}

func TestConnectNoTransportReformEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReformEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack:       true,
				enableEncryption: true,
			})
	})
}

func TestConnectWithChaosNoTransportReformEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReformEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:      true,
				enableNack:       true,
				enableEncryption: true,
			})
	})
}

func TestConnectWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithNewInstanceEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithSymmetricContractsWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithNewInstanceEncrypted\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithNewInstanceEncrypted\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithChaosWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosWithNewInstanceEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaosWithNewInstanceEncrypted\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaosWithNewInstanceEncrypted\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
			})
	})
}

func TestConnectNoTransportReformWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReformWithNewInstanceEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack:        true,
				enableNewInstance: true,
				enableEncryption:  true,
			})
	})
}

func TestConnectWithChaosNoTransportReformWithNewInstanceEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReformWithNewInstanceEncrypted\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:       true,
				enableNack:        true,
				enableNewInstance: true,
				enableEncryption:  true,
			})
	})
}

// -----------------------------------------------------------------------
// Encrypted-sequence variants with EncryptAllowUnwrappedFallback=true.
//
// Same matrix as the Encrypted tests, but the sender falls back to plaintext
// if the TLS handshake fails. The handshake still completes under test, so
// these mirror the strict variants; they also exercise SendSequence's
// parallel-handshake path (app packs flow during the handshake rather than
// gating on session readiness).
// -----------------------------------------------------------------------

func TestConnectNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithSymmetricContractsNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithAsymmetricContractsNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableEncryption: true,
				allowFallback:    true,
			})
	})
}

func TestConnectWithChaosNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaosNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaosNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectNoTransportReformNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReformNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableEncryption: true,
				allowFallback:    true,
			})
	})
}

func TestConnectWithChaosNoTransportReformNoNackEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReformNoNackEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:      true,
				enableEncryption: true,
				allowFallback:    true,
			})
	})
}

func TestConnectH1EncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectH1EncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH1,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectAutoEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectAutoEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeAuto,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectH3EncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectH3EncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectDnsEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectDnsEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3Dns,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectDnsPumpEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectDnsPumpEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3DnsPump,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithSymmetricContractsEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsEncryptedAllowFallback\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithSymmetricContractsWithForceStreamEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithForceStreamEncryptedAllowFallback\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				forceStream:           true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithAsymmetricContractsEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsEncryptedAllowFallback\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithForceStreamEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithForceStreamEncryptedAllowFallback\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				forceStream:           true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithChaosH1EncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosH1EncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH1,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithChaosH3EncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosH3EncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				transportMode:         connect.TransportModeH3,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaosEncryptedAllowFallback\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaosEncryptedAllowFallback\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectNoTransportReformEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReformEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack:       true,
				enableEncryption: true,
				allowFallback:    true,
			})
	})
}

func TestConnectWithChaosNoTransportReformEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReformEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:      true,
				enableNack:       true,
				enableEncryption: true,
				allowFallback:    true,
			})
	})
}

func TestConnectWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithSymmetricContractsWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithChaosWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaosWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaosWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
				enableEncryption:      true,
				allowFallback:         true,
			})
	})
}

func TestConnectNoTransportReformWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectNoTransportReformWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack:        true,
				enableNewInstance: true,
				enableEncryption:  true,
				allowFallback:     true,
			})
	})
}

func TestConnectWithChaosNoTransportReformWithNewInstanceEncryptedAllowFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReformWithNewInstanceEncryptedAllowFallback\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:       true,
				enableNack:        true,
				enableNewInstance: true,
				enableEncryption:  true,
				allowFallback:     true,
			})
	})
}

const (
	contractTestNone      int = 0
	contractTestSymmetric     = 1
	// the normal client-provider relationship
	contractTestAsymmetric = 2
)

type testConnectConfig struct {
	enableChaos           bool
	enableTransportReform bool
	enableNack            bool
	enableNewInstance     bool
	transportMode         connect.TransportMode
	forceStream           bool
	// enableEncryption turns on the per-peer encryption session for both
	// clients (set symmetrically: both sides need Encrypt=true to handshake).
	// During the handshake (cipher nil) traffic is plaintext, then outer-wrapped.
	enableEncryption bool
	// allowFallback selects the end-to-end encryption mode when
	// enableEncryption is set: true runs EncryptionModeOpportunistic (seal
	// when established, plaintext otherwise — the historical behavior), false
	// runs EncryptionModeRequired (fail-closed: application data never
	// plaintext; the entry gate holds sends until the cipher establishes).
	// The ...EncryptedAllowFallback test battery sets true; the base
	// ...Encrypted tests exercise Required.
	allowFallback bool
}

// Cancel every carrier first, then join the whole group so H3 reservations and
// route publications are gone before a replacement group is constructed.
func closeTestConnectPlatformTransports(
	ctx context.Context,
	transports []*connect.PlatformTransport,
) error {
	for _, transport := range transports {
		transport.Close()
	}
	for i, transport := range transports {
		if err := transport.CloseAndWait(ctx); err != nil {
			return fmt.Errorf("join platform transport %d: %w", i, err)
		}
	}
	return nil
}

// Wait until one member of a replacement group has published live routes.
// H3's shared memory budget intentionally permits some peers to remain queued.
func waitForTestConnectPlatformTransport(
	ctx context.Context,
	transports []*connect.PlatformTransport,
	timeout time.Duration,
) error {
	waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
	defer waitCancel()
	for {
		for _, transport := range transports {
			if transport.IsConnected() {
				return nil
			}
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Give each logical client the production carrier capacity of the separate
// device process it represents while preserving contention among its routes.
func newTestConnectPlatformBudget() *connect.PlatformTransportBudget {
	defaults := connect.DefaultPlatformTransportBudget().Stats()
	return connect.NewPlatformTransportBudget(
		defaults.TotalByteCount,
		defaults.MaxTransportCount,
	)
}

// Prove simulated devices cannot starve each other's carrier working set while
// each device still keeps the production-sized admission ceiling.
func TestConnectFixturePlatformBudgetsAreIndependent(t *testing.T) {
	budgetA := newTestConnectPlatformBudget()
	budgetB := newTestConnectPlatformBudget()
	if budgetA == budgetB {
		t.Fatal("logical clients shared one platform transport budget")
	}
	settings := connect.DefaultPlatformTransportSettings()
	for name, budget := range map[string]*connect.PlatformTransportBudget{
		"client A": budgetA,
		"client B": budgetB,
	} {
		stats := budget.Stats()
		if stats.TotalByteCount < settings.H3BudgetByteCount {
			t.Errorf("%s platform budget=%d, below one H3 working set=%d", name, stats.TotalByteCount, settings.H3BudgetByteCount)
		}
	}
}

// this test that two clients can communicate via the connect server
// spin up two connect servers on different ports, and connect one client to each server
// send message bursts between the clients
// contract logic is optional so that the effects of contracts can be isolated
// sets all sequence buffer sizes to 0 to test deadlock
func testConnect(
	t testing.TB,
	contractTest int,
	config *testConnectConfig,
) {
	if testing.Short() {
		return
	}

	if config.transportMode == "" {
		config.transportMode = connect.TransportModeH1
	}
	fmt.Printf("[transport mode]%s\n", config.transportMode)

	type SimpleMessage struct {
		messageIndex uint32
		messageCount uint32
	}
	type Message struct {
		sourceId    connect.Id
		messages    []SimpleMessage
		provideMode protocol.ProvideMode
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	// Sequence state intentionally remains alive for the whole stress case, but
	// a missing receive/ack is a failed test and must not pause the package for
	// that entire lifetime. Keep those two bounds independent.
	sequenceStateTimeout := 900 * time.Second
	progressTimeout := 60 * time.Second

	// larger values test the send queue and receive queue sizes
	var messageContentSizes []ByteCount
	transportCount := 1
	burstM := 1
	newInstanceM := 0
	nackM := 0

	nackDroppedByteCount := ByteCount(0)
	pauseTimeout := 200 * time.Millisecond
	sequenceIdleTimeout := 5 * time.Second
	minResendInterval := 10 * time.Millisecond

	// switch config.transportMode {
	// case connect.TransportModeH1, connect.TransportModeH3:
	messageContentSizes = []ByteCount{
		1024,
		// 2048,
		// 4 * 1024,
		// 128 * 1024,
		// 1024 * 1024,
	}
	transportCount = 6
	burstM = 6
	if config.enableNewInstance {
		newInstanceM = 4
	}
	if config.enableNack {
		nackM = 6
	}
	// default:
	// 	messageContentSizes = []ByteCount{
	// 		1024,
	// 	}
	// 	// sequenceIdleTimeout = 1 * time.Second
	// 	// minResendInterval = 1 * time.Second
	// }

	// note the receiver idle timeout must be sufficiently large
	// or else retransmits might be delivered multiple times
	// send and forward idle timeouts can be arbirary values

	randPauseTimeout := func() time.Duration {
		return pauseTimeout/4 + time.Duration(mathrand.Int63n(int64(pauseTimeout)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := "connect"
	block := "test"

	type testServerEndpoint struct {
		h1Port  int
		h3Port  int
		dnsPort int
	}

	// The handshake's TLS server flight is one `EncryptedControl{Handshake}`
	// Pack, ~2 KiB protobuf-wrapped. A framer with `MaxMessageLen` below that
	// closes the transport mid-handshake, and SendSequence's resend of the
	// oversized pack deadlocks. Floor every framer cap at
	// `ClientSettings.MinimumMessageLenLimit()` (worst-case derivation there).
	framerMaxMessageLen := max(
		2*int(messageContentSizes[len(messageContentSizes)-1]),
		int(connect.DefaultClientSettings().MinimumMessageLenLimit()),
	)

	clientIdA := server.NewId()
	clientAInstanceId := server.NewId()
	clientIdB := server.NewId()
	clientBInstanceId := server.NewId()

	routes := map[string]string{}
	for i := 0; i < 10; i += 1 {
		host := fmt.Sprintf("host%d", i)
		routes[host] = "127.0.0.1"
	}

	// FIXME server quic tls to create a temp cert
	// FIXME client tls to trust self signed cert
	createServer := func(
		exchange *Exchange,
		h3PacketConn net.PacketConn,
		dnsPacketConn net.PacketConn,
	) (*http.Server, *ConnectHandler) {
		settings := DefaultConnectHandlerSettings()
		// settings.EnableTlsSelfSign = true
		settings.ListenH3Port = 0
		settings.ListenDnsPort = 0
		settings.EnableProxyProtocol = false
		settings.FramerSettings.MaxMessageLen = framerMaxMessageLen
		settings.TransportTlsSettings.EnableSelfSign = true
		settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
		settings.ConnectionAnnounceTimeout = 0
		// settings.ConnectionRateLimitSettings.MaxTotalConnectionCount = 1000
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
		// settings.MaximumExchangeMessageByteCount = 2 * int(messageContentSizes[len(messageContentSizes)-1])
		connectHandler := NewConnectHandlerWithPacketConns(
			ctx,
			server.NewId(),
			exchange,
			settings,
			ConnectHandlerPacketConns{
				H3:  h3PacketConn,
				Dns: dnsPacketConn,
			},
		)

		routes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("GET", "/", connectHandler.Connect),
		}

		routerHandler := router.NewRouter(ctx, routes)

		server := &http.Server{
			Handler: routerHandler,
		}
		return server, connectHandler
	}

	hostEndpoints := map[string]testServerEndpoint{}
	exchanges := map[string]*Exchange{}
	servers := map[string]*http.Server{}
	connectHandlers := []*ConnectHandler{}
	for i := 0; i < 10; i += 1 {
		host := fmt.Sprintf("host%d", i)
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("connect listener %d: %v", i, err)
		}
		h1Port := listener.Addr().(*net.TCPAddr).Port

		h3PacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("h3 listener %d: %v", i, err)
		}
		h3Port := h3PacketConn.LocalAddr().(*net.UDPAddr).Port

		dnsPacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			h3PacketConn.Close()
			t.Fatalf("dns listener %d: %v", i, err)
		}
		dnsPort := dnsPacketConn.LocalAddr().(*net.UDPAddr).Port
		hostEndpoints[host] = testServerEndpoint{
			h1Port:  h1Port,
			h3Port:  h3Port,
			dnsPort: dnsPort,
		}

		exchangeListener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("exchange listener %d: %v", i, err)
		}
		exchangePort := exchangeListener.Addr().(*net.TCPAddr).Port
		hostToServicePorts := map[int]int{
			exchangePort: exchangePort,
		}

		settings := DefaultExchangeSettings()
		settings.ExchangeBufferSize = 0
		// Exercise the forward path with deterministic, reliable delivery: a
		// zero-size destination queue lets the owned sender worker wait up to
		// WriteTimeout. The Client callback remains a zero-wait handoff.
		settings.ForwardBufferSize = 0
		settings.ForwardTimeout = settings.WriteTimeout
		settings.ExchangeResidentTtl = sequenceIdleTimeout
		settings.ForwardIdleTimeout = sequenceIdleTimeout
		settings.FramerSettings.MaxMessageLen = framerMaxMessageLen
		if config.enableChaos {
			settings.ExchangeChaosSettings.ResidentShutdownPerSecond = 0.005
		}
		switch contractTest {
		case contractTestSymmetric, contractTestAsymmetric:
			settings.ForwardEnforceActiveContracts = true
		default:
			settings.ForwardEnforceActiveContracts = false
		}

		exchange := NewExchangeWithListeners(
			ctx,
			host,
			service,
			block,
			hostToServicePorts,
			routes,
			settings,
			map[int]net.Listener{exchangePort: exchangeListener},
		)
		exchanges[host] = exchange

		server, connectHandler := createServer(
			exchange,
			h3PacketConn,
			dnsPacketConn,
		)
		servers[host] = server
		connectHandlers = append(connectHandlers, connectHandler)
		defer server.Close()
		go server.Serve(listener)
	}

	defer func() {
		for _, connectHandler := range connectHandlers {
			connectHandler.Close()
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		for _, connectHandler := range connectHandlers {
			if !connectHandler.WaitForIdle(closeCtx) {
				t.Errorf("connect handlers did not finish during teardown")
				return
			}
		}
	}()

	randServer := func() (string, testServerEndpoint) {
		endpoints := slices.Collect(maps.Values(hostEndpoints))
		endpoint := endpoints[mathrand.Intn(len(endpoints))]
		return fmt.Sprintf("ws://127.0.0.1:%d", endpoint.h1Port), endpoint
	}
	// Every carrier generation uses the same hermetic endpoint policy. The DNS
	// pump destination is distinct from TLS SNI in production, but this fixture's
	// pump listener is the loopback DNS socket selected below.
	newPlatformSettings := func(
		endpoint testServerEndpoint,
		platformBudget *connect.PlatformTransportBudget,
	) *connect.PlatformTransportSettings {
		settings := connect.DefaultPlatformTransportSettings()
		settings.QuicTlsConfig.InsecureSkipVerify = true
		settings.H3Port = endpoint.h3Port
		settings.DnsPort = endpoint.dnsPort
		settings.DnsPumpHost = "127.0.0.1"
		settings.FramerSettings.MaxMessageLen = framerMaxMessageLen
		settings.PlatformTransportBudget = platformBudget
		return settings
	}

	maxMessageContentSize := ByteCount(0)
	for _, messageContentSize := range messageContentSizes {
		maxMessageContentSize = max(maxMessageContentSize, messageContentSize)
	}
	standardContractTransferByteCount := 4 * maxMessageContentSize
	if config.forceStream {
		// signal exchanges need at least 16k, and the contract is half filled
		standardContractTransferByteCount = max(standardContractTransferByteCount, 64*1024)
	}
	standardContractFillFraction := float32(0.5)

	clientStrategyA := connect.NewClientStrategyWithDefaults(ctx)
	clientStrategyB := connect.NewClientStrategyWithDefaults(ctx)

	// Hermetic p2p: the default WebRtcSettings gather ICE candidates on every
	// host interface and query live STUN servers. On a multihomed host that
	// is a per-attempt lottery — failed attempts of the ForceStream variants
	// showed thousands of cross-interface "no route to host" pion failures
	// and closed-agent churn filling the stall window, while passing attempts
	// showed almost none. Loopback-only, no STUN makes any p2p attempt
	// deterministic (a couple of loopback pairs) without disabling the p2p
	// machinery the stream paths exercise.
	hermeticWebRtc := func(s *connect.ClientSettings) {
		s.WebRtcSettings.Log = connect.NewNoopLogger()
		s.WebRtcSettings.IceServerUrls = nil
		s.WebRtcSettings.UseLoopbackOnlyIceInterfaces = true
	}

	// The out-of-band peer key fetcher is the contract-free identity source:
	// with contracts enabled the peer's key also arrives via the stored
	// contract, but contractTestNone has no contracts, and without ANY key
	// source the peer identity proof buffers forever — under
	// EncryptionModeRequired that is a correct fail-closed refusal (no
	// verified identity, no cipher, no data), which surfaced as 5/5
	// establishment timeouts in every contractTestNone+Encrypted variant
	// once the AllowFallback wiring ran Required here (Opportunistic ran
	// those directions plaintext and hid it). Production wires this fetcher
	// through the platform GetClientKey api; the closures late-bind because
	// the peer client is constructed after these settings are consumed.
	var clientA, clientB *connect.Client
	peerKeyFetcherFor := func(peer **connect.Client) func(connect.Id) func(context.Context) ([]byte, error) {
		return func(peerId connect.Id) func(context.Context) ([]byte, error) {
			return func(ctx context.Context) ([]byte, error) {
				return (*peer).ClientKeyManager().PublicKey(), nil
			}
		}
	}

	clientSettingsA := connect.DefaultClientSettings()
	hermeticWebRtc(clientSettingsA)
	clientSettingsA.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsA.SendBufferSettings.AckBufferSize = 0
	clientSettingsA.SendBufferSettings.AckTimeout = sequenceStateTimeout
	clientSettingsA.SendBufferSettings.MinResendInterval = minResendInterval
	clientSettingsA.ReceiveBufferSettings.GapTimeout = sequenceStateTimeout
	clientSettingsA.ReceiveBufferSettings.SequenceBufferSize = 0
	// ack per received message instead of coalescing. The production default
	// coalesces acks over a 10ms window; this test sets MinResendInterval=10ms
	// and 0-size buffers, so a coalesce window on the same order as the resend
	// interval makes ack timing nondeterministic. Per-message acks keep the
	// deadlock/ordering assertions deterministic.
	clientSettingsA.ReceiveBufferSettings.AckCompressTimeout = 0
	// clientSettingsA.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsA.ForwardBufferSettings.SequenceBufferSize = 0
	// disable scheduled network events
	clientSettingsA.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
	// set this low enough to test new contracts in the transfer
	clientSettingsA.SendBufferSettings.ContractFillFraction = standardContractFillFraction
	clientSettingsA.ContractManagerSettings.StandardContractTransferByteCount = standardContractTransferByteCount
	clientSettingsA.SendBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsA.ReceiveBufferSettings.IdleTimeout = sequenceStateTimeout
	clientSettingsA.ForwardBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsA.ControlPingTimeout = 30 * time.Second
	clientSettingsA.DefaultTransferOpts.ForceStream = config.forceStream
	// IdleTimeout=0 reaps the encryption session at refs==0 (no keep-alive),
	// deliberately — like 0-size buffers exposing deadlocks, it forces a fresh
	// session + handshake per burst so bugs in the restart -> plaintext ->
	// upgrade path surface instead of being hidden by session reuse.
	// (Production uses max(send,receive) idle; correctness must hold at 0.)
	// IdleTimeout=0 reaps the encryption session at refs==0 — a stress that
	// forces a fresh session + handshake per burst to exercise the
	// restart -> plaintext -> upgrade path. That rationale is
	// OPPORTUNISTIC-only: under Required there is no plaintext bridge, so a
	// per-burst reap makes every burst pay a full handshake under the entry
	// gate, and the peer's independent reap between bursts leaves it unable
	// to decrypt the next wrap — it nacks, the sealer re-handshakes, and the
	// cycle repeats every burst (a reap-nack-rehandshake thrash that only
	// Required feels; observed stalling ~1/3 of Required asymmetric attempts
	// under -race). Required instead uses a realistic idle timeout matching
	// production (DefaultClientSettings sets max(send,receive) idle), which
	// keeps the established cipher across bursts while still exercising a
	// fresh handshake at setup.
	encryptionSessionIdleTimeout := 0 * time.Second
	if config.enableEncryption && !config.allowFallback {
		encryptionSessionIdleTimeout = sequenceStateTimeout
	}
	if config.enableEncryption {
		clientSettingsA.EncryptionSettings.Mode = connect.EncryptionModeRequired
		if config.allowFallback {
			clientSettingsA.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
		}
		clientSettingsA.EncryptionSettings.IdleTimeout = encryptionSessionIdleTimeout
		if contractTest != contractTestAsymmetric {
			clientSettingsA.EncryptionSettings.EncryptionControlUseCompanion = false
		}
	}
	clientSettingsA.EncryptionSettings.NewPeerClientPublicKeyFetcher = peerKeyFetcherFor(&clientB)
	clientA = connect.NewClient(ctx, connect.Id(clientIdA), Testing_NewControllerOutOfBandControl(ctx, clientIdA, clientSettingsA.ContractManagerSettings), clientSettingsA)
	// routeManagerA := connect.NewRouteManager(clientA)
	// contractManagerA := connect.NewContractManagerWithDefaults(clientA)
	// clientA.Setup(routeManagerA, contractManagerA)
	// go clientA.Run()

	clientSettingsB := connect.DefaultClientSettings()
	hermeticWebRtc(clientSettingsB)
	clientSettingsB.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsB.SendBufferSettings.AckBufferSize = 0
	clientSettingsB.SendBufferSettings.AckTimeout = sequenceStateTimeout
	clientSettingsB.SendBufferSettings.MinResendInterval = minResendInterval
	clientSettingsB.ReceiveBufferSettings.GapTimeout = sequenceStateTimeout
	clientSettingsB.ReceiveBufferSettings.SequenceBufferSize = 0
	// ack per received message instead of coalescing (see clientSettingsA)
	clientSettingsB.ReceiveBufferSettings.AckCompressTimeout = 0
	// clientSettingsB.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsB.ForwardBufferSettings.SequenceBufferSize = 0
	// disable scheduled network events
	clientSettingsB.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
	// set this low enough to test new contracts in the transfer
	clientSettingsB.SendBufferSettings.ContractFillFraction = standardContractFillFraction
	clientSettingsB.ContractManagerSettings.StandardContractTransferByteCount = standardContractTransferByteCount
	clientSettingsB.SendBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsB.ReceiveBufferSettings.IdleTimeout = sequenceStateTimeout
	clientSettingsB.ForwardBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsB.ControlPingTimeout = 30 * time.Second
	clientSettingsB.DefaultTransferOpts.ForceStream = config.forceStream
	if config.enableEncryption {
		clientSettingsB.EncryptionSettings.Mode = connect.EncryptionModeRequired
		if config.allowFallback {
			clientSettingsB.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
		}
		clientSettingsB.EncryptionSettings.IdleTimeout = encryptionSessionIdleTimeout
		if contractTest != contractTestAsymmetric {
			clientSettingsB.EncryptionSettings.EncryptionControlUseCompanion = false
		}
	}
	clientSettingsB.EncryptionSettings.NewPeerClientPublicKeyFetcher = peerKeyFetcherFor(&clientA)
	clientB = connect.NewClient(ctx, connect.Id(clientIdB), Testing_NewControllerOutOfBandControl(ctx, clientIdB, clientSettingsB.ContractManagerSettings), clientSettingsB)
	// routeManagerB := connect.NewRouteManager(clientB)
	// contractManagerB := connect.NewContractManagerWithDefaults(clientB)
	// clientB.Setup(routeManagerB, contractManagerB)
	// go clientB.Run()

	networkIdA := server.NewId()
	networkNameA := "testConnectNetworkA"
	userIdA := server.NewId()
	deviceIdA := server.NewId()

	model.Testing_CreateNetwork(
		ctx,
		networkIdA,
		networkNameA,
		userIdA,
	)
	model.Testing_CreateDevice(
		ctx,
		networkIdA,
		deviceIdA,
		clientIdA,
		"a",
		"a",
	)

	networkIdB := server.NewId()
	networkNameB := "testConnectNetworkB"
	userIdB := server.NewId()
	deviceIdB := server.NewId()

	model.Testing_CreateNetwork(
		ctx,
		networkIdB,
		networkNameB,
		userIdB,
	)
	model.Testing_CreateDevice(
		ctx,
		networkIdB,
		deviceIdB,
		clientIdB,
		"b",
		"b",
	)

	receiveA := make(chan *Message, 16384)
	receiveB := make(chan *Message, 16384)

	decodeSimpleMessages := func(frames []*protocol.Frame) []SimpleMessage {
		messages := make([]SimpleMessage, 0, len(frames))
		for _, frame := range frames {
			message, err := connect.FromFrame(frame)
			if err != nil {
				panic(err)
			}
			if simpleMessage, ok := message.(*protocol.SimpleMessage); ok {
				messages = append(messages, SimpleMessage{
					messageIndex: simpleMessage.MessageIndex,
					messageCount: simpleMessage.MessageCount,
				})
			}
		}
		return messages
	}

	// printReceive := func(clientName string, frames []*protocol.Frame) {
	// 	for _, frame := range frames {
	// 		simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
	// 		if 0 < simpleMessage.MessageCount {
	// 			fmt.Printf("[%s] receive acked message %d\n", clientName, simpleMessage.MessageIndex)
	// 		} else {
	// 			fmt.Printf("[%s] receive nacked message %d\n", clientName, simpleMessage.MessageIndex)
	// 		}
	// 	}
	// }

	clientA.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
		// printReceive("a", frames)
		select {
		case receiveA <- &Message{
			sourceId:    source.SourceId,
			messages:    decodeSimpleMessages(frames),
			provideMode: peer.ProvideMode,
		}:
		default:
			panic(errors.New("Receive overflow."))
		}
	})

	clientB.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
		// printReceive("b", frames)
		select {
		case receiveB <- &Message{
			sourceId:    source.SourceId,
			messages:    decodeSimpleMessages(frames),
			provideMode: peer.ProvideMode,
		}:
		default:
			panic(errors.New("Receive overflow."))
		}
	})

	// attach transports
	guestMode := false
	isPro := false

	byJwtA := jwt.NewByJwt(
		networkIdA,
		userIdA,
		networkNameA,
		guestMode,
		isPro,
	).Client(deviceIdA, clientIdA)

	authA := &connect.ClientAuth{
		ByJwt: byJwtA.Sign(),
		// ClientId: clientIdA,
		InstanceId: connect.Id(clientAInstanceId),
		AppVersion: "0.0.0",
	}
	platformBudgetA := newTestConnectPlatformBudget()
	platformBudgetB := newTestConnectPlatformBudget()

	transportAs := []*connect.PlatformTransport{}
	for i := 0; i < transportCount; i += 1 {
		host, endpoint := randServer()
		settings := newPlatformSettings(endpoint, platformBudgetA)
		transportA := connect.NewPlatformTransportWithTargetMode(
			ctx,
			clientStrategyA,
			clientA.RouteManager(),
			host,
			authA,
			config.transportMode,
			settings,
		)
		transportAs = append(transportAs, transportA)
		// go transportA.Run(clientA.RouteManager())
	}

	byJwtB := jwt.NewByJwt(
		networkIdB,
		userIdB,
		networkNameB,
		guestMode,
		isPro,
	).Client(deviceIdB, clientIdB)

	authB := &connect.ClientAuth{
		ByJwt: byJwtB.Sign(),
		// ClientId: clientIdB,
		InstanceId: connect.Id(clientBInstanceId),
		AppVersion: "0.0.0",
	}

	transportBs := []*connect.PlatformTransport{}
	for i := 0; i < transportCount; i += 1 {
		host, endpoint := randServer()
		settings := newPlatformSettings(endpoint, platformBudgetB)
		transportB := connect.NewPlatformTransportWithTargetMode(
			ctx,
			clientStrategyB,
			clientB.RouteManager(),
			host,
			authB,
			config.transportMode,
			settings,
		)
		transportBs = append(transportBs, transportB)
		// go transportB.Run(clientB.RouteManager())
	}

	cleanupComplete := false
	cleanup := func() {
		if cleanupComplete {
			return
		}
		cleanupComplete = true
		closeCtx, closeCancel := context.WithTimeout(context.Background(), progressTimeout)
		defer closeCancel()
		clientA.Close()
		clientB.Close()
		if err := clientA.CloseAndWait(closeCtx); err != nil {
			t.Errorf("join client A: %v", err)
		}
		if err := clientB.CloseAndWait(closeCtx); err != nil {
			t.Errorf("join client B: %v", err)
		}
		if err := closeTestConnectPlatformTransports(closeCtx, transportAs); err != nil {
			t.Errorf("join client A transports: %v", err)
		}
		if err := closeTestConnectPlatformTransports(closeCtx, transportBs); err != nil {
			t.Errorf("join client B transports: %v", err)
		}
		for _, testServer := range servers {
			testServer.Close()
		}
		for _, exchange := range exchanges {
			exchange.Close()
		}
	}
	defer cleanup()

	if err := waitForTestConnectPlatformTransport(ctx, transportAs, progressTimeout); err != nil {
		t.Fatalf("client A initial platform transport: %v", err)
	}
	if err := waitForTestConnectPlatformTransport(ctx, transportBs, progressTimeout); err != nil {
		t.Fatalf("client B initial platform transport: %v", err)
	}

	initialTransferBalance := ByteCount(1024) * ByteCount(1024) * ByteCount(1024) * ByteCount(1024)

	subscriptionYearDuration := 365 * 24 * time.Hour

	balanceCodeA, err := model.CreateBalanceCode(
		ctx,
		initialTransferBalance,
		subscriptionYearDuration,
		0,
		"test-1",
		"",
		"",
	)
	connect.AssertEqual(t, nil, err)

	result, err := model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret:    balanceCodeA.Secret,
			NetworkId: networkIdA,
		},
		ctx,
	)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, nil, result.Error)

	balanceCodeB, err := model.CreateBalanceCode(
		ctx,
		initialTransferBalance,
		subscriptionYearDuration,
		0,
		"test-2",
		"",
		"",
	)
	connect.AssertEqual(t, nil, err)

	result, err = model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret:    balanceCodeB.Secret,
			NetworkId: networkIdB,
		},
		ctx,
	)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, nil, result.Error)

	// set up provide
	switch contractTest {
	case contractTestNone:
		clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
		clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

	case contractTestSymmetric:
		// FIXME
		// clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
		// clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

		provideModes := map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		}
		if config.forceStream {
			// passive peers in the WebRTC handshake send signals via
			// companion contracts (verified as ProvideMode_Stream), so
			// both sides need Stream provide enabled.
			provideModes[protocol.ProvideMode_Stream] = true
		}

		func() {
			ack := make(chan struct{})
			clientA.ContractManager().SetProvideModesWithAckCallback(
				provideModes,
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

		func() {
			ack := make(chan struct{})
			clientB.ContractManager().SetProvideModesWithAckCallback(
				provideModes,
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

	case contractTestAsymmetric:
		// FIXME
		// clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
		// clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

		// a->b is provide
		// b->a is a companion

		func() {
			ack := make(chan struct{})
			clientA.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
				map[protocol.ProvideMode]bool{},
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

		func() {
			ack := make(chan struct{})
			clientB.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
				map[protocol.ProvideMode]bool{
					protocol.ProvideMode_Network: true,
					protocol.ProvideMode_Public:  true,
				},
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

	}

	for _, messageContentSize := range messageContentSizes {
		// the message bytes are hex encoded which doubles the size
		messageContentBytes := make([]byte, messageContentSize/2)
		mathrand.Read(messageContentBytes)
		messageContent := hex.EncodeToString(messageContentBytes)
		connect.AssertEqual(t, int(messageContentSize), len(messageContent))

		ackA := make(chan error, 1024+burstM*2)
		ackB := make(chan error, 1024+burstM*2)

		for burstSize := 1; burstSize <= burstM; burstSize += 1 {
			for b := 0; b < 2; b += 1 {
				fmt.Printf(
					"[progress][%s] burstSize=%d b=%d\n",
					model.ByteCountHumanReadable(messageContentSize),
					burstSize,
					b,
				)

				if config.enableTransportReform {
					closeCtx, closeCancel := context.WithTimeout(ctx, progressTimeout)
					if err := closeTestConnectPlatformTransports(closeCtx, transportAs); err != nil {
						closeCancel()
						panic(fmt.Errorf("retire client A transports: %w", err))
					}
					closeCancel()
					transportAs = nil
					fmt.Printf("pause\n")
					select {
					case <-ctx.Done():
						return
					case <-time.After(randPauseTimeout()):
					}
					fmt.Printf("unpause\n")
					if 0 < newInstanceM && 0 == mathrand.Intn(newInstanceM) {
						fmt.Printf("new instance\n")
						clientAInstanceId = server.NewId()
					}
					authA = &connect.ClientAuth{
						ByJwt: byJwtA.Sign(),
						// ClientId: clientIdA,
						InstanceId: connect.Id(clientAInstanceId),
						AppVersion: "0.0.0",
					}
					for i := 0; i < transportCount; i += 1 {
						fmt.Printf("new transport a\n")
						host, endpoint := randServer()
						settings := newPlatformSettings(endpoint, platformBudgetA)
						transportA := connect.NewPlatformTransportWithTargetMode(
							ctx,
							clientStrategyA,
							clientA.RouteManager(),
							host,
							authA,
							config.transportMode,
							settings,
						)
						transportAs = append(transportAs, transportA)
						// go transportA.Run(clientA.RouteManager())
					}
					if err := waitForTestConnectPlatformTransport(ctx, transportAs, progressTimeout); err != nil {
						panic(fmt.Errorf("connect replacement client A transport: %w", err))
					}
				}

				go func() {
					for i := 0; i < burstSize; i += 1 {
						if 0 < i && i == burstSize/2 {
							fmt.Printf("pause\n")
							select {
							case <-ctx.Done():
								return
							case <-time.After(randPauseTimeout()):
							}
						}
						fmt.Printf("unpause\n")
						for j := 0; j < nackM; j += 1 {
							frame, err := connect.ToFrame(&protocol.SimpleMessage{
								MessageIndex: uint32(i*nackM + j),
								MessageCount: uint32(0),
								Content:      messageContent,
							}, connect.DefaultProtocolVersion)
							if err != nil {
								panic(err)
							}
							_, err = clientA.SendWithTimeoutDetailed(
								frame,
								connect.Id(clientIdB),
								func(err error) {
									if err != nil {
										panic(err)
									}
								},
								-1,
								connect.NoAck(),
							)
							if err != nil && !server.IsDoneError(err) {
								panic(fmt.Errorf("Could not send = %v", err))
							}
						}
						frame, err := connect.ToFrame(&protocol.SimpleMessage{
							MessageIndex: uint32(i),
							MessageCount: uint32(burstSize),
							Content:      messageContent,
						}, connect.DefaultProtocolVersion)
						if err != nil {
							panic(err)
						}
						_, err = clientA.SendWithTimeoutDetailed(
							frame,
							connect.Id(clientIdB),
							func(err error) {
								select {
								case ackA <- err:
								default:
									panic(errors.New("Ack overflow."))
								}
							},
							-1,
						)
						if err != nil && !server.IsDoneError(err) {
							panic(fmt.Errorf("Could not send = %v", err))
						}
					}
				}()

				// messagesToB := []*Message{}
				nackBCount := 0
				for i := 0; i < burstSize; {
					select {
					case message := <-receiveB:
						// messagesToB = append(messagesToB, message)

						// check in order
						for _, message := range message.messages {
							if 0 < message.messageCount {
								connect.AssertEqual(t, uint32(burstSize), message.messageCount)
								connect.AssertEqual(t, uint32(i), message.messageIndex)
								i += 1
							} else {
								nackBCount += 1
							}
						}
					case <-time.After(progressTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				endTime := time.Now().Add(1 * time.Second)
				for nackBCount < nackM*burstSize {
					timeout := endTime.Sub(time.Now())
					if timeout <= 0 {
						break
					}
					select {
					case message := <-receiveB:
						// messagesToB = append(messagesToB, message)

						// check in order
						for _, message := range message.messages {
							if 0 < message.messageCount {
								t.Fatal("Unexpected ack message.")
							} else {
								nackBCount += 1
							}
						}
					case <-time.After(timeout):
					}
				}
				if nackBCount != nackM*burstSize {
					fmt.Printf("B dropped nacks: %d <> %d\n", nackBCount, nackM*burstSize)
					nackDroppedByteCount += messageContentSize * ByteCount(nackM*burstSize-nackBCount)
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <-ackA:
						connect.AssertEqual(t, err, nil)
					case <-time.After(progressTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				select {
				case <-ackA:
					panic(errors.New("Too many acks."))
				default:
				}
				// check in order
				// for i, message := range messagesToB {
				// 	for _, frame := range message.frames {
				// 		simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
				// 		connect.AssertEqual(t, uint32(i), simpleMessage.MessageIndex)
				// 	}
				// }

				if config.enableTransportReform {
					closeCtx, closeCancel := context.WithTimeout(ctx, progressTimeout)
					if err := closeTestConnectPlatformTransports(closeCtx, transportBs); err != nil {
						closeCancel()
						panic(fmt.Errorf("retire client B transports: %w", err))
					}
					closeCancel()
					transportBs = nil
					fmt.Printf("pause\n")
					select {
					case <-ctx.Done():
						return
					case <-time.After(randPauseTimeout()):
					}
					fmt.Printf("unpause\n")
					if 0 < newInstanceM && 0 == mathrand.Intn(newInstanceM) {
						fmt.Printf("new instance\n")
						clientBInstanceId = server.NewId()
					}
					authB = &connect.ClientAuth{
						ByJwt: byJwtB.Sign(),
						// ClientId: clientIdB,
						InstanceId: connect.Id(clientBInstanceId),
						AppVersion: "0.0.0",
					}
					for i := 0; i < transportCount; i += 1 {
						fmt.Printf("new transport b\n")
						host, endpoint := randServer()
						settings := newPlatformSettings(endpoint, platformBudgetB)
						transportB := connect.NewPlatformTransportWithTargetMode(
							ctx,
							clientStrategyB,
							clientB.RouteManager(),
							host,
							authB,
							config.transportMode,
							settings,
						)
						transportBs = append(transportBs, transportB)
						// go transportB.Run(clientB.RouteManager())
					}
					if err := waitForTestConnectPlatformTransport(ctx, transportBs, progressTimeout); err != nil {
						panic(fmt.Errorf("connect replacement client B transport: %w", err))
					}
				}

				go func() {
					for i := 0; i < burstSize; i += 1 {
						if 0 < i && i == burstSize/2 {
							fmt.Printf("pause\n")
							select {
							case <-ctx.Done():
								return
							case <-time.After(randPauseTimeout()):
							}
						}
						fmt.Printf("unpause\n")
						for j := 0; j < nackM; j += 1 {
							opts := []any{
								connect.NoAck(),
							}
							if contractTest == contractTestAsymmetric {
								opts = append(opts, connect.CompanionContract())
							}

							frame, err := connect.ToFrame(&protocol.SimpleMessage{
								MessageIndex: uint32(i*nackM + j),
								MessageCount: uint32(0),
								Content:      messageContent,
							}, connect.DefaultProtocolVersion)
							if err != nil {
								panic(err)
							}
							_, err = clientB.SendWithTimeoutDetailed(
								frame,
								connect.Id(clientIdA),
								func(err error) {
									if err != nil {
										panic(err)
									}
								},
								-1,
								opts...,
							)
							if err != nil && !server.IsDoneError(err) {
								panic(fmt.Errorf("Could not send = %v", err))
							}
						}
						opts := []any{}
						if contractTest == contractTestAsymmetric {
							opts = append(opts, connect.CompanionContract())
						}
						frame, err := connect.ToFrame(&protocol.SimpleMessage{
							MessageIndex: uint32(i),
							MessageCount: uint32(burstSize),
							Content:      messageContent,
						}, connect.DefaultProtocolVersion)
						if err != nil {
							panic(err)
						}
						_, err = clientB.SendWithTimeoutDetailed(
							frame,
							connect.Id(clientIdA),
							func(err error) {
								select {
								case ackB <- err:
								default:
									panic(errors.New("Ack overflow."))
								}
							},
							-1,
							opts...,
						)
						if err != nil && !server.IsDoneError(err) {
							panic(fmt.Errorf("Could not send = %v", err))
						}
					}
				}()

				// 	sendToA(burstSize - 1)
				// }()
				// messagesToA := []*Message{}
				nackACount := 0
				for i := 0; i < burstSize; {
					select {
					case message := <-receiveA:
						// messagesToB = append(messagesToB, message)

						// check in order
						for _, message := range message.messages {
							if 0 < message.messageCount {
								connect.AssertEqual(t, uint32(burstSize), message.messageCount)
								connect.AssertEqual(t, uint32(i), message.messageIndex)
								i += 1
							} else {
								nackACount += 1
							}
						}
					case <-time.After(progressTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				endTime = time.Now().Add(1 * time.Second)
				for nackACount < nackM*burstSize {
					timeout := endTime.Sub(time.Now())
					if timeout <= 0 {
						break
					}
					select {
					case message := <-receiveA:
						// messagesToB = append(messagesToB, message)

						// check in order
						for _, message := range message.messages {
							if 0 < message.messageCount {
								t.Fatal("Unexpected ack message.")
							} else {
								nackACount += 1
							}
						}
					case <-time.After(timeout):
					}
				}
				if nackACount != nackM*burstSize {
					fmt.Printf("A dropped nacks: %d <> %d\n", nackACount, nackM*burstSize)
					nackDroppedByteCount += messageContentSize * ByteCount(nackM*burstSize-nackACount)
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <-ackB:
						connect.AssertEqual(t, err, nil)
					case <-time.After(progressTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				select {
				case <-ackB:
					panic(errors.New("Too many acks."))
				default:
				}
				// check in order
				// for i, message := range messagesToA {
				// 	for _, frame := range message.frames {
				// 		simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
				// 		connect.AssertEqual(t, uint32(i), simpleMessage.MessageIndex)
				// 	}
				// }

				if config.enableEncryption || config.forceStream {
					// flush EncryptedControl messages and p2p signal messages
					controlMessageCount := func(messageTypes []protocol.MessageType) int {
						count := 0
						for _, messageType := range messageTypes {
							switch messageType {
							case protocol.MessageType_TransferEncryptedControl:
								count += 1
							}
						}
						return count
					}
					for {
						_, _, sequenceIdA, resendMessageTypesA := clientA.ResendQueueSizeAndMessageTypes(connect.Id(clientIdB), connect.MultiHopId{}, false, false)
						_, _, sequenceIdB, resendMessageTypesB := clientB.ResendQueueSizeAndMessageTypes(connect.Id(clientIdA), connect.MultiHopId{}, false, false)
						_, _, receiveMessageTypesA := clientA.ReceiveQueueSizeAndMessageTypes(connect.DestinationId(connect.Id(clientIdB)), sequenceIdB)
						_, _, receiveMessageTypesB := clientB.ReceiveQueueSizeAndMessageTypes(connect.DestinationId(connect.Id(clientIdA)), sequenceIdA)
						count := 0
						count += controlMessageCount(resendMessageTypesA)
						count += controlMessageCount(resendMessageTypesB)
						count += controlMessageCount(receiveMessageTypesA)
						count += controlMessageCount(receiveMessageTypesB)
						if count == 0 {
							break
						}
						fmt.Printf("Waiting for %d control messages to settle\n", count)
						select {
						case <-time.After(10 * time.Second):
						}
					}
				}

				resendItemCountA, resendItemByteCountA, sequenceIdA, resendMessageTypesA := clientA.ResendQueueSizeAndMessageTypes(connect.Id(clientIdB), connect.MultiHopId{}, false, false)
				connect.AssertEqual(t, resendMessageTypesA, nil)
				connect.AssertEqual(t, resendItemCountA, 0)
				connect.AssertEqual(t, resendItemByteCountA, ByteCount(0))

				resendItemCountB, resentItemByteCountB, sequenceIdB, resendMessageTypesB := clientB.ResendQueueSizeAndMessageTypes(connect.Id(clientIdA), connect.MultiHopId{}, false, false)
				connect.AssertEqual(t, resendMessageTypesB, nil)
				connect.AssertEqual(t, resendItemCountB, 0)
				connect.AssertEqual(t, resentItemByteCountB, ByteCount(0))

				receiveItemCountA, receiveItemByteCountA, receiveMessageTypesA := clientA.ReceiveQueueSizeAndMessageTypes(connect.DestinationId(connect.Id(clientIdB)), sequenceIdB)
				connect.AssertEqual(t, receiveMessageTypesA, nil)
				connect.AssertEqual(t, receiveItemCountA, 0)
				connect.AssertEqual(t, receiveItemByteCountA, ByteCount(0))

				receiveItemCountB, receiveItemByteCountB, receiveMessageTypesB := clientB.ReceiveQueueSizeAndMessageTypes(connect.DestinationId(connect.Id(clientIdA)), sequenceIdA)
				connect.AssertEqual(t, receiveMessageTypesB, nil)
				connect.AssertEqual(t, receiveItemCountB, 0)
				connect.AssertEqual(t, receiveItemByteCountB, ByteCount(0))
			}
		}
	}

	// clientA.Cancel()
	// clientB.Cancel()

	// close(receiveA)
	// close(receiveB)

	select {
	case <-time.After(5 * time.Second):
	}

	clientA.Flush()
	clientB.Flush()

	select {
	case <-time.After(5 * time.Second):
	}

	cleanup()

	connect.AssertEqual(
		t,
		ByteCount(0),
		clientA.ContractManager().LocalStats().ContractOpenByteCount(),
	)
	connect.AssertEqual(
		t,
		ByteCount(0),
		clientB.ContractManager().LocalStats().ContractOpenByteCount(),
	)

	// these contracts were queued in the contract manager
	// and should be partially closed by the source.
	// The source's close-contract control messages land asynchronously (with
	// retries under chaos transport churn), so a fixed settle can race a close
	// that is still in flight — poll until every remaining partial close is a
	// tolerated party (checkpoint/source, closed here) or the other side's
	// close completes the contract. A close that never lands still fails at
	// the deadline.
	var contractIdPartialClosePartiesAToB map[server.Id]model.ContractParty
	var contractIdPartialClosePartiesBToA map[server.Id]model.ContractParty
	partialCloseDeadline := time.Now().Add(30 * time.Second)
	for {
		contractIdPartialClosePartiesAToB = model.GetOpenContractIdsWithPartialClose(ctx, clientIdA, clientIdB)
		contractIdPartialClosePartiesBToA = model.GetOpenContractIdsWithPartialClose(ctx, clientIdB, clientIdA)

		// note ContractPartySource is for contracts that were queued up and never used
		for contractId, party := range contractIdPartialClosePartiesAToB {
			if party == model.ContractPartyCheckpoint || party == model.ContractPartySource {
				model.CloseContract(ctx, contractId, clientIdB, 0, false)
				delete(contractIdPartialClosePartiesAToB, contractId)
			} else {
				fmt.Printf("A->B PARTY: %s %s\n", contractId, party)
			}
		}
		for contractId, party := range contractIdPartialClosePartiesBToA {
			if party == model.ContractPartyCheckpoint || party == model.ContractPartySource {
				model.CloseContract(ctx, contractId, clientIdA, 0, false)
				delete(contractIdPartialClosePartiesBToA, contractId)
			} else {
				fmt.Printf("B->A PARTY: %s %s\n", contractId, party)
			}
		}

		if len(contractIdPartialClosePartiesAToB) == 0 && len(contractIdPartialClosePartiesBToA) == 0 {
			break
		}
		if partialCloseDeadline.Before(time.Now()) {
			break
		}
		select {
		case <-time.After(500 * time.Millisecond):
		}
	}

	connect.AssertEqual(t, len(contractIdPartialClosePartiesAToB), 0)
	connect.AssertEqual(t, len(contractIdPartialClosePartiesBToA), 0)

	// FIXME what are these other contracts?
	// for _, party := range contractIdPartialClosePartiesAToB {
	// 	connect.AssertEqual(t, model.ContractPartySource, party)
	// }
	// for _, party := range contractIdPartialClosePartiesBToA {
	// 	connect.AssertEqual(t, model.ContractPartySource, party)
	// }

	flushedContractIdsA := []server.Id{}
	for _, contractId := range clientA.ContractManager().Flush(false) {
		flushedContractIdsA = append(flushedContractIdsA, server.Id(contractId))
	}
	flushedContractIdsB := []server.Id{}
	for _, contractId := range clientB.ContractManager().Flush(false) {
		flushedContractIdsB = append(flushedContractIdsB, server.Id(contractId))
	}

	// SendSequence now flushes pending contracts when closed
	// it's normal to have one pending contract per sequence due the predictive queueing of contracts
	connect.AssertEqual(t, len(flushedContractIdsA), 0)
	connect.AssertEqual(t, len(flushedContractIdsB), 0)

	// if e := len(contractIdPartialClosePartiesAToB) - len(flushedContractIdsA); 1 < e {
	// 	connect.AssertEqual(t, len(flushedContractIdsA), len(contractIdPartialClosePartiesAToB))
	// }
	// for _, contractId := range flushedContractIdsA {
	// 	party, ok := contractIdPartialClosePartiesAToB[contractId]
	// 	connect.AssertEqual(t, true, ok)
	// 	connect.AssertEqual(t, model.ContractPartySource, party)
	// }
	// if e := len(contractIdPartialClosePartiesBToA) - len(flushedContractIdsB); 1 < e {
	// 	connect.AssertEqual(t, len(flushedContractIdsB), len(contractIdPartialClosePartiesBToA))
	// }
	// for _, contractId := range flushedContractIdsB {
	// 	party, ok := contractIdPartialClosePartiesBToA[contractId]
	// 	connect.AssertEqual(t, true, ok)
	// 	connect.AssertEqual(t, model.ContractPartySource, party)
	// }

	localStatsA := clientA.ContractManager().LocalStats()
	localStatsB := clientB.ContractManager().LocalStats()

	byteCountA := (localStatsA.ContractCloseByteCount + localStatsB.ReceiveContractCloseByteCount) / 2
	byteCountB := (localStatsB.ContractCloseByteCount + localStatsA.ReceiveContractCloseByteCount) / 2

	fmt.Printf("Net nack dropped bytes = %d\n", nackDroppedByteCount)
	byteCountEquivalent := func(a ByteCount, b ByteCount) {
		// because of resets and dropped nacks there may be some small discrepancy
		// (see note at top about messages closer to the contract size getting dropped more frequenctly)
		threshold := 64*model.Mib + nackDroppedByteCount
		e := a - b
		if e < -threshold || threshold < e {
			connect.AssertEqual(t, a, b)
		}
	}

	switch contractTest {
	case contractTestNone:
		// no balance deducted
		byteCountEquivalent(
			initialTransferBalance,
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance,
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)
	case contractTestSymmetric:
		// a and b each have 1x balance deducted

		byteCountEquivalent(
			initialTransferBalance-
				byteCountA-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesAToB)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance-
				byteCountB-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesBToA)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)

	case contractTestAsymmetric:
		// a has 2x balance deducted and b has no balance deducted

		byteCountEquivalent(
			initialTransferBalance-
				byteCountA-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesAToB))-
				byteCountB-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesBToA)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance,
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)
	}
}

func printAllStacks() {
	b := make([]byte, 128*1024*1024)
	n := runtime.Stack(b, true)
	fmt.Printf("ALL STACKS: %s\n", string(b[0:n]))
}

type controllerOutOfBandControl struct {
	ctx                     context.Context
	clientId                server.Id
	contractManagerSettings *connect.ContractManagerSettings
}

func Testing_NewControllerOutOfBandControl(
	ctx context.Context,
	clientId server.Id,
	contractManagerSettings *connect.ContractManagerSettings,
) connect.OutOfBandControl {
	return &controllerOutOfBandControl{
		ctx:                     ctx,
		clientId:                clientId,
		contractManagerSettings: contractManagerSettings,
	}
}

func (self *controllerOutOfBandControl) SendControl(frames []*protocol.Frame, callback func(resultFrames []*protocol.Frame, err error)) {
	// recover panics (e.g. a canceled ctx raised out of the db layer at test
	// teardown) instead of crashing the test binary
	go server.HandleError(func() {
		defer func() {
			for _, frame := range frames {
				connect.MessagePoolReturn(frame.MessageBytes)
			}
		}()
		resultFrames, err := controller.ConnectControlFrames(self.ctx, self.clientId, frames, self.contractManagerSettings)
		defer func() {
			for _, frame := range resultFrames {
				connect.MessagePoolReturn(frame.MessageBytes)
			}
		}()
		if callback != nil {
			callback(resultFrames, err)
		}
	})
}
