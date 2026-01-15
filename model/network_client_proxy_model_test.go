package model

import (
	mathrand "math/rand"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

func TestSignProxyId(t *testing.T) {

	proxyId := server.NewId()

	signedProxyId := SignProxyId(proxyId)

	// ensure the signed proxy id can be used as a hostname part
	assert.Equal(t, len(signedProxyId) < 63, true)

	proxyId2, err := ParseSignedProxyId(signedProxyId)
	assert.Equal(t, err, nil)
	assert.Equal(t, proxyId, proxyId2)

	// try fuzzed values and make sure they don't parse
	for range 32 {
		b := []byte(signedProxyId)
		var i int
		var j int
		for {
			i = mathrand.Intn(16)
			j = (i + 1 + mathrand.Intn(15)) % 16
			if b[i] != b[j] {
				break
			}
		}
		assert.NotEqual(t, b[i], b[j])
		b[i], b[j] = b[j], b[i]

		_, err := ParseSignedProxyId(string(b))
		assert.NotEqual(t, err, nil)
	}

}

func TestSignProxyIdHosts(t *testing.T) {

	host := "06ds11j8v14jm3kuoig10h95j8tt7gdefb33jnap47jbiq1paapoheo8e8.connect.bringyour.com"
	hostProxyId := strings.SplitN(host, ".", 2)[0]
	_, err := ParseSignedProxyId(hostProxyId)
	assert.Equal(t, err, nil)

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
