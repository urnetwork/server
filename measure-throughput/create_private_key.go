package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

func createPrivateKey(tempDir string) error {

	// Generate RSA private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	pksc8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	// Encode the private key to PEM format
	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: pksc8,
	}

	keyDir := filepath.Join(tempDir, "tls", "bringyour.com")

	err = os.MkdirAll(keyDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create key dir: %w", err)
	}

	// // Create a file to save the PEM encoded private key
	file, err := os.Create(filepath.Join(keyDir, "bringyour.com.key"))
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer file.Close()

	// Write the PEM encoded private key to the file
	err = pem.Encode(file, privateKeyPEM)
	if err != nil {
		return fmt.Errorf("failed to encode private key: %w", err)
	}

	return nil

}
