package sidecarhost

// The base URLs a block reaches other services at (§19.4): api's, whose internal gate the blocks with no database
// read (§8.9), the dns API block's, which the dns edges feed from (§12.6), and the sockets service's, which the edges
// find and register at (§9). warp hands a deployed block each one in its environment, from the env_vars of its
// fragment (warp/services/sidecar-blocks): https://<name>.${WARP_DOMAIN}, which warpctl writes out with the
// deployment's own domain when it renders the block's unit, so no fragment holds a production name. A block's flag
// reads the variable as its default: a deployed block needs no argument, a flag given wins, as run-local.sh's
// do, and a process with neither, a test's among them, has no URL and stays as it did without one.
//
// That api's internal gate answers only the site's own blocks is the lb's: warp refuses /internal/gate/, and the dns
// API's /dns/internal/, to every source outside the service's internal_routes allow list (W8; QD-027, QD-680).

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

const (
	// -api_url's default: api's base URL, or for a dns edge the dns API block's.
	ApiUrlEnvName = "UR_API_URL"
	// -sockets_url's default: the sockets service's base URL.
	SocketsUrlEnvName = "UR_SOCKETS_URL"
)

// A base URL flag whose default is an environment variable's value, read when the flag is read: a value given on the
// command line wins, and with none the environment's answers. Safe for concurrent reads once parsed, as a flag is.
type ServiceUrlFlag struct {
	envName string
	value   string
	set     bool
}

var _ flag.Value = (*ServiceUrlFlag)(nil)

// Registers on flags a flag named name whose default is envName's value.
func NewServiceUrlFlag(flags *flag.FlagSet, name string, envName string, usage string) *ServiceUrlFlag {
	self := &ServiceUrlFlag{envName: envName}
	flags.Var(self, name, fmt.Sprintf("%s; the default is $%s", usage, envName))
	return self
}

// The flag's value: the command line's, else the environment's, else empty.
func (self *ServiceUrlFlag) Url() string {
	if self == nil {
		return ""
	}
	if self.set {
		return self.value
	}
	return os.Getenv(self.envName)
}

// The variable the default is read from.
func (self *ServiceUrlFlag) EnvName() string {
	if self == nil {
		return ""
	}
	return self.envName
}

// See flag.Value. The flag package also calls it on a zero value, which answers empty.
func (self *ServiceUrlFlag) String() string {
	return self.Url()
}

// See flag.Value.
func (self *ServiceUrlFlag) Set(value string) error {
	self.value = value
	self.set = true
	return nil
}

// Refuses flags' flag named name unless it reads envName as its default, as a block's main registers it: what each
// block's test holds its URL flags to.
func CheckServiceUrlFlag(flags *flag.FlagSet, name string, envName string) error {
	registered := flags.Lookup(name)
	if registered == nil {
		return fmt.Errorf("no -%s", name)
	}
	serviceUrlFlag, ok := registered.Value.(*ServiceUrlFlag)
	if !ok {
		return errors.New("-" + name + " is not a ServiceUrlFlag, so its default reads no environment")
	}
	if serviceUrlFlag.EnvName() != envName {
		return fmt.Errorf("-%s reads $%s, not $%s", name, serviceUrlFlag.EnvName(), envName)
	}
	return nil
}
