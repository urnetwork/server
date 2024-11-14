package server

import (
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestParseClientAddress(t *testing.T) {
	ip, port, err := ParseClientAddress("[2001:470:99:57:e643:4bff:fe23:a343]:443")
	assert.Equal(t, ip, "2001:470:99:57:e643:4bff:fe23:a343")
	assert.Equal(t, port, 443)
	assert.Equal(t, err, nil)

	ip, port, err = ParseClientAddress("127.0.0.1:443")
	assert.Equal(t, ip, "127.0.0.1")
	assert.Equal(t, port, 443)
	assert.Equal(t, err, nil)

	ip, port, err = ParseClientAddress("fd00:6a4f:a007:15da::1:40704")
	assert.Equal(t, ip, "fd00:6a4f:a007:15da::1")
	assert.Equal(t, port, 40704)
	assert.Equal(t, err, nil)

	ip, port, err = ParseClientAddress(":443")
	assert.NotEqual(t, err, nil)
}
