package handlers

// Apple notification trust roots are seeded from the deployed config and
// refreshed from Apple's certificate authority endpoints. Refresh failures
// retain the last complete bundle, so verification never falls back to an
// empty or partially downloaded trust store.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

const (
	appleRootRefreshInterval       = 24 * time.Hour
	appleRootRetryInterval         = 30 * time.Minute
	appleRootMaxBytes        int64 = 128 * 1024
)

var appleRootUrls = []string{
	"https://www.apple.com/appleca/AppleIncRootCertificate.cer",
	"https://www.apple.com/certificateauthority/AppleRootCA-G2.cer",
	"https://www.apple.com/certificateauthority/AppleRootCA-G3.cer",
}

var configuredAppleRootCertificates = sync.OnceValue(func() []*x509.Certificate {
	rootCertificates, err := parseAppleRootCertificates(server.Config.RequireBytes("apple_roots.pem"))
	if err != nil {
		panic(err)
	}
	return rootCertificates
})

type appleRootStore struct {
	stateLock        sync.RWMutex
	rootCertificates []*x509.Certificate
	httpClient       *http.Client
	rootUrls         []string
}

var defaultAppleRootStore = sync.OnceValue(func() *appleRootStore {
	store := newAppleRootStore(
		configuredAppleRootCertificates(),
		server.DefaultHttpClient(),
		appleRootUrls,
	)
	go server.HandleError(store.refresh, func() {})
	return store
})

func newAppleRootStore(
	rootCertificates []*x509.Certificate,
	httpClient *http.Client,
	rootUrls []string,
) *appleRootStore {
	return &appleRootStore{
		rootCertificates: append([]*x509.Certificate(nil), rootCertificates...),
		httpClient:       httpClient,
		rootUrls:         append([]string(nil), rootUrls...),
	}
}

func (self *appleRootStore) certificates() []*x509.Certificate {
	self.stateLock.RLock()
	defer self.stateLock.RUnlock()
	return append([]*x509.Certificate(nil), self.rootCertificates...)
}

func (self *appleRootStore) refresh() {
	for {
		rootCertificates, err := self.fetch(context.Background())
		refreshInterval := appleRootRefreshInterval
		if err != nil {
			refreshInterval = appleRootRetryInterval
			glog.Errorf("[apple-roots] refresh failed; retaining configured roots: %v", err)
		} else {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.rootCertificates = rootCertificates
			}()
			glog.Infof("[apple-roots] refreshed %d trust roots", len(rootCertificates))
		}
		time.Sleep(refreshInterval)
	}
}

func (self *appleRootStore) fetch(ctx context.Context) ([]*x509.Certificate, error) {
	rootCertificates := make([]*x509.Certificate, 0, len(self.rootUrls))
	for _, rootUrl := range self.rootUrls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rootUrl, nil)
		if err != nil {
			return nil, err
		}
		res, err := self.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		certificateDer, readErr := io.ReadAll(io.LimitReader(res.Body, appleRootMaxBytes+1))
		closeErr := res.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s returned %s", rootUrl, res.Status)
		}
		if int64(len(certificateDer)) > appleRootMaxBytes {
			return nil, fmt.Errorf("%s certificate exceeds the size limit", rootUrl)
		}
		rootCertificate, err := x509.ParseCertificate(certificateDer)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", rootUrl, err)
		}
		if err := validateAppleRootCertificate(rootCertificate); err != nil {
			return nil, fmt.Errorf("validate %s: %w", rootUrl, err)
		}
		for _, existingCertificate := range rootCertificates {
			if bytes.Equal(existingCertificate.Raw, rootCertificate.Raw) {
				return nil, fmt.Errorf("%s duplicates another Apple root", rootUrl)
			}
		}
		rootCertificates = append(rootCertificates, rootCertificate)
	}
	if len(rootCertificates) != len(self.rootUrls) || len(rootCertificates) == 0 {
		return nil, errors.New("Apple root refresh returned an incomplete bundle")
	}
	return rootCertificates, nil
}

func parseAppleRootCertificates(pemBytes []byte) ([]*x509.Certificate, error) {
	rootCertificates := []*x509.Certificate{}
	for len(bytes.TrimSpace(pemBytes)) != 0 {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			return nil, errors.New("invalid configured Apple root certificate PEM")
		}
		pemBytes = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		rootCertificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse configured Apple root certificate: %w", err)
		}
		if err := validateAppleRootCertificate(rootCertificate); err != nil {
			return nil, err
		}
		rootCertificates = append(rootCertificates, rootCertificate)
	}
	if len(rootCertificates) == 0 {
		return nil, errors.New("no configured Apple root certificates")
	}
	return rootCertificates, nil
}

func validateAppleRootCertificate(rootCertificate *x509.Certificate) error {
	if !rootCertificate.IsCA || !rootCertificate.BasicConstraintsValid {
		return errors.New("Apple root certificate is not a valid CA")
	}
	if !bytes.Equal(rootCertificate.RawIssuer, rootCertificate.RawSubject) ||
		rootCertificate.CheckSignatureFrom(rootCertificate) != nil {
		return errors.New("Apple root certificate is not self-signed")
	}
	organizationIsApple := false
	for _, organization := range rootCertificate.Subject.Organization {
		if strings.EqualFold(organization, "Apple Inc.") {
			organizationIsApple = true
			break
		}
	}
	if !organizationIsApple {
		return errors.New("root certificate is not issued by Apple Inc.")
	}
	return nil
}
