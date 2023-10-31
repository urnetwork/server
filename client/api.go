package client


import (

	// "bringyour.com/bringyour/model"
)


type apiCallback[R any] interface {
	Result(result R)
	Error()
}


type ApiClient struct {
	apiUrl string

	byJwt string
}

// TODO manage extenders

func NewApiClient(apiUrl string) *ApiClient {
	// FIXME
	return nil
}

// this gets attached to api calls that need it
func (self *ApiClient) SetByJwt(byJwt string) {

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

func (self *ApiClient) AuthLogin(authLogin *AuthLoginArgs, callback AuthLoginCallback) {

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

func (self *ApiClient) AuthLoginWithPassword(authLoginWithPassword *AuthLoginWithPasswordArgs, callback AuthLoginWithPasswordCallback) {

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

func (self *ApiClient) AuthVerify(authVerify *AuthVerifyArgs, callback AuthVerifyCallback) {

}



type NetworkCheckCallback apiCallback[*NetworkCheckResult]

type NetworkCheckArgs struct {
	NetworkName string  `json:"network_name"`
}

type NetworkCheckResult struct {
	Available bool  `json:"available"`
}

func (self *ApiClient) NetworkCheck(networkCheck *NetworkCheckArgs, callback NetworkCheckCallback) {

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

func (self *ApiClient) NetworkCreate(networkCreate *NetworkCreateArgs, callback NetworkCreateCallback) {

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

func (self *ApiClient) AuthNetworkClient(authNetworkClient *NetworkCreateArgs, callback AuthNetworkClientCallback) {

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



func (self *ApiClient) FindLocations(findLocations *FindLocationsArgs, callback FindLocationsCallback) {

}



// FIXME Need a model call for below
// give a locationId or a locationGroupId, and the number of parallel

// n parallel
func (self *ApiClient) ChooseDestinations() {

}






