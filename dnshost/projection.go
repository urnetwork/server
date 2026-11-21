package dnshost

import (
	"context"
	"fmt"
	"net/http"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// DNS reads signed rows through core API. Its block has neither a metadata reader nor a writer credential.
func installDnsProjection(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, source sidecar.AclGateSource, gateSettings *sidecar.AclGateSettings, deps *dnsservice.DnsApiDeps) (func(), error) {
	if source == nil {
		name := server.InternalGateCredentialFileName(server.InternalGateCallerDns)
		credential := func() (string, error) {
			data, err := readHostSecret(settings, name)
			if err != nil {
				return "", fmt.Errorf("dns projection credential: %w", err)
			}
			return server.ParseInternalGateCredential(data)
		}
		if _, err := credential(); err != nil {
			return nil, err
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		var err error
		source, err = sidecar.NewAclGateApiSource(settings.ApiUrl, client, credential, nil)
		if err != nil {
			return nil, err
		}
	}
	cache, stop, err := sidecarhost.WatchAclGate(ctx, host, source, gateSettings)
	if err != nil {
		return nil, err
	}
	view := sidecar.NewAclGateView(cache, host.Abuse, nil)
	if deps.RecordGate == nil {
		deps.RecordGate = dnsservice.NewDnsAclRecordGate(view, cache)
	}
	if deps.KeyRecordGate == nil {
		deps.KeyRecordGate = dnsservice.NewDnsAclKeyRecordGate(view)
	}
	restoreScopes := model.SetDnsScopeLabelSource(model.DnsScopeLabelSourceOf(cache))
	restoreApproval := model.SetApprovalGateStates(model.ApprovalGateStatesOf(cache))
	return func() {
		restoreApproval()
		restoreScopes()
		stop()
	}, nil
}
