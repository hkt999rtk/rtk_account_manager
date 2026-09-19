package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const maxServiceCRLBytes = 1 << 20

// The CRL is reloaded for every new TLS connection so an atomic file rotation
// revokes a service certificate without restarting Account Manager.
func serviceRegistrationTLSConfig(identity tls.Certificate, roots *x509.CertPool, crlPath string) (*tls.Config, error) {
	if _, err := loadServiceCRLs(crlPath); err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:           []tls.Certificate{identity},
		ClientCAs:              roots,
		ClientAuth:             tls.RequireAndVerifyClientCert,
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			return verifyServiceRegistrationCRL(state, crlPath, time.Now())
		},
	}, nil
}

// TLS verification runs only at handshake time. Recheck the CRL on every
// registration request so a revoked workload cannot renew its lease forever
// over an existing keep-alive connection.
func serviceRegistrationCRLMiddleware(next http.Handler, crlPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || verifyServiceRegistrationCRL(*r.TLS, crlPath, time.Now()) != nil {
			http.Error(w, "service certificate is not authorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func verifyServiceRegistrationCRL(state tls.ConnectionState, crlPath string, now time.Time) error {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) < 2 {
		return fmt.Errorf("verified service certificate chain is required")
	}
	leaf, issuer := state.VerifiedChains[0][0], state.VerifiedChains[0][1]
	crls, err := loadServiceCRLs(crlPath)
	if err != nil {
		return err
	}
	var selected *x509.RevocationList
	matchedIssuer := false
	for _, crl := range crls {
		if !bytes.Equal(crl.RawIssuer, issuer.RawSubject) {
			continue
		}
		if err := crl.CheckSignatureFrom(issuer); err != nil {
			continue
		}
		matchedIssuer = true
		if now.Before(crl.ThisUpdate) || !now.Before(crl.NextUpdate) {
			continue
		}
		if selected == nil || crl.ThisUpdate.After(selected.ThisUpdate) ||
			crl.ThisUpdate.Equal(selected.ThisUpdate) && crl.Number != nil && (selected.Number == nil || crl.Number.Cmp(selected.Number) > 0) {
			selected = crl
		}
	}
	if selected == nil {
		if matchedIssuer {
			return fmt.Errorf("service certificate CRL is not current")
		}
		return fmt.Errorf("no service certificate CRL from verified issuer")
	}
	for _, entry := range selected.RevokedCertificateEntries {
		if entry.SerialNumber.Cmp(leaf.SerialNumber) == 0 {
			return fmt.Errorf("service certificate is revoked")
		}
	}
	return nil
}

func loadServiceCRLs(path string) ([]*x509.RevocationList, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open service certificate CRL: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxServiceCRLBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxServiceCRLBytes {
		return nil, fmt.Errorf("invalid service certificate CRL file")
	}
	var result []*x509.RevocationList
	for len(bytes.TrimSpace(data)) > 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			if len(result) == 0 {
				crl, err := x509.ParseRevocationList(data)
				if err == nil {
					return []*x509.RevocationList{crl}, nil
				}
			}
			return nil, fmt.Errorf("invalid service certificate CRL encoding")
		}
		if block.Type != "X509 CRL" || len(block.Headers) != 0 {
			return nil, fmt.Errorf("invalid service certificate CRL PEM block")
		}
		crl, err := x509.ParseRevocationList(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse service certificate CRL: %w", err)
		}
		result = append(result, crl)
		data = rest
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("service certificate CRL is empty")
	}
	return result, nil
}
