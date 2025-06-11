package main

import (
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestTransportTls(t *testing.T) {

	// create from config

	// get various certs that should exist

	transportTls, err := NewTransportTlsFromConfig()
	assert.Equal(t, err, nil)

	tlsConfig, err := transportTls.GetTlsConfig("ur.network")
	assert.Equal(t, err, nil)
	assert.NotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("bringyour.com")
	assert.Equal(t, err, nil)
	assert.NotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("main-connect.ur.network")
	assert.Equal(t, err, nil)
	assert.NotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("main-connect.bringyour.com")
	assert.Equal(t, err, nil)
	assert.NotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("foo.ur.network")
	assert.NotEqual(t, err, nil)
	assert.Equal(t, tlsConfig, nil)

}
