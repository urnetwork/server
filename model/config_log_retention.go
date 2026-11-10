package model

// §7.9's retention row at api: each network's entries are kept from the
// retention point, min(the previous base, the newest base's folded_through +
// 1), on for the network's life; older entries, and the rows no retained base
// names, are deleted 30 days after the newer base, a pass's work bounded and
// every pass resumable, since a pass only deletes what is past the point and a
// later pass finds what an earlier left.
//
//	point    the newest base at least KeepAfterNewerBase old decides: its
//	         retention point, from its own folded_through and the base before
//	         it; the first base keeps everything (folded_through 0)
//	entries  proj/<network_id>/<n>/_ for 0 < n below the point; entry 0, the
//	         Signed Genesis, is kept for the network's life
//	rows     projrows/<network_id>/<sha256>/_ objects no base at or past the
//	         point names, once they are KeepAfterNewerBase old, so a base's rows
//	         written ahead of its entry are never taken
//
// The bases come from the sequencer's verifier, which loads every entry the
// store holds, so a pass in a process with no sequencer deletes nothing.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Tunables of retention.
type ConfigLogRetentionSettings struct {
	// How long the entries a newer base retires stay: 30 days (§7.9), so a
	// device that started at the previous base still finds them.
	KeepAfterNewerBase time.Duration
	// The most objects one network's pass deletes; the next pass goes on.
	DeleteMaxCount int
	// The most networks one pass looks at; the next starts after the last.
	NetworkMaxCount int
	// The page one List asks for.
	ListPageCount int
}

func DefaultConfigLogRetentionSettings() *ConfigLogRetentionSettings {
	return &ConfigLogRetentionSettings{
		KeepAfterNewerBase: 30 * 24 * time.Hour,
		DeleteMaxCount:     1000,
		NetworkMaxCount:    100,
		ListPageCount:      500,
	}
}

// The retention point bases set at nowMs: the newest base at least keep old
// sets it from its folded_through and the base before it, else nothing is
// retired and the point is 1. bases are in position order.
func configLogRetentionPoint(bases []*configLogBaseInfo, nowMs int64, keep time.Duration) uint64 {
	for i := len(bases) - 1; 0 <= i; i -= 1 {
		if nowMs-bases[i].timeMs < keep.Milliseconds() {
			continue
		}
		point := bases[i].foldedThrough + 1
		if 0 < i {
			point = min(point, bases[i-1].n)
		}
		return max(point, 1)
	}
	return 1
}

// The rows the bases at or past point name.
func configLogRetainedRows(bases []*configLogBaseInfo, point uint64) map[string]bool {
	retained := map[string]bool{}
	for _, base := range bases {
		if point <= base.n {
			retained[string(base.rowsSha256)] = true
		}
	}
	return retained
}

// What one network's pass did.
type ConfigLogRetentionNetworkResult struct {
	Point        uint64
	DeletedCount int
	// it stopped at DeleteMaxCount, with more past the point
	More bool
}

// One network's pass at now: the entries below its retention point and the
// rows no retained base names, at most DeleteMaxCount objects.
func (self *ConfigLogSequencer) RetainNetwork(ctx context.Context, networkId server.Id, now time.Time, settings *ConfigLogRetentionSettings) (*ConfigLogRetentionNetworkResult, error) {
	network := self.network(networkId)
	bases, loaded, err := func() ([]*configLogBaseInfo, bool, error) {
		network.stateLock.Lock()
		defer network.stateLock.Unlock()
		if err := network.loadWithLock(ctx, self, nil); err != nil {
			return nil, false, err
		}
		return append([]*configLogBaseInfo{}, network.bases...), network.log != nil, nil
	}()
	if err != nil {
		return nil, err
	}
	if !loaded || len(bases) == 0 {
		return &ConfigLogRetentionNetworkResult{Point: 1}, nil
	}
	point := configLogRetentionPoint(bases, now.UnixMilli(), settings.KeepAfterNewerBase)
	return retainConfigLogBelow(ctx, self.cluster, sidecar.Id(networkId), point, configLogRetainedRows(bases, point), now, settings)
}

// Deletes the network's entries below point, entry 0 never, and then its rows
// that retained does not name and that are KeepAfterNewerBase old, at most
// DeleteMaxCount objects. A point of 1 deletes nothing.
func retainConfigLogBelow(
	ctx context.Context,
	cluster sidecar.BlobCluster,
	networkId sidecar.Id,
	point uint64,
	retained map[string]bool,
	now time.Time,
	settings *ConfigLogRetentionSettings,
) (*ConfigLogRetentionNetworkResult, error) {
	result := &ConfigLogRetentionNetworkResult{Point: point}
	if point <= 1 {
		return result, nil
	}
	startAfter := sidecar.ConfigEntryKey(networkId, 0)
	for {
		infos, more, err := cluster.List(ctx, sidecar.ConfigEntryKeyPrefix(networkId), startAfter, settings.ListPageCount)
		if err != nil {
			return nil, err
		}
		stop := !more || len(infos) == 0
		for _, info := range infos {
			startAfter = info.Key
			n, ok := sidecar.ParseConfigEntryKey(networkId, info.Key)
			if !ok || n == 0 {
				continue
			}
			if point <= n {
				stop = true
				break
			}
			if settings.DeleteMaxCount <= result.DeletedCount {
				result.More = true
				return result, nil
			}
			if err := cluster.Delete(ctx, info.Key); err != nil {
				return nil, err
			}
			result.DeletedCount += 1
		}
		if stop {
			break
		}
	}

	// the rows no retained base names, once old enough that no append can be
	// writing them ahead of its entry
	startAfter = ""
	for {
		infos, more, err := cluster.List(ctx, configLogRowsKeyPrefix(networkId), startAfter, settings.ListPageCount)
		if err != nil {
			return nil, err
		}
		for _, info := range infos {
			startAfter = info.Key
			rowsSha256, ok := configLogParseRowsKey(networkId, info.Key)
			if !ok || retained[string(rowsSha256)] || now.Sub(info.LastModified) < settings.KeepAfterNewerBase {
				continue
			}
			if settings.DeleteMaxCount <= result.DeletedCount {
				result.More = true
				return result, nil
			}
			if err := cluster.Delete(ctx, info.Key); err != nil {
				return nil, err
			}
			result.DeletedCount += 1
		}
		if !more || len(infos) == 0 {
			return result, nil
		}
	}
}

// projrows/<network_id>/, the prefix of the network's rows (sidecar.ConfigRowsKey).
func configLogRowsKeyPrefix(networkId sidecar.Id) string {
	return "projrows/" + networkId.String() + "/"
}

// The rows_sha256 a projrows key of the network names, false for another key.
func configLogParseRowsKey(networkId sidecar.Id, key string) ([]byte, bool) {
	rest, found := strings.CutPrefix(key, configLogRowsKeyPrefix(networkId))
	if !found {
		return nil, false
	}
	hexText, found := strings.CutSuffix(rest, "/_")
	if !found {
		return nil, false
	}
	rowsSha256, err := hex.DecodeString(hexText)
	if err != nil || len(rowsSha256) != 32 || sidecar.ConfigRowsKey(networkId, rowsSha256) != key {
		return nil, false
	}
	return rowsSha256, true
}

// Where a retention pass stopped.
type ConfigLogRetentionResult struct {
	NetworkCount int        `json:"network_count"`
	DeletedCount int        `json:"deleted_count"`
	// set when the pass stopped short: the next starts after it
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

// The networks with a log, in id order after afterNetworkId, at most limit:
// network_acl_head's rows (MG2).
func configLogRetentionNetworks(ctx context.Context, afterNetworkId *server.Id, limit int) (networkIds []server.Id) {
	if !server.SidecarMg2Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		after := server.Id{}
		if afterNetworkId != nil {
			after = *afterNetworkId
		}
		result, err := conn.Query(
			ctx,
			`SELECT network_id FROM network_acl_head WHERE $1 < network_id ORDER BY network_id LIMIT $2`,
			after,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return
}

// One retention pass at now through this process's sequencer, after
// afterNetworkId: each network's entries and rows past its retention point. A
// process with no sequencer deletes nothing and answers its error.
func RetainConfigLogs(ctx context.Context, now time.Time, afterNetworkId *server.Id, settings *ConfigLogRetentionSettings) (*ConfigLogRetentionResult, error) {
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	result := &ConfigLogRetentionResult{}
	networkIds := configLogRetentionNetworks(ctx, afterNetworkId, settings.NetworkMaxCount)
	var retainErr error
	for _, networkId := range networkIds {
		networkResult, err := sequencer.RetainNetwork(ctx, networkId, now, settings)
		if err != nil {
			retainErr = errors.Join(retainErr, fmt.Errorf("%s: %w", networkId, err))
			continue
		}
		result.NetworkCount += 1
		result.DeletedCount += networkResult.DeletedCount
	}
	if len(networkIds) == settings.NetworkMaxCount {
		last := networkIds[len(networkIds)-1]
		result.AfterNetworkId = &last
	}
	return result, retainErr
}
