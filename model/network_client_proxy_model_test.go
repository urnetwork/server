package model

import (
	mathrand "math/rand"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

func TestSignProxyId(t *testing.T) {

	proxyId := server.NewId()

	signedProxyId := SignProxyId(proxyId)

	// ensure the signed proxy id can be used as a hostname part
	assert.Equal(t, len(signedProxyId) < 63, true)

	proxyId2, err := ParseProxyId(signedProxyId)
	assert.Equal(t, err, nil)
	assert.Equal(t, proxyId, proxyId2)

	// try fuzzed values and make sure they don't parse
	for range 32 {
		b := []byte(signedProxyId)
		i := mathrand.Intn(16)
		j := (i + 1 + mathrand.Intn(15)) % 16
		b[i], b[j] = b[j], b[i]

		proxyId3, err := ParseProxyId(string(b))
		assert.NotEqual(t, err, nil)
	}
}

// FIXME
/*
func TestProxyDeviceConfig() {

	// create config

	// load config for proxy id
	// load config for client

	// load connection for proxy id
	// load conenction for client

	CreateProxyDeviceConfig(&ProxyDeviceConfig{
		ClientId: clientId,
	})

}
*/
