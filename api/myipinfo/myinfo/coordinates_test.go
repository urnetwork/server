package myinfo_test

import (
	"testing"

	"bringyour.com/service/api/myipinfo/myinfo"
	"github.com/stretchr/testify/require"
)

func TestParseCoordinates(t *testing.T) {
	c, err := myinfo.ParseCoordinates("45.8399,-119.7006")
	require.NoError(t, err)

	require.InDelta(t, 45.8399, c.Latitude, 1e-8)
	require.Equal(t, -119.7006, c.Longitude, 1e-8)
}