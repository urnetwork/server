package client

import (
	"context"
	// "sync"

	"bringyour.com/connect"
)

// a network space is a set of server and app configurations

type Extender struct {
	Ip     string
	Secret string
}

type ExtenderAutoConfigure struct {
	DnsIp            string
	ExtenderHostname string
}

type NetworkSpaceKey struct {
	HostName string
	EnvName  string
}

type NetworkSpaceValues struct {
	EnvSecret                string
	Bundled                  bool
	NetExposeServerIps       bool
	NetExposeServerHostNames bool
	LinkHostName             string
	MigrationHostName        string
	Store                    string
	Wallet                   string

	// custom extender
	// this overrides any auto discovered extenders
	Extender              *Extender
	ExtenderAutoConfigure *ExtenderAutoConfigure
}

type NetworkSpace struct {
	ctx    context.Context
	cancel context.CancelFunc

	key         NetworkSpaceKey
	values      NetworkSpaceValues
	storagePath string

	apiUrl      string
	platformUrl string

	clientStrategy  *connect.ClientStrategy
	asyncLocalState *AsyncLocalState
	api             *BringYourApi

	// ProfileKey
	//     all urls are derived from the host name and env name
	// bundled
	// require ech
	// custom extender
	// custom extender autoconfigure
	// link prefix
	// migrationn host name
	// store
	// wallet

	// local settings

	// create api from networkspace
	// create device from api that has networkspace
}

func newNetworkSpace(ctx context.Context, key NetworkSpaceKey, values NetworkSpaceValues, storagePath string) {

	// clientStrategySettings := connect.DefaultClientStrategySettings()
	// clientStrategySettings.ExposeServerIps = networkSpace.GetExposeServerIps()
	// clientStrategySettings.ExposeServerHostNames = networkSpace.GetExposeServerHostNames()

	// clientStrategy := connect.NewClientStrategy(
	// 	cancelCtx,
	// 	clientStrategySettings,
	// )

	// extenderIpSecrets := map[netip.Addr]string{}
	// if extender != nil {
	// 	if ip, err := netip.ParseAddr(extender.Ip); err == nil {
	// 		extenderIpSecrets[ip] = extender.Secret
	// 	}
	// }
	// self.clientStrategy.SetCustomExtenders(extenderIpSecrets)
}

func (self *NetworkSpace) GetHostName() string {
	return self.values.EnvSecret
}

func (self *NetworkSpace) GetEnvName() string {
	return self.values.EnvSecret
}

func (self *NetworkSpace) GetEnvSecret() string {
	return self.values.EnvSecret
}

func (self *NetworkSpace) GetBundled() bool {
	return self.values.Bundled
}

func (self *NetworkSpace) GetNetExposeServerIps() bool {
	return self.values.NetExposeServerIps
}

func (self *NetworkSpace) GetNetExposeServerHostNames() bool {
	return self.values.NetExposeServerHostNames
}

func (self *NetworkSpace) GetLinkHostName() string {
	return self.values.LinkHostName
}

func (self *NetworkSpace) GetMigrationHostName() string {
	return self.values.MigrationHostName
}

func (self *NetworkSpace) GetStore() string {
	return self.values.Store
}

func (self *NetworkSpace) GetWallet() string {
	return self.values.Wallet
}

func (self *NetworkSpace) GetExtender() *Extender {
	return self.values.Extender
}

func (self *NetworkSpace) GetExtenderAutoConfigure() *ExtenderAutoConfigure {
	return self.values.ExtenderAutoConfigure
}

func (self *NetworkSpace) GetAsyncLocalState() *AsyncLocalState {
	return self.asyncLocalState
}

func (self *NetworkSpace) GetApiUrl() string {
	return self.apiUrl
}

func (self *NetworkSpace) GetPlatformUrl() string {
	return self.platformUrl
}

func (self *NetworkSpace) GetApi() *BringYourApi {
	return self.api
}

func (self *NetworkSpace) close() {
	self.cancel()
}

type NetworkSpaceUpdate interface {
	Update(values *NetworkSpaceValues)
}

type NetworkSpaceListener interface {
	ActiveNetworkSpaceChanged(networkSpace *NetworkSpace)
}

type NetworkSpaceManager struct {
}

func NewNetworkSpaceManager(storagePath string) *NetworkSpaceManager {
	return &NetworkSpaceManager{}
}

func (self *NetworkSpaceManager) GetActiveNetworkSpace() *NetworkSpace {
	return nil
}

func (self *NetworkSpaceManager) SetActiveNetworkSpace(networkSpace *NetworkSpace) {

}

func (self *NetworkSpaceManager) AddNetworkSpaceListener(listener NetworkSpaceListener) Sub {
	return nil
}

func (self *NetworkSpaceManager) GetNetworkSpace(hostName string, envName string) *NetworkSpace {
	return nil
}

func (self *NetworkSpaceManager) UpdateNetworkSpace(hostName string, envName string, callback NetworkSpaceUpdate) *NetworkSpace {
	return nil
}

func (self *NetworkSpaceManager) RemoveNetworkSpace(networkSpace *NetworkSpace) {

}
