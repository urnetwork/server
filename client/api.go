package client

import (
	"context"
	// "encoding/json"

	// "encoding/base64"
	// "bytes"
	// "errors"
	"fmt"
	// "io"
	// "net"
	// "net/http"
	// "strings"
	// "time"

	"bringyour.com/connect"
)

var apiLog = logFn("api")

type BringYourApi struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientStrategy *connect.ClientStrategy

	apiUrl string

	byJwt string
}

func NewBringYourApi(apiUrl string) *BringYourApi {
	return newBringYourApiWithContext(context.Background(), apiUrl)
}

func newBringYourApiWithContext(ctx context.Context, apiUrl string) *BringYourApi {
	ctx, cancel := context.WithCancel(ctx)

	clientStrategy := connect.NewClientStrategy(
		ctx,
		connect.DefaultClientStrategySettings(),
	)

	return &BringYourApi{
		ctx:            ctx,
		cancel:         cancel,
		clientStrategy: clientStrategy,
		apiUrl:         apiUrl,
	}
}

// this gets attached to api calls that need it
func (self *BringYourApi) SetByJwt(byJwt string) {
	self.byJwt = byJwt
}

type AuthLoginCallback connect.ApiCallback[*AuthLoginResult]

// `model.AuthLoginArgs`
type AuthLoginArgs struct {
	UserAuth    string `json:"user_auth,omitempty"`
	AuthJwtType string `json:"auth_jwt_type,omitempty"`
	AuthJwt     string `json:"auth_jwt,omitempty"`
}

// `model.AuthLoginResult`
type AuthLoginResult struct {
	UserName    string                  `json:"user_name,omitempty"`
	UserAuth    string                  `json:"user_auth,omitempty"`
	AuthAllowed *StringList             `json:"auth_allowed,omitempty"`
	Error       *AuthLoginResultError   `json:"error,omitempty"`
	Network     *AuthLoginResultNetwork `json:"network,omitempty"`
}

// `model.AuthLoginResultError`
type AuthLoginResultError struct {
	SuggestedUserAuth string `json:"suggested_user_auth,omitempty"`
	Message           string `json:"message"`
}

// `model.AuthLoginResultNetwork`
type AuthLoginResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

func (self *BringYourApi) AuthLogin(authLogin *AuthLoginArgs, callback AuthLoginCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/login", self.apiUrl),
			authLogin,
			self.byJwt,
			&AuthLoginResult{},
			callback,
		)
	})
}

type AuthLoginWithPasswordCallback connect.ApiCallback[*AuthLoginWithPasswordResult]

type AuthLoginWithPasswordArgs struct {
	UserAuth string `json:"user_auth"`
	Password string `json:"password"`
}

type AuthLoginWithPasswordResult struct {
	VerificationRequired *AuthLoginWithPasswordResultVerification `json:"verification_required,omitempty"`
	Network              *AuthLoginWithPasswordResultNetwork      `json:"network,omitempty"`
	Error                *AuthLoginWithPasswordResultError        `json:"error,omitempty"`
}

type AuthLoginWithPasswordResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type AuthLoginWithPasswordResultNetwork struct {
	ByJwt       string `json:"by_jwt,omitempty"`
	NetworkName string `json:"name,omitempty"`
}

type AuthLoginWithPasswordResultError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) AuthLoginWithPassword(authLoginWithPassword *AuthLoginWithPasswordArgs, callback AuthLoginWithPasswordCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/login-with-password", self.apiUrl),
			authLoginWithPassword,
			self.byJwt,
			&AuthLoginWithPasswordResult{},
			callback,
		)
	})
}

type AuthVerifyCallback connect.ApiCallback[*AuthVerifyResult]

type AuthVerifyArgs struct {
	UserAuth   string `json:"user_auth"`
	VerifyCode string `json:"verify_code"`
}

type AuthVerifyResult struct {
	Network *AuthVerifyResultNetwork `json:"network,omitempty"`
	Error   *AuthVerifyResultError   `json:"error,omitempty"`
}

type AuthVerifyResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

type AuthVerifyResultError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) AuthVerify(authVerify *AuthVerifyArgs, callback AuthVerifyCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/verify", self.apiUrl),
			authVerify,
			self.byJwt,
			&AuthVerifyResult{},
			callback,
		)
	})
}

type AuthPasswordResetCallback connect.ApiCallback[*AuthPasswordResetResult]

type AuthPasswordResetArgs struct {
	UserAuth string `json:"user_auth"`
}

type AuthPasswordResetResult struct {
	UserAuth string `json:"user_auth"`
}

func (self *BringYourApi) AuthPasswordReset(authPasswordReset *AuthPasswordResetArgs, callback AuthPasswordResetCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/password-reset", self.apiUrl),
			authPasswordReset,
			self.byJwt,
			&AuthPasswordResetResult{},
			callback,
		)
	})
}

type AuthVerifySendCallback connect.ApiCallback[*AuthVerifySendResult]

type AuthVerifySendArgs struct {
	UserAuth string `json:"user_auth"`
}

type AuthVerifySendResult struct {
	UserAuth string `json:"user_auth"`
}

func (self *BringYourApi) AuthVerifySend(authVerifySend *AuthVerifySendArgs, callback AuthVerifySendCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/verify-send", self.apiUrl),
			authVerifySend,
			self.byJwt,
			&AuthVerifySendResult{},
			callback,
		)
	})
}

type NetworkCheckCallback connect.ApiCallback[*NetworkCheckResult]

type NetworkCheckArgs struct {
	NetworkName string `json:"network_name"`
}

type NetworkCheckResult struct {
	Available bool `json:"available"`
}

func (self *BringYourApi) NetworkCheck(networkCheck *NetworkCheckArgs, callback NetworkCheckCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/network-check", self.apiUrl),
			networkCheck,
			self.byJwt,
			&NetworkCheckResult{},
			callback,
		)
	})
}

type NetworkCreateCallback connect.ApiCallback[*NetworkCreateResult]

type NetworkCreateArgs struct {
	UserName    string `json:"user_name"`
	UserAuth    string `json:"user_auth,omitempty"`
	AuthJwt     string `json:"auth_jwt,omitempty"`
	AuthJwtType string `json:"auth_jwt_type,omitempty"`
	Password    string `json:"password,omitempty"`
	NetworkName string `json:"network_name"`
	Terms       bool   `json:"terms"`
}

type NetworkCreateResult struct {
	Network              *NetworkCreateResultNetwork      `json:"network,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error                *NetworkCreateResultError        `json:"error,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt       string `json:"by_jwt,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
}

type NetworkCreateResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type NetworkCreateResultError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) NetworkCreate(networkCreate *NetworkCreateArgs, callback NetworkCreateCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/network-create", self.apiUrl),
			networkCreate,
			self.byJwt,
			&NetworkCreateResult{},
			callback,
		)
	})
}

type AuthNetworkClientCallback connect.ApiCallback[*AuthNetworkClientResult]

type AuthNetworkClientArgs struct {
	// FIXME how to bring this back as optional with gomobile. Use a new type *OptionalId?
	// if omitted, a new client_id is created
	// ClientId string `json:"client_id,omitempty"`
	Description string `json:"description"`
	DeviceSpec  string `json:"device_spec"`
}

type AuthNetworkClientResult struct {
	ByClientJwt string                  `json:"by_client_jwt,omitempty"`
	Error       *AuthNetworkClientError `json:"error,omitempty"`
}

type AuthNetworkClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool   `json:"client_limit_exceeded"`
	Message             string `json:"message"`
}

func (self *BringYourApi) AuthNetworkClient(authNetworkClient *AuthNetworkClientArgs, callback AuthNetworkClientCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/auth-client", self.apiUrl),
			authNetworkClient,
			self.byJwt,
			&AuthNetworkClientResult{},
			callback,
		)
	})
}

type GetNetworkClientsCallback connect.ApiCallback[*NetworkClientsResult]

type NetworkClientResident struct {
	ClientId              *Id      `json:"client_id"`
	InstanceId            *Id      `json:"client_id"`
	ResidentId            *Id      `json:"resident_id"`
	ResidentHost          string   `json:"resident_host"`
	ResidentService       string   `json:"resident_service"`
	ResidentBlock         string   `json:"resident_block"`
	ResidentInternalPorts *IntList `json:"resident_internal_ports"`
}

type NetworkClientsResult struct {
	Clients *NetworkClientInfoList `json:"clients"`
}

type NetworkClientInfo struct {
	ClientId    *Id    `json:"client_id"`
	NetworkId   *Id    `json:"network_id"`
	Description string `json:"description"`
	DeviceSpec  string `json:"device_spec"`

	CreateTime *Time `json:"create_time"`
	AuthTime   *Time `json:"auth_time"`

	Resident    *NetworkClientResident       `json:"resident,omitempty"`
	ProvideMode ProvideMode                  `json:"provide_mode"`
	Connections *NetworkClientConnectionList `json:"connections"`
}

type NetworkClientConnection struct {
	ClientId          *Id    `json:"client_id"`
	ConnectionId      *Id    `json:"connection_id"`
	ConnectTime       *Time  `json:"connect_time"`
	DisconnectTime    *Time  `json:"disconnect_time,omitempty"`
	ConnectionHost    string `json:"connection_host"`
	ConnectionService string `json:"connection_service"`
	ConnectionBlock   string `json:"connection_block"`
}

func (self *BringYourApi) GetNetworkClients(callback GetNetworkClientsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/clients", self.apiUrl),
			self.byJwt,
			&NetworkClientsResult{},
			callback,
		)
	})
}

type FindLocationsCallback connect.ApiCallback[*FindLocationsResult]

type FindLocationsArgs struct {
	Query string `json:"query"`
	// the max search distance is `MaxDistanceFraction * len(Query)`
	// in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
	MaxDistanceFraction       float32 `json:"max_distance_fraction,omitempty"`
	EnableMaxDistanceFraction bool    `json:"enable_max_distance_fraction,omitempty"`
}

type FindLocationsResult struct {
	Specs *ProviderSpecList `json:"specs"`
	// this includes groups that show up in the location results
	// all `ProviderCount` are from inside the location results
	// groups are suggestions that can be used to broaden the search
	Groups *LocationGroupResultList `json:"groups"`
	// this includes all parent locations that show up in the location results
	// every `CityId`, `RegionId`, `CountryId` will have an entry
	Locations *LocationResultList       `json:"locations"`
	Devices   *LocationDeviceResultList `json:"devices"`
}

type LocationResult struct {
	LocationId   *Id          `json:"location_id"`
	LocationType LocationType `json:"location_type"`
	Name         string       `json:"name"`
	// FIXME
	City string `json:"city,omitempty"`
	// FIXME
	Region string `json:"region,omitempty"`
	// FIXME
	Country           string `json:"country,omitempty"`
	CountryCode       string `json:"country_code,omitempty"`
	CityLocationId    *Id    `json:"city_location_id,omitempty"`
	RegionLocationId  *Id    `json:"region_location_id,omitempty"`
	CountryLocationId *Id    `json:"country_location_id,omitempty"`
	ProviderCount     int    `json:"provider_count,omitempty"`
	MatchDistance     int    `json:"match_distance,omitempty"`
}

type LocationGroupResult struct {
	LocationGroupId *Id    `json:"location_group_id"`
	Name            string `json:"name"`
	ProviderCount   int    `json:"provider_count,omitempty"`
	Promoted        bool   `json:"promoted,omitempty"`
	MatchDistance   int    `json:"match_distance,omitempty"`
}

type LocationDeviceResult struct {
	ClientId   *Id    `json:"client_id"`
	DeviceName string `json:"device_name"`
}

func (self *BringYourApi) GetProviderLocations(callback FindLocationsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/provider-locations", self.apiUrl),
			self.byJwt,
			&FindLocationsResult{},
			callback,
		)
	})
}

func (self *BringYourApi) FindProviderLocations(findLocations *FindLocationsArgs, callback FindLocationsCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/find-provider-locations", self.apiUrl),
			findLocations,
			self.byJwt,
			&FindLocationsResult{},
			callback,
		)
	})
}

func (self *BringYourApi) FindLocations(findLocations *FindLocationsArgs, callback FindLocationsCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/find-locations", self.apiUrl),
			findLocations,
			self.byJwt,
			&FindLocationsResult{},
			callback,
		)
	})
}

type FindProvidersCallback connect.ApiCallback[*FindProvidersResult]

type FindProvidersArgs struct {
	LocationId       *Id     `json:"location_id,omitempty"`
	LocationGroupId  *Id     `json:"location_group_id,omitempty"`
	Count            int     `json:"count"`
	ExcludeClientIds *IdList `json:"exclude_location_ids,omitempty"`
}

type FindProvidersResult struct {
	ClientIds *IdList `json:"client_ids,omitempty"`
}

func (self *BringYourApi) FindProviders(findProviders *FindProvidersArgs, callback FindProvidersCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/find-providers", self.apiUrl),
			findProviders,
			self.byJwt,
			&FindProvidersResult{},
			callback,
		)
	})
}

type ProviderSpec struct {
	LocationId      *Id `json:"location_id,omitempty"`
	LocationGroupId *Id `json:"location_group_id,omitempty"`
	ClientId        *Id `json:"client_id,omitempty"`
}

func (self *ProviderSpec) toConnectProviderSpec() *connect.ProviderSpec {
	connectProviderSpec := &connect.ProviderSpec{}
	if self.LocationId != nil {
		connectLocationId := self.LocationId.toConnectId()
		connectProviderSpec.LocationId = &connectLocationId
	}
	if self.LocationGroupId != nil {
		connectLocationGroupId := self.LocationGroupId.toConnectId()
		connectProviderSpec.LocationGroupId = &connectLocationGroupId
	}
	if self.ClientId != nil {
		connectClientId := self.ClientId.toConnectId()
		connectProviderSpec.ClientId = &connectClientId
	}
	return connectProviderSpec
}

type FindProviders2Callback connect.ApiCallback[*FindProviders2Result]

type FindProviders2Args struct {
	Specs            *ProviderSpecList `json:"specs"`
	Count            int               `json:"count"`
	ExcludeClientIds *IdList           `json:"exclude_client_ids"`
}

type FindProviders2Result struct {
	ProviderStats *FindProvidersProviderList `json:"providers"`
}

type FindProvidersProvider struct {
	ClientId                *Id `json:"client_id"`
	EstimatedBytesPerSecond int `json:"estimated_bytes_per_second"`
}

func (self *BringYourApi) FindProviders2(findProviders2 *FindProviders2Args, callback FindProvidersCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/find-providers2", self.apiUrl),
			findProviders2,
			self.byJwt,
			&FindProvidersResult{},
			callback,
		)
	})
}

type WalletCircleInitCallback connect.ApiCallback[*WalletCircleInitResult]

type WalletCircleInitResult struct {
	UserToken   *CircleUserToken       `json:"user_token,omitempty"`
	ChallengeId string                 `json:"challenge_id,omitempty"`
	Error       *WalletCircleInitError `json:"error,omitempty"`
}

type WalletCircleInitError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) WalletCircleInit(callback WalletCircleInitCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/wallet/circle-init", self.apiUrl),
			nil,
			self.byJwt,
			&WalletCircleInitResult{},
			callback,
		)
	})
}

type WalletValidateAddressCallback connect.ApiCallback[*WalletValidateAddressResult]

type WalletValidateAddressArgs struct {
	Address string `json:"address,omitempty"`
	Chain   string `json:"chain,omitempty"`
}

type WalletValidateAddressResult struct {
	Valid bool `json:"valid,omitempty"`
}

func (self *BringYourApi) WalletValidateAddress(walletValidateAddress *WalletValidateAddressArgs, callback WalletValidateAddressCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/wallet/validate-address", self.apiUrl),
			walletValidateAddress,
			self.byJwt,
			&WalletValidateAddressResult{},
			callback,
		)
	})
}

type WalletType = string

const (
	WalletTypeCircleUserControlled WalletType = "circle_uc"
	WalletTypeXch                  WalletType = "xch"
	WalletTypeSol                  WalletType = "sol"
)

type Blockchain = string

const (
	SOL   Blockchain = "SOL"
	MATIC Blockchain = "MATIC"
)

type CreateAccountWalletArgs struct {
	Blockchain       Blockchain `json:"blockchain"`
	WalletAddress    string     `json:"wallet_address"`
	DefaultTokenType string     `json:"default_token_type"`
}

type CreateAccountWalletResult struct {
	WalletId *Id `json:"wallet_id"`
}

type CreateAccountWalletCallback connect.ApiCallback[*CreateAccountWalletResult]

func (self *BringYourApi) CreateAccountWallet(createAccountWallet *CreateAccountWalletArgs, callback CreateAccountWalletCallback) {

	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/account/wallet", self.apiUrl),
			createAccountWallet,
			self.byJwt,
			&CreateAccountWalletResult{},
			callback,
		)
	})
}

type SetPayoutWalletArgs struct {
	WalletId *Id `json:"wallet_id"`
}

type SetPayoutWalletResult struct{}

type SetPayoutWalletCallback connect.ApiCallback[*SetPayoutWalletResult]

func (self *BringYourApi) SetPayoutWallet(payoutWallet *SetPayoutWalletArgs, callback SetPayoutWalletCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/account/payout-wallet", self.apiUrl),
			payoutWallet,
			self.byJwt,
			&SetPayoutWalletResult{},
			callback,
		)
	})
}

type GetAccountWalletsResult struct {
	Wallets *AccountWalletsList `json:"wallets"`
}

type GetAccountWalletsCallback connect.ApiCallback[*GetAccountWalletsResult]

func (self *BringYourApi) GetAccountWallets(callback GetAccountWalletsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/account/wallets", self.apiUrl),
			self.byJwt,
			&GetAccountWalletsResult{},
			callback,
		)
	})
}

type GetPayoutWalletIdResult struct {
	Id *Id `json:"id"`
}

type GetPayoutWalletCallback connect.ApiCallback[*GetPayoutWalletIdResult]

func (self *BringYourApi) GetPayoutWallet(callback GetPayoutWalletCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/account/payout-wallet", self.apiUrl),
			self.byJwt,
			&GetPayoutWalletIdResult{},
			callback,
		)
	})
}

type CircleUserToken struct {
	UserToken     string `json:"user_token"`
	EncryptionKey string `json:"encryption_key"`
}

type CircleWalletInfo struct {
	WalletId             string    `json:"wallet_id"`
	TokenId              string    `json:"token_id"`
	Blockchain           string    `json:"blockchain"`
	BlockchainSymbol     string    `json:"blockchain_symbol"`
	CreateDate           string    `json:"create_date"`
	BalanceUsdcNanoCents NanoCents `json:"balance_usdc_nano_cents"`
}

type WalletBalanceCallback connect.ApiCallback[*WalletBalanceResult]

type WalletBalanceResult struct {
	WalletInfo *CircleWalletInfo `json:"wallet_info,omitempty"`
}

func (self *BringYourApi) WalletBalance(callback WalletBalanceCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/wallet/balance", self.apiUrl),
			self.byJwt,
			&WalletBalanceResult{},
			callback,
		)
	})
}

type WalletCircleTransferOutCallback connect.ApiCallback[*WalletCircleTransferOutResult]

type WalletCircleTransferOutArgs struct {
	ToAddress           string    `json:"to_address"`
	AmountUsdcNanoCents NanoCents `json:"amount_usdc_nano_cents"`
	Terms               bool      `json:"terms"`
}

type WalletCircleTransferOutResult struct {
	UserToken   *CircleUserToken              `json:"user_token,omitempty"`
	ChallengeId string                        `json:"challenge_id,omitempty"`
	Error       *WalletCircleTransferOutError `json:"error,omitempty"`
}

type WalletCircleTransferOutError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) WalletCircleTransferOut(walletCircleTransferOut *WalletCircleTransferOutArgs, callback WalletCircleTransferOutCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/wallet/circle-transfer-out", self.apiUrl),
			walletCircleTransferOut,
			self.byJwt,
			&WalletCircleTransferOutResult{},
			callback,
		)
	})
}

type Subscription struct {
	SubscriptionId *Id    `json:"subscription_id"`
	Store          string `json:"store"`
	Plan           string `json:"plan"`
}

type TransferBalance struct {
	BalanceId             *Id       `json:"balance_id"`
	NetworkId             *Id       `json:"network_id"`
	StartTime             string    `json:"start_time"`
	EndTime               string    `json:"end_time"`
	StartBalanceByteCount ByteCount `json:"start_balance_byte_count"`
	// how much money the platform made after subtracting fees
	NetRevenue       NanoCents `json:"net_revenue"`
	BalanceByteCount ByteCount `json:"balance_byte_count"`
}

type SubscriptionBalanceCallback connect.ApiCallback[*SubscriptionBalanceResult]

type SubscriptionBalanceResult struct {
	BalanceByteCount          ByteCount            `json:"balance_byte_count"`
	CurrentSubscription       *Subscription        `json:"current_subscription,omitempty"`
	ActiveTransferBalances    *TransferBalanceList `json:"active_transfer_balances,omitempty"`
	PendingPayoutUsdNanoCents NanoCents            `json:"pending_payout_usd_nano_cents"`
	WalletInfo                *CircleWalletInfo    `json:"wallet_info,omitempty"`
	UpdateTime                string               `json:"update_time"`
}

func (self *BringYourApi) SubscriptionBalance(callback SubscriptionBalanceCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/subscription/balance", self.apiUrl),
			self.byJwt,
			&SubscriptionBalanceResult{},
			callback,
		)
	})
}

type SubscriptionCreatePaymentIdCallback connect.ApiCallback[*SubscriptionCreatePaymentIdResult]

type SubscriptionCreatePaymentIdArgs struct {
}

type SubscriptionCreatePaymentIdResult struct {
	SubscriptionPaymentId *Id                               `json:"subscription_payment_id,omitempty"`
	Error                 *SubscriptionCreatePaymentIdError `json:"error,omitempty"`
}

type SubscriptionCreatePaymentIdError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) SubscriptionCreatePaymentId(createPaymentId *SubscriptionCreatePaymentIdArgs, callback SubscriptionCreatePaymentIdCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/subscription/create-payment-id", self.apiUrl),
			createPaymentId,
			self.byJwt,
			&SubscriptionCreatePaymentIdResult{},
			callback,
		)
	})
}

func (self *BringYourApi) SubscriptionCreatePaymentIdSync(createPaymentId *SubscriptionCreatePaymentIdArgs) (*SubscriptionCreatePaymentIdResult, error) {
	return connect.HttpPostWithStrategy(
		self.ctx,
		self.clientStrategy,
		fmt.Sprintf("%s/subscription/create-payment-id", self.apiUrl),
		createPaymentId,
		self.byJwt,
		&SubscriptionCreatePaymentIdResult{},
		connect.NewNoopApiCallback[*SubscriptionCreatePaymentIdResult](),
	)
}

type NetworkUser struct {
	UserId   *Id    `json:"userId"`
	UserName string `json:"userName"`
	UserAuth string `json:"userAuth"`
	Verified bool   `json:"verified"`
	AuthType string `json:"authType"`
}

type GetNetworkUserError struct {
	Message string `json:"message"`
}

type GetNetworkUserResult struct {
	NetworkUser *NetworkUser         `json:"networkUser,omitempty"`
	Error       *GetNetworkUserError `json:"error,omitempty"`
}

type GetNetworkUserCallback connect.ApiCallback[*GetNetworkUserResult]

func (self *BringYourApi) GetNetworkUser(callback GetNetworkUserCallback) (*GetNetworkUserResult, error) {
	return connect.HttpGetWithStrategy(
		self.ctx,
		self.clientStrategy,
		fmt.Sprintf("%s/network/user", self.apiUrl),
		self.byJwt,
		&GetNetworkUserResult{},
		callback,
	)
}
