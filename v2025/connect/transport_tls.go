package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"math/big"
	"net"

	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/tls"

	// "crypto/elliptic"
	// "crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	// "crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
)

// read tls host names from config
// load keys for the host names

func DefaultTransportTlsSettings() *TransportTlsSettings {
	return &TransportTlsSettings{
		EnableSelfSign: false,
	}
}

type TransportTlsSettings struct {
	EnableSelfSign  bool
	DefaultHostName string
}

type TransportTls struct {
	allowedHosts map[string]bool

	settings *TransportTlsSettings

	stateLock sync.Mutex

	tlsConfigs map[string]*tls.Config
}

func NewTransportTlsFromConfig(settings *TransportTlsSettings) (*TransportTls, error) {
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

	return NewTransportTls(allowedHosts, settings), nil
}

func NewTransportTls(allowedHosts map[string]bool, settings *TransportTlsSettings) *TransportTls {
	return &TransportTls{
		allowedHosts: allowedHosts,
		tlsConfigs:   map[string]*tls.Config{},
		settings:     settings,
	}
}

func (self *TransportTls) GetTlsConfigForClient(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
	hostName := clientHello.ServerName
	if hostName == "" {
		hostName = self.settings.DefaultHostName
	}
	return self.GetTlsConfig(hostName)
}

func (self *TransportTls) GetTlsConfig(hostName string) (*tls.Config, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	glog.Infof("[tls]try %s\n", hostName)

	if tlsConfig, ok := self.tlsConfigs[hostName]; ok {
		// note a nil entry might have been entered for a previous key miss
		if tlsConfig == nil {
			glog.Infof("[tls]did not find tls config for %s\n", hostName)
			return nil, fmt.Errorf("Missing key for server name \"%s\".", hostName)
		}
		glog.Infof("[tls]found tls config for %s\n", hostName)
		// make a copy
		tlsConfigCopy := *tlsConfig
		return &tlsConfigCopy, nil
	}

	selfSigned := func() (*tls.Config, error) {
		glog.Infof("[tls]creating self-signed cert for %s\n", hostName)

		organizationName := hostName
		validFrom := 180 * 24 * time.Hour
		validFor := 180 * 24 * time.Hour
		certPemBytes, keyPemBytes, err := selfSign(
			[]string{hostName},
			organizationName,
			validFrom,
			validFor,
		)
		if err != nil {
			return nil, err
		}
		// X509KeyPair
		cert, err := tls.X509KeyPair(certPemBytes, keyPemBytes)

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}

		self.tlsConfigs[hostName] = tlsConfig
		// make  acopy
		tlsConfigCopy := *tlsConfig
		return &tlsConfigCopy, nil
	}

	if !self.allowedHosts[hostName] {
		baseName, ok := baseName(hostName)
		if ok {
			ok = self.allowedHosts[fmt.Sprintf("*.%s", baseName)]
		}
		if !ok {
			if self.settings.EnableSelfSign {
				return selfSigned()
			} else {
				return nil, fmt.Errorf("Server name \"%s\" not allowed.", hostName)
			}
		}
	}

	findExplicit := func() (certPath string, keyPath string, ok bool) {
		certPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("tls/%s/%s.crt", hostName, hostName))
		if err != nil {
			return
		}
		keyPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("tls/%s/%s.key", hostName, hostName))
		if err != nil {
			return
		}
		certPath = certPaths[0]
		keyPath = keyPaths[0]
		ok = true
		return
	}

	findWildcard := func(baseName string) (certPath string, keyPath string, ok bool) {
		// baseName := strings.SplitN(hostName, ".", 2)[1]

		certPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("tls/star.%s/star.%s.crt", baseName, baseName))
		if err != nil {
			return
		}
		keyPaths, err := server.Vault.ResourcePaths(fmt.Sprintf("tls/star.%s/star.%s.key", baseName, baseName))
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
		baseName, ok := baseName(hostName)
		if ok {
			certPath, keyPath, ok = findWildcard(baseName)
		}
		if !ok {
			if self.settings.EnableSelfSign {
				return selfSigned()
			} else {
				glog.Infof("[tls]did not find tls config for %s\n", hostName)
				// add a missing entry to avoid future lookups
				self.tlsConfigs[hostName] = nil
				return nil, fmt.Errorf("Missing lookup key for server name \"%s\".", hostName)
			}
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

	glog.Infof("[tls]found tls config for %s\n", hostName)

	// make a copy
	tlsConfigCopy := *tlsConfig
	return &tlsConfigCopy, nil
}

func selfSign(
	hosts []string,
	organization string,
	validFrom time.Duration,
	validFor time.Duration,
) (
	certPemBytes []byte,
	keyPemBytes []byte,
	returnErr error,
) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	// priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		returnErr = err
		return
	}

	publicKey := func() any {
		switch k := any(privateKey).(type) {
		case *rsa.PrivateKey:
			return &k.PublicKey
		case *ecdsa.PrivateKey:
			return &k.PublicKey
		case ed25519.PrivateKey:
			return k.Public().(ed25519.PublicKey)
		default:
			return nil
		}
	}()

	// ECDSA, ED25519 and RSA subject keys should have the DigitalSignature
	// KeyUsage bits set in the x509.Certificate template
	keyUsage := x509.KeyUsageDigitalSignature
	// Only RSA subject keys should have the KeyEncipherment KeyUsage bits set. In
	// the context of TLS this KeyUsage is particular to RSA key exchange and
	// authentication.
	if _, isRSA := any(privateKey).(*rsa.PrivateKey); isRSA {
		keyUsage |= x509.KeyUsageKeyEncipherment
	}

	notBefore := time.Now().Add(-validFrom)
	notAfter := notBefore.Add(validFor)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		// log.Fatalf("Failed to generate serial number: %v", err)
		returnErr = err
		return
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{organization},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              keyUsage,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	// we hope the client is using tls1.3 which hides the self signed cert
	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey, privateKey)
	if err != nil {
		returnErr = err
		return
	}
	certPemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	privBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		returnErr = err
		return
	}
	keyPemBytes = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	return
}

func baseName(hostName string) (string, bool) {
	glog.V(2).Infof("[tls]base name %s\n", hostName)
	if net.ParseIP(hostName) != nil {
		return "", false
	}
	parts := strings.SplitN(hostName, ".", 2)
	if len(parts) < 2 {
		return "", false
	}
	return parts[1], true
}
