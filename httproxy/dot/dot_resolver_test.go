package dot_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urnetwork/server/httproxy/dot"
)

func TestDnsCache_Resolve(t *testing.T) {
	c := dot.NewDNSCache()

	res, err := c.Resolve(context.Background(), "grafana.prod.ur.network")
	require.NoError(t, err)

	slices.Sort(res)
	require.Equal(t, []string{"65.49.70.83", "65.49.70.85"}, res)

	res, err = c.Resolve(context.Background(), "grafana.prod.ur.network")
	require.NoError(t, err)

	slices.Sort(res)
	require.Equal(t, []string{"65.49.70.83", "65.49.70.85"}, res)

	require.Equal(t, uint64(1), c.LookupsPeformed.Load())

}
