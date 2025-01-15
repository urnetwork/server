package connprovider

import (
	"context"
	"fmt"

	"github.com/urnetwork/connect"
)

type Provider struct {
	apiURL string
	jwt    string
	ctx    context.Context
}

func New(ctx context.Context, apiURL string) *Provider {
	return &Provider{
		apiURL: apiURL,
		ctx:    ctx,
	}
}

func ConvertAuthCodeToJWT(ctx context.Context, apiURL string, authCode string) (string, error) {
	strategy := connect.NewClientStrategyWithDefaults(ctx)
	api := connect.NewBringYourApi(ctx, strategy, apiURL)
	res, err := api.AuthCodeLoginSync(&connect.AuthCodeLoginArgs{
		AuthCode: authCode,
	})
	if err != nil {
		return "", fmt.Errorf("could not log in with auth code: %w", err)
	}

	if res.Error != nil {
		return "", fmt.Errorf("auth code login failed: %v", res.Error)
	}

	return res.ByJwt, nil

}

func (p *Provider) ConnectWithToken(jwt string, opts ConnectionOptions) (*ConnectionInfo, error) {

	// Auth network client
	clientJWT, err := authNetworkClient(
		p.ctx,
		p.apiURL,
		jwt,
		&connect.AuthNetworkClientArgs{
			Description: opts.DeviceDescription,
			DeviceSpec:  opts.DeviceSpec,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("auth network client failed: %w", err)
	}

	// Parse client ID
	clientID, err := parseByJwtClientId(clientJWT)
	if err != nil {
		return nil, fmt.Errorf("parse byJwt client id failed: %w", err)
	}
	providersSpec := []*connect.ProviderSpec{
		{
			BestAvailable: true,
		},
	}

	if opts.Location != "" {
		locationId, err := connect.ParseId(opts.Location)
		if err != nil {
			return nil, fmt.Errorf("parse location id failed: %w", err)
		}

		providersSpec = []*connect.ProviderSpec{
			{
				LocationId: &locationId,
			},
		}
	}

	return &ConnectionInfo{
		JWT:           jwt,
		ClientJWT:     clientJWT,
		ClientID:      clientID,
		ProvidersSpec: providersSpec,
	}, nil
}

func (p *Provider) Connect(userAuth, password string, opts ConnectionOptions) (*ConnectionInfo, error) {
	// Login
	jwt, err := LoginWithCredentials(p.ctx, p.apiURL, userAuth, password)
	if err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}
	p.jwt = jwt

	// Extracted function call
	return p.ConnectWithToken(jwt, opts)
}

type ConnectionOptions struct {
	DeviceDescription string
	DeviceSpec        string
	Location          string
}

type ConnectionInfo struct {
	JWT           string
	ClientJWT     string
	ClientID      connect.Id
	ProvidersSpec []*connect.ProviderSpec
}

// SetupConnection handles the full connection setup including the generator creation
func (p *Provider) SetupConnection(userAuth, password string, opts ConnectionOptions, platformURL string) (*ConnectionInfo, *connect.ApiMultiClientGenerator, error) {
	// Get connection info
	connInfo, err := p.Connect(userAuth, password, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("connection failed: %w", err)
	}

	// Create generator
	generator := connect.NewApiMultiClientGenerator(
		p.ctx,
		connInfo.ProvidersSpec,
		connect.NewClientStrategyWithDefaults(p.ctx),
		[]connect.Id{connInfo.ClientID}, // exclude self
		p.apiURL,
		connInfo.ClientJWT,
		platformURL,
		opts.DeviceDescription,
		opts.DeviceSpec,
		"1.2.3",
		connect.DefaultClientSettings,
		connect.DefaultApiMultiClientGeneratorSettings(),
	)

	return connInfo, generator, nil
}
