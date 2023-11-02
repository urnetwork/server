package client


import (
	"encoding/json"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)


const defaultHttpTimeout = 10 * time.Second
const defaultHttpConnectTimeout = 5 * time.Second
const defaultHttpTlsTimeout = 5 * time.Second


func defaultClient() *http.Client {
	// see https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
	dialer := &net.Dialer{
    	Timeout: defaultHttpConnectTimeout,
  	}
	transport := &http.Transport{
	  	DialContext: dialer.DialContext,
	  	TLSHandshakeTimeout: defaultHttpTlsTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout: defaultHttpTimeout,
	}
}


type apiCallback[R any] interface {
	Result(result R)
	Error()
}


type BringYourApi struct {
	apiUrl string

	byJwt string
}

// TODO manage extenders

func NewBringYourApi(apiUrl string) *BringYourApi {
	// FIXME
	return nil
}

// this gets attached to api calls that need it
func (self *BringYourApi) SetByJwt(byJwt string) {

}


type AuthLoginCallback apiCallback[*AuthLoginResult]

// `model.AuthLoginArgs`
type AuthLoginArgs struct {
	UserAuth string `json:"user_auth,omitempty"`
	AuthJwtType string `json:"auth_jwt_type,omitempty"`
	AuthJwt string `json:"auth_jwt,omitempty"`
}

// `model.AuthLoginResult`
type AuthLoginResult struct {
	UserName string `json:"user_name,omitempty"`
	UserAuth string `json:"user_auth,omitempty"`
	AuthAllowed *StringList `json:"auth_allowed,omitempty"`
	Error *AuthLoginResultError `json:"error,omitempty"`
	Network *AuthLoginResultNetwork `json:"network,omitempty"`
}

// `model.AuthLoginResultError`
type AuthLoginResultError struct {
	SuggestedUserAuth string `json:"suggested_user_auth,omitempty"`
	Message string `json:"message"`
}

// `model.AuthLoginResultNetwork`
type AuthLoginResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

func (self *BringYourApi) AuthLogin(authLogin *AuthLoginArgs, callback AuthLoginCallback) {
	go post(
		fmt.Sprintf("%s/auth/login", self.apiUrl),
		authLogin,
		&AuthLoginResult{},
		callback,
	)
}


type AuthLoginWithPasswordCallback apiCallback[*AuthLoginWithPasswordResult]

type AuthLoginWithPasswordArgs struct {
	UserAuth string `json:"user_auth"`
	Password string `json:"password"`
}

type AuthLoginWithPasswordResult struct {
	VerificationRequired *AuthLoginWithPasswordResultVerification `json:"verification_required,omitempty"`
	Network *AuthLoginWithPasswordResultNetwork `json:"network,omitempty"`
	Error *AuthLoginWithPasswordResultError `json:"error,omitempty"`
}

type AuthLoginWithPasswordResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type AuthLoginWithPasswordResultNetwork struct {
	ByJwt string `json:"by_jwt,omitempty"`
	NetworkName string `json:"name,omitempty"`
}

type AuthLoginWithPasswordResultError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) AuthLoginWithPassword(authLoginWithPassword *AuthLoginWithPasswordArgs, callback AuthLoginWithPasswordCallback) {
	go post(
		fmt.Sprintf("%s/auth/login-with-password", self.apiUrl),
		authLoginWithPassword,
		&AuthLoginWithPasswordResult{},
		callback,
	)
}


type AuthVerifyCallback apiCallback[*AuthVerifyResult]

type AuthVerifyArgs struct {
	UserAuth string `json:"user_auth"`
	VerifyCode string `json:"verify_code"`
}

type AuthVerifyResult struct {
	Network *AuthVerifyResultNetwork `json:"network,omitempty"`
	Error *AuthVerifyResultError `json:"error,omitempty"`
}

type AuthVerifyResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

type AuthVerifyResultError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) AuthVerify(authVerify *AuthVerifyArgs, callback AuthVerifyCallback) {
	go post(
		fmt.Sprintf("%s/auth/verify", self.apiUrl),
		authVerify,
		&AuthVerifyResult{},
		callback,
	)
}


type NetworkCheckCallback apiCallback[*NetworkCheckResult]

type NetworkCheckArgs struct {
	NetworkName string  `json:"network_name"`
}

type NetworkCheckResult struct {
	Available bool  `json:"available"`
}

func (self *BringYourApi) NetworkCheck(networkCheck *NetworkCheckArgs, callback NetworkCheckCallback) {
	go post(
		fmt.Sprintf("%s/auth/network-check", self.apiUrl),
		networkCheck,
		&NetworkCheckResult{},
		callback,
	)
}


type NetworkCreateCallback apiCallback[*NetworkCreateResult]

type NetworkCreateArgs struct {
	UserName string `json:"user_name"`
	UserAuth string `json:"user_auth,omitempty"`
	AuthJwt string `json:"auth_jwt,omitempty"`
	AuthJwtType string `json:"auth_jwt_type,omitempty"`
	Password string `json:"password,omitempty"`
	NetworkName string `json:"network_name"`
	Terms bool `json:"terms"`
}

type NetworkCreateResult struct {
	Network *NetworkCreateResultNetwork `json:"network,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error *NetworkCreateResultError `json:"error,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt string `json:"by_jwt,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
}

type NetworkCreateResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type NetworkCreateResultError struct {
	Message string `json:"message"`
}

func (self *BringYourApi) NetworkCreate(networkCreate *NetworkCreateArgs, callback NetworkCreateCallback) {
	go post(
		fmt.Sprintf("%s/auth/network-create", self.apiUrl),
		networkCreate,
		&NetworkCreateResult{},
		callback,
	)
}


type AuthNetworkClientCallback apiCallback[*AuthNetworkClientResult]

type AuthNetworkClientArgs struct {
	// if omitted, a new client_id is created
	ClientId Id `json:"client_id",omitempty`
	Description string `json:"description"`
	DeviceSpec string `json:"device_spec"`
}

type AuthNetworkClientResult struct {
	ByJwt string `json:"by_jwt,omitempty"`
	Error *AuthNetworkClientError `json:"error,omitempty"`
}

type AuthNetworkClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool `json:"client_limit_exceeded"` 
	Message string `json:"message"`
}

func (self *BringYourApi) AuthNetworkClient(authNetworkClient *AuthNetworkClientArgs, callback AuthNetworkClientCallback) {
	go post(
		fmt.Sprintf("%s/network/auth-client", self.apiUrl),
		authNetworkClient,
		&AuthNetworkClientResult{},
		callback,
	)
}


type FindLocationsCallback apiCallback[*FindLocationsResult]

type FindLocationsArgs struct {
    Query string `json:"query"`
    // the max search distance is `MaxDistanceFraction * len(Query)`
    // in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
    MaxDistanceFraction float32 `json:"max_distance_fraction,omitempty"`
    EnableMaxDistanceFraction bool `json:"enable_max_distance_fraction,omitempty"`
}

type FindLocationsResult struct {
    // this includes groups that show up in the location results
    // all `ProviderCount` are from inside the location results
    // groups are suggestions that can be used to broaden the search
    Groups *LocationGroupResultList `json:"groups"`
    // this includes all parent locations that show up in the location results
    // every `CityId`, `RegionId`, `CountryId` will have an entry
    Locations *LocationResultList `json:"locations"`
}

type LocationGroupResult struct {
    LocationGroupId Id
    Name string
    ProviderCount int
    Promoted bool
    MatchDistance int
}

type LocationResult struct {
    LocationId Id `json:"location_id"`
    LocationType LocationType `json:"location_type"`
    Name string `json:"name"`
    CityLocationId Id `json:"city_location_id,omitempty"`
    RegionLocationId Id `json:"region_location_id,omitempty"`
    CountryLocationId Id `json:"country_location_id,omitempty"`
    CountryCode string `json:"country_code"`
    ProviderCount int `json:"provider_count,omitempty"`
    MatchDistance int `json:"match_distance,omitempty"`
}

func (self *BringYourApi) FindLocations(findLocations *FindLocationsArgs, callback FindLocationsCallback) {
	go post(
		fmt.Sprintf("%s/network/locations", self.apiUrl),
		findLocations,
		&FindLocationsResult{},
		callback,
	)
}


type GetActiveProvidersCallback apiCallback[*GetActiveProvidersResults]

type GetActiveProvidersArgs struct {
	LocationId Id `json:"location_id,omitempty"`
	GroupLocationId Id `json:"group_location_id,omitempty"`
	Count int `json:"count"`
	ExcludeClientIds *IdList `json:"exclude_location_ids,omitempty"`
}

type GetActiveProvidersResults struct {
	ClientIds *IdList `json:"client_ids,omitempty"`
}

func (self *BringYourApi) GetActiveProviders(getActiveProviders *GetActiveProvidersArgs, callback GetActiveProvidersCallback) {
	go post(
		fmt.Sprintf("%s/network/active-providers", self.apiUrl),
		getActiveProviders,
		&GetActiveProvidersResults{},
		callback,
	)
}


func post[R any](url string, args any, result R, callback apiCallback[R]) {
	requestBodyBytes, err := json.Marshal(args)
	if err != nil {
		callback.Error()
		return
	}

	client := defaultClient()
	r, err := client.Post(url, "text/json", bytes.NewReader(requestBodyBytes))
	if err != nil {
		callback.Error()
		return
	}

	responseBodyBytes, err := io.ReadAll(r.Body)
	r.Body.Close()

	err = json.Unmarshal(responseBodyBytes, &result)
	if err != nil {
		callback.Error()
		return
	}

	callback.Result(result)
}


// TODO post with extender

