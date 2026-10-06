//go:generate go run .

// Command connectgen writes the connect integration matrix.
//
// The matrix is a clean cross product and was 93 hand-written wrappers, each
// existing only to set a few booleans. Expressing it as axes is what makes the
// extender variants affordable: doubling the matrix is one flag here rather
// than 93 more functions to write and keep in step.
//
//	31 base variants x 3 encryption modes x 2 extender modes
//
// The output is ordinary top-level tests, one per case, because that is what
// connect/CODESTYLE.md asks for and what makes a single case selectable with
// -run. A table loop cannot do either: server.TestEnv.Run ends an exhausted
// case with t.FailNow, which would abort the loop and hide every case after
// the first failure.
//
// Edit the axes below and re-run; never edit the generated file.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
)

const generatedPath = "../connect_generated_test.go"

// A base variant: everything except the encryption and extender axes.
type baseCase struct {
	name     string
	contract string
	// mode is the transport mode literal, empty for the default (h1).
	mode            string
	chaos           bool
	transportReform bool
	nack            bool
	newInstance     bool
	forceStream     bool
}

// The 31 base variants, transcribed from the hand-written matrix they replace.
var baseCases = []baseCase{
	{name: "ConnectAuto", contract: "contractTestNone", mode: "connect.TransportModeAuto", transportReform: true, nack: true},
	{name: "ConnectDns", contract: "contractTestNone", mode: "connect.TransportModeH3Dns", transportReform: true, nack: true},
	{name: "ConnectDnsPump", contract: "contractTestNone", mode: "connect.TransportModeH3DnsPump", transportReform: true, nack: true},
	{name: "ConnectH1", contract: "contractTestNone", mode: "connect.TransportModeH1", transportReform: true, nack: true},
	{name: "ConnectH3", contract: "contractTestNone", mode: "connect.TransportModeH3", transportReform: true, nack: true},
	{name: "ConnectNoNack", contract: "contractTestNone", transportReform: true},
	{name: "ConnectNoTransportReform", contract: "contractTestNone", nack: true},
	{name: "ConnectNoTransportReformNoNack", contract: "contractTestNone"},
	{name: "ConnectNoTransportReformWithNewInstance", contract: "contractTestNone", nack: true, newInstance: true},
	{name: "ConnectWithAsymmetricContracts", contract: "contractTestAsymmetric", transportReform: true, nack: true},
	{name: "ConnectWithAsymmetricContractsNoNack", contract: "contractTestAsymmetric"},
	{name: "ConnectWithAsymmetricContractsWithChaos", contract: "contractTestAsymmetric", chaos: true, transportReform: true, nack: true},
	{name: "ConnectWithAsymmetricContractsWithChaosNoNack", contract: "contractTestAsymmetric", chaos: true, transportReform: true},
	{name: "ConnectWithAsymmetricContractsWithChaosWithNewInstance", contract: "contractTestAsymmetric", chaos: true, transportReform: true, nack: true, newInstance: true},
	{name: "ConnectWithAsymmetricContractsWithForceStream", contract: "contractTestAsymmetric", transportReform: true, nack: true, forceStream: true},
	{name: "ConnectWithAsymmetricContractsWithNewInstance", contract: "contractTestAsymmetric", transportReform: true, nack: true, newInstance: true},
	{name: "ConnectWithChaosH1", contract: "contractTestNone", mode: "connect.TransportModeH1", chaos: true, transportReform: true, nack: true},
	{name: "ConnectWithChaosH3", contract: "contractTestNone", mode: "connect.TransportModeH3", chaos: true, transportReform: true, nack: true},
	{name: "ConnectWithChaosNoNack", contract: "contractTestNone", chaos: true, transportReform: true},
	{name: "ConnectWithChaosNoTransportReform", contract: "contractTestNone", chaos: true, nack: true},
	{name: "ConnectWithChaosNoTransportReformNoNack", contract: "contractTestNone", chaos: true},
	{name: "ConnectWithChaosNoTransportReformWithNewInstance", contract: "contractTestNone", chaos: true, nack: true, newInstance: true},
	{name: "ConnectWithChaosWithNewInstance", contract: "contractTestNone", chaos: true, transportReform: true, nack: true, newInstance: true},
	{name: "ConnectWithNewInstance", contract: "contractTestNone", transportReform: true, nack: true, newInstance: true},
	{name: "ConnectWithSymmetricContracts", contract: "contractTestSymmetric", transportReform: true, nack: true},
	{name: "ConnectWithSymmetricContractsNoNack", contract: "contractTestSymmetric", transportReform: true},
	{name: "ConnectWithSymmetricContractsWithChaos", contract: "contractTestSymmetric", chaos: true, transportReform: true, nack: true},
	{name: "ConnectWithSymmetricContractsWithChaosNoNack", contract: "contractTestSymmetric", chaos: true, transportReform: true},
	{name: "ConnectWithSymmetricContractsWithChaosWithNewInstance", contract: "contractTestSymmetric", chaos: true, transportReform: true, nack: true, newInstance: true},
	{name: "ConnectWithSymmetricContractsWithForceStream", contract: "contractTestSymmetric", transportReform: true, nack: true, forceStream: true},
	{name: "ConnectWithSymmetricContractsWithNewInstance", contract: "contractTestSymmetric", transportReform: true, nack: true, newInstance: true},
}

// The encryption axis. The name suffix is what the hand-written tests used, so
// every existing test keeps its name.
type encryptionMode struct {
	suffix        string
	encryption    bool
	allowFallback bool
}

var encryptionModes = []encryptionMode{
	{suffix: ""},
	{suffix: "Encrypted", encryption: true},
	{suffix: "EncryptedAllowFallback", encryption: true, allowFallback: true},
}

// The extender axis.
//
// A *WithExtender case routes every client connection through an in-process
// extender instead of dialing the operator directly, with the direct
// strategies turned off so the extender is the only way through.
type extenderMode struct {
	suffix   string
	extender bool
}

var extenderModes = []extenderMode{
	{suffix: ""},
	{suffix: "WithExtender", extender: true},
}

// extenderCapable reports whether a base variant can route through an
// extender.
//
// Every variant can, now that an extender relays datagrams as well as streams
// (ExtenderHeader.Datagram). Before that an h3 carrier could not traverse one
// at all: an extender yields a reliable byte stream per carrier and cannot see
// inside the inner tls, so it had nothing to reframe and quic had no packet
// boundaries to run on.
//
// Kept as a function rather than deleted because it is where the answer goes
// if some future carrier cannot be extended.
func (self baseCase) extenderCapable() bool {
	return true
}

// generate returns the formatted file and how many cases it holds.
func generate() ([]byte, int, error) {
	var out bytes.Buffer
	fmt.Fprint(&out, header)

	count := 0
	for _, base := range baseCases {
		for _, encryption := range encryptionModes {
			for _, extender := range extenderModes {
				if extender.extender && !base.extenderCapable() {
					continue
				}
				fmt.Fprint(&out, testFunc(base, encryption, extender))
				count += 1
			}
		}
	}
	fmt.Fprintf(&out, "\n// %d cases.\n", count)

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, 0, fmt.Errorf("format: %w", err)
	}
	return formatted, count, nil
}

func main() {
	formatted, count, err := generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "connectgen: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(generatedPath, formatted, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "connectgen: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "connectgen: wrote %d cases to %s\n", count, generatedPath)
}

func testFunc(base baseCase, encryption encryptionMode, extender extenderMode) string {
	name := "Test" + base.name + encryption.suffix + extender.suffix

	fields := []string{}
	add := func(field string, set bool) {
		if set {
			fields = append(fields, field+": true")
		}
	}
	add("enableChaos", base.chaos)
	add("enableTransportReform", base.transportReform)
	add("enableNack", base.nack)
	add("enableNewInstance", base.newInstance)
	add("forceStream", base.forceStream)
	if base.mode != "" {
		fields = append(fields, "transportMode: "+base.mode)
	}
	add("enableEncryption", encryption.encryption)
	add("allowFallback", encryption.allowFallback)
	add("enableExtender", extender.extender)

	var body bytes.Buffer
	fmt.Fprintf(&body, "func %s(t *testing.T) {\n", name)
	fmt.Fprintf(&body, "\tserver.DefaultTestEnv().Run(t, func(t testing.TB) {\n")
	fmt.Fprintf(&body, "\t\tfmt.Printf(\"[progress]start %s\\n\")\n", name)
	fmt.Fprintf(&body, "\t\ttestConnect(t, %s, &testConnectConfig{\n", base.contract)
	for _, field := range fields {
		fmt.Fprintf(&body, "\t\t\t%s,\n", field)
	}
	fmt.Fprintf(&body, "\t\t})\n\t})\n}\n\n")
	return body.String()
}

const header = `// Code generated by connectgen. DO NOT EDIT.

package connect

import (
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

`
