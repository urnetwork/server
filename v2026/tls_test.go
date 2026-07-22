package server

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestTransportTls(t *testing.T) {

	// create from config

	// get various certs that should exist

	settings := &TransportTlsSettings{
		EnableSelfSign: false,
	}
	transportTls, err := NewTransportTlsFromConfig(settings)
	connect.AssertEqual(t, err, nil)

	tlsConfig, err := transportTls.GetTlsConfig("ur.network")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("bringyour.com")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("main-connect.ur.network")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("main-connect.bringyour.com")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("foo.ur.network")
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, tlsConfig, nil)

}

func TestTransportTlsSelfSign(t *testing.T) {

	settings := &TransportTlsSettings{
		EnableSelfSign: true,
	}
	transportTls, err := NewTransportTlsFromConfig(settings)
	connect.AssertEqual(t, err, nil)

	tlsConfig, err := transportTls.GetTlsConfig("ur.network")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("bringyour.com")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("main-connect.ur.network")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("main-connect.bringyour.com")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("foo.ur.network")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("foo2.bar.ur.network")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

}
