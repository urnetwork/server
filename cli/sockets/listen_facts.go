package main

// The sockets block reads server-owned listen facts from the API on its own
// internal bearer. The namespace cache calls this transport off the inline path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server/model"
)

// Installs the authorization log source and the facts source at the same configured API.
func (self *hostDeps) setGateApiSources(apiUrl string, bearer string, client *http.Client, maxFactsBytes int64) error {
	if bearer == "" || maxFactsBytes <= 0 {
		return errors.New("sockets needs an internal bearer and a positive listen facts bound")
	}
	if client == nil {
		client = &http.Client{}
	}
	// Keep the block's internal bearer at this origin.
	internalClient := *client
	internalClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	projection, err := sidecar.NewAclGateApiSource(apiUrl, &internalClient, func() (string, error) { return bearer, nil }, nil)
	if err != nil {
		return err
	}
	self.Source = projection
	base := strings.TrimSuffix(apiUrl, "/") + model.TlsListenFactsPath
	self.TlsListenFacts = socketsservice.TlsListenFactsFunc(func(ctx context.Context, networkId connect.Id) (*socketsservice.TlsListenFacts, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+networkId.String(), nil)
		if err != nil {
			return nil, errors.New("invalid listen facts request")
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := internalClient.Do(request)
		if err != nil {
			return nil, errors.New("listen facts request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listen facts returned HTTP %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, maxFactsBytes+1))
		if err != nil || int64(len(body)) > maxFactsBytes {
			return nil, errors.New("listen facts could not be read within the response bound")
		}
		var facts *socketsservice.TlsListenFacts
		if err := json.Unmarshal(body, &facts); err != nil || facts == nil {
			return nil, errors.New("invalid listen facts")
		}
		return facts, nil
	})
	return nil
}
