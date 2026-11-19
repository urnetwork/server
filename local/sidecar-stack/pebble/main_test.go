package main

// The local tier's Pebble in process on loopback: the management port hands out the root and the listener's
// certificate, a client trusting that certificate reads the ACME directory and a nonce, and a stop ends both ports.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestPebbleServesItsDirectoryAndRoots(t *testing.T) {
	settings := DefaultPebbleSettings()
	settings.AlwaysValid = true
	server, err := newPebbleServer(settings, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	acmeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	managementListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- server.serve(ctx, acmeListener, managementListener, settings.ShutdownTimeout)
	}()
	management := "http://" + managementListener.Addr().String()
	read := func(path string) []byte {
		t.Helper()
		response, err := http.Get(management + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s answered %d", path, response.StatusCode)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, path := range []string{"/roots/0", "/tls-cert"} {
		block, _ := pem.Decode(read(path))
		if block == nil {
			t.Fatalf("%s is no PEM", path)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	status := map[string]string{}
	if err := json.Unmarshal(read("/status"), &status); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(read("/tls-cert"))
	listenerCertificate, _ := x509.ParseCertificate(block.Bytes)
	roots := x509.NewCertPool()
	roots.AddCert(listenerCertificate)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}}}
	response, err := client.Get("https://" + acmeListener.Addr().String() + status["directory"])
	if err != nil {
		t.Fatal(err)
	}
	directory := map[string]any{}
	err = json.NewDecoder(response.Body).Decode(&directory)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	newNonce, _ := directory["newNonce"].(string)
	if newNonce == "" || directory["newOrder"] == nil || directory["newAccount"] == nil || directory["renewalInfo"] == nil {
		t.Fatalf("the directory %v", directory)
	}
	nonce, err := client.Head(newNonce)
	if err != nil {
		t.Fatal(err)
	}
	nonce.Body.Close()
	if nonce.Header.Get("Replay-Nonce") == "" {
		t.Fatal("newNonce answered no nonce")
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("the stop: %v", err)
	}
}
