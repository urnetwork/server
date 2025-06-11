package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/urnetwork/server"
)

// read tls host names from config
// load keys for the host names

type TransportTls struct {
	allowedHosts map[string]bool

	stateLock sync.Mutex

	tlsConfigs map[string]*tls.Config
}

func NewTransportTlsFromConfig() (*TransportTls, error) {
	r, err := server.Config.SimpleResource("connect.yml")
	if err != nil {
		return nil, err
	}

	c := r.Parse()
	allowedHostsAny := c["allowed_hosts"].([]any)
	allowedHosts := map[string]bool{}
	for _, allowedHostAny := range allowedHostsAny {
		allowedHosts[allowedHostAny.(string)] = true
	}

	return NewTransportTls(allowedHosts), nil
}

func NewTransportTls(allowedHosts map[string]bool) *TransportTls {
	return &TransportTls{
		allowedHosts: allowedHosts,
		tlsConfigs:   map[string]*tls.Config{},
	}
}

func (self *TransportTls) GetTlsConfigForClient(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
	hostName := clientHello.ServerName
	return self.GetTlsConfig(hostName)
}

func (self *TransportTls) GetTlsConfig(hostName string) (*tls.Config, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if tlsConfig, ok := self.tlsConfigs[hostName]; ok {
		if tlsConfig == nil {
			return nil, fmt.Errorf("Missing key for server name \"%s\".", hostName)
		}
		// make a copy
		tlsConfigCopy := *tlsConfig
		return &tlsConfigCopy, nil
	}

	if !self.allowedHosts[hostName] {
		baseName := strings.SplitN(hostName, ".", 2)[1]
		if !self.allowedHosts[fmt.Sprintf("*.%s", baseName)] {
			return nil, fmt.Errorf("Server name \"%s\" not allowed.", hostName)
		}
	}

	findExplicit := func() (certPath string, keyPath string, ok bool) {
		certPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("all/tls/%s/%s.crt", hostName, hostName))
		if err != nil {
			return
		}
		keyPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("all/tls/%s/%s.key", hostName, hostName))
		if err != nil {
			return
		}
		certPath = certPaths[0]
		keyPath = keyPaths[0]
		ok = true
		return
	}

	findWildcard := func() (certPath string, keyPath string, ok bool) {
		baseName := strings.SplitN(hostName, ".", 2)[1]

		certPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("all/tls/star.%s/star.%s.crt", baseName, baseName))
		if err != nil {
			return
		}
		keyPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("all/tls/star.%s/star.%s.key", baseName, baseName))
		if err != nil {
			return
		}
		certPath = certPaths[0]
		keyPath = keyPaths[0]
		ok = true
		return
	}

	certPath, keyPath, ok := findExplicit()
	if !ok {
		certPath, keyPath, ok = findWildcard()
		if !ok {
			// add a missing entry to avoid future lookups
			self.tlsConfigs[hostName] = nil
			return nil, fmt.Errorf("Missing lookup key for server name \"%s\".", hostName)
		}
	}

	certPemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(certPemBytes, keyPemBytes)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	self.tlsConfigs[hostName] = tlsConfig
	// make  acopy
	tlsConfigCopy := *tlsConfig
	return &tlsConfigCopy, nil
}
