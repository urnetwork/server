package model

// What api's internal gate projection routes read beside ConfigLogSequencer.Read (ACL-M4-05; QD-680): one base's
// ProjectionRows by their hash, verbatim, from projrows/, which a gate host holds to that hash itself. A carrier's
// read: nothing here judges the rows, as nothing in Read judges the entries.

import (
	"context"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// One base's rows by their hash, verbatim, and whether the store holds them.
func (self *ConfigLogSequencer) ReadRows(ctx context.Context, networkId server.Id, rowsSha256 []byte) ([]byte, bool, error) {
	return configLogGet(ctx, self.cluster, sidecar.ConfigRowsKey(sidecar.Id(networkId), rowsSha256))
}
