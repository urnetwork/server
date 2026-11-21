package model

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
)

// Only the leases and group acknowledgments belonging to a verified repo are
// read. A missing hash means no fields existed. A missing or inconsistent
// projection, failed object read or unavailable Redis is a failed snapshot.
type abuseHoldSource struct {
	host      *sidecar.ServiceHost
	leaseLife time.Duration
}

func NewAbuseHoldSoftStateSource(host *sidecar.ServiceHost, settings *evsservice.EvsSettings) AbuseHoldSoftStateSource {
	if settings == nil {
		settings = evsservice.DefaultEvsSettings()
	}
	return &abuseHoldSource{host: host, leaseLife: settings.LeaseLife}
}

func (s *abuseHoldSource) NetworkRepoIds(ctx context.Context, tx server.PgTx, networkId server.Id) ([]server.Id, error) {
	if s.host == nil || s.host.Blobs == nil {
		return nil, ErrHoldPlacementSnapshotUnavailable
	}
	cluster, err := s.host.Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	// A placement does not reuse a process's cached projection. Verify the
	// retained chain afresh and require the pin Postgres records for it.
	head, err := getNetworkAclHeadIn(ctx, tx, networkId)
	if err != nil {
		return nil, err
	}
	sequencer := NewConfigLogSequencer(cluster, nil, nil)
	network := sequencer.network(networkId)
	if err := network.loadWithLock(ctx, sequencer, nil); err != nil {
		return nil, err
	}
	if network.log == nil {
		if head != nil {
			return nil, ErrHoldPlacementSnapshotUnavailable
		}
		positions, _, err := sequencer.listPositions(ctx, networkId, 0, 1)
		if err != nil {
			return nil, err
		}
		if len(positions) != 0 {
			return nil, ErrHoldPlacementSnapshotUnavailable
		}
		return nil, nil
	}
	pin := network.log.Pin()
	if network.log.State().NetworkId() != sidecar.Id(networkId) || head == nil || head.Version != pin.GetVersion() || !bytes.Equal(head.Chain, pin.GetChain()) {
		return nil, ErrHoldPlacementSnapshotUnavailable
	}
	view, err := network.log.State().Acl()
	if err != nil {
		return nil, err
	}
	var repos []server.Id
	for _, id := range view.RepoIds() {
		repos = append(repos, server.Id(id))
	}
	return repos, nil
}

func (s *abuseHoldSource) RepoSoftState(ctx context.Context, id server.Id, at time.Time) ([]*AbuseHoldSoftStateKey, error) {
	if s.host == nil || s.host.Redis == nil || s.host.Blobs == nil {
		return nil, ErrHoldPlacementSnapshotUnavailable
	}
	repoId := sidecar.Id(id)
	leaseKey := evsservice.LeaseKey(repoId)
	keys := []string{leaseKey}
	placement := sidecar.NewEvsRepoPlaceResolver(s.host.Blobs, s.host.Clock, nil)
	cluster, _, err := placement.Cluster(ctx, repoId)
	if err != nil && !errors.Is(err, sidecar.ErrEvsRepoNotPlaced) {
		return nil, err
	}
	if err == nil {
		ackKeys, err := gkmservice.RepoAckKeys(ctx, cluster, repoId)
		if err != nil {
			return nil, err
		}
		keys = append(keys, ackKeys...)
	}
	var found []*AbuseHoldSoftStateKey
	for _, key := range keys {
		fields, err := s.host.Redis.HashGetAll(ctx, key)
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue
		}
		entry := &AbuseHoldSoftStateKey{Key: key, Fields: make(map[string]string, len(fields))}
		for field, value := range fields {
			if !at.IsZero() && !s.softStateExistedAt(key == leaseKey, field, string(value), at) {
				return nil, ErrHoldPlacementSnapshotUnavailable
			}
			entry.Fields[field] = string(value)
		}
		found = append(found, entry)
	}
	return found, nil
}

// EVS stores the seven-day expiry on each write. GKM stores each ack's
// receipt millisecond and the hash's creation millisecond. An overwrite
// after the placement cut makes the previous value unknowable.
func (s *abuseHoldSource) softStateExistedAt(lease bool, field, value string, at time.Time) bool {
	stamp := value
	if !lease && field != "since" {
		var ok bool
		_, stamp, ok = strings.Cut(value, ":")
		if !ok {
			return false
		}
	}
	ms, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || ms <= 0 {
		return false
	}
	written := time.UnixMilli(ms)
	if lease {
		written = written.Add(-s.leaseLife)
	}
	return !written.After(at)
}
