package model


// create proxy config
// return a proxy id

// remove proxy config

// get proxy config
//    by proxyId
//    by clientId


type ProxyDeviceMode int

const (
	ProxyDeviceModeDevice ProxyDeviceMode = 1
)

type ProxyDeviceConnection struct {
	ProxyId server.Id `json:"proxy_id"`
	ClientId server.Id `json:"client_id"`
	InstanceId server.Id `json:"instance_id"`
}

type ProxyDeviceConfig struct {
	ProxyDeviceConnection
	ProxyDeviceMode ProxyDeviceMode `json:"proxy_device_mode"`
	LockSubnets []netip.Prefix `json:"lock_subnets"`
	HttpRequireAuth bool `json:"http_require_auth"`

	InitialDeviceState *ProxyDeviceState `json:"initial_device_state"`
}

type ProxyDeviceState struct {
	Location *sdk.ConnectLocation `json:"location"`

	PerformanceProfile *sdk.PerformanceProfile `json:"performance_profile"`
}


// FIXME use a proxy jwt as the url


func GetProxyDeviceConnection(proxyId server.Id) *ProxyDeviceConnection {

}

func GetProxyDeviceConnectionForClient(clientId server.Id) *ProxyDeviceConnection {
	
}


func CreateProxyDeviceConfig(proxyDeviceConfig *ProxyDeviceConfig) {

}

func RemoveProxyDeviceConfig(proxyId server.Id) {

}

func GetProxyDeviceConfig(proxyId server.Id) *ProxyDeviceConfig {

}

func GetProxyDeviceConfigByClientId(clientId server.Id, instanceId server.Id) *ProxyDeviceConfig {

}

// FIXME use a mapping of country code to connection location
func GetConnectLocationForCountryCode(countryCode string) *ConnectLocation {

}



CREATE TABLE proxy_device_config (
	proxy_id UUID,
	client_id UUID,
	instance_id UUID,
	config_json text,

	PRIMARY KEY (proxy_id)
)

CREATE INDEX proxy_device_config_client_id_instance_id ON proxy_device_config (client_id, instance_id)





