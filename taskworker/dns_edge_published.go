package taskworker

// §12.7's hourly edge-records pass in taskworker (model.ReconcileDnsEdgeRecords, work.DnsEdgeRecords): the source it
// reads each network's published names and Team scope labels from (model.DnsEdgePublishedSource), both of one
// verified state, over this process's one gate cache (C-09), the states §21.6's published names read
// (tls_published_names.go). It loads as that source does: a network the cache does not hold is asked for and waited
// on, at most LoadWait, and one still not loaded fails the run, which the pass retries rather than take grants it
// never read for none. The scope labels are the state's own (sidecar.ConfigState.TeamScopeLabels), so a scope is
// taken only from a verified projection (§12.5), and the pass needs no process-wide scope source.

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// See model.DnsEdgePublishedSource: the network's loaded state's published names at now and its scope labels.
func (self *tlsPublishedNamesSource) edgePublished(ctx context.Context, networkId server.Id, now time.Time) (*model.DnsEdgePublished, error) {
	snapshot, err := self.state(ctx, sidecar.Id(networkId))
	if err != nil {
		return nil, err
	}
	return model.DnsEdgePublishedOf(snapshot.Acl, snapshot.Config, now), nil
}

// Installs the pass's source over states as the process's (model.SetDnsEdgePublishedSource), and answers what
// removes it unless a newer runtime's has replaced it.
func installDnsEdgePublishedSource(states tlsPublishedNamesStates) (remove func()) {
	return installDnsEdgePublishedSourceWithSettings(states, nil)
}

// installDnsEdgePublishedSource with the loader's settings, nil for the defaults.
func installDnsEdgePublishedSourceWithSettings(states tlsPublishedNamesStates, settings *tlsPublishedNamesSourceSettings) (remove func()) {
	return model.SetDnsEdgePublishedSource(newTlsPublishedNamesSource(states, settings).edgePublished)
}
