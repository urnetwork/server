package handlers

import (
	"bytes"
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestAppleRootStoreFetchRequiresCompleteBundle(t *testing.T) {
	now := time.Now().UTC()
	chains := []*appleTestCertificateChain{
		newAppleTestCertificateChain(t, now, now.Add(24*time.Hour)),
		newAppleTestCertificateChain(t, now, now.Add(24*time.Hour)),
		newAppleTestCertificateChain(t, now, now.Add(24*time.Hour)),
	}

	mux := http.NewServeMux()
	for i, chain := range chains {
		path := "/root-" + string(rune('a'+i))
		rootDer := append([]byte(nil), chain.rootCertificate.Raw...)
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/pkix-cert")
			_, _ = w.Write(rootDer)
		})
	}
	mux.HandleFunc("/failure", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	rootServer := httptest.NewServer(mux)
	defer rootServer.Close()

	rootUrls := []string{
		rootServer.URL + "/root-a",
		rootServer.URL + "/root-b",
		rootServer.URL + "/root-c",
	}
	store := newAppleRootStore(
		[]*x509.Certificate{chains[0].rootCertificate},
		rootServer.Client(),
		rootUrls,
	)
	refreshedRoots, err := store.fetch(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(refreshedRoots), len(chains))
	for i, chain := range chains {
		connect.AssertEqual(t, bytes.Equal(refreshedRoots[i].Raw, chain.rootCertificate.Raw), true)
	}

	failedStore := newAppleRootStore(
		[]*x509.Certificate{chains[0].rootCertificate},
		rootServer.Client(),
		[]string{rootUrls[0], rootUrls[1], rootServer.URL + "/failure"},
	)
	_, err = failedStore.fetch(context.Background())
	connect.AssertEqual(t, err != nil, true)
	retainedRoots := failedStore.certificates()
	connect.AssertEqual(t, len(retainedRoots), 1)
	connect.AssertEqual(t, bytes.Equal(retainedRoots[0].Raw, chains[0].rootCertificate.Raw), true)
}
