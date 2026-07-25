package emaildelivery

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestSMTPConfigValidationAndClassification(t *testing.T) {
	if _, err := NewSMTPClient(SMTPConfig{}); err == nil {
		t.Fatal("missing host unexpectedly accepted")
	}
	if _, err := NewSMTPClient(SMTPConfig{Host: "smtp.example", Encryption: "tls"}); err == nil {
		t.Fatal("unsupported encryption unexpectedly accepted")
	}
	if _, err := NewSMTPClient(SMTPConfig{Host: "smtp.example", Encryption: "starttls"}); err != nil {
		t.Fatal(err)
	}
	if !IsTransient(classifySMTPError(&textproto.Error{Code: 451, Msg: "try later"})) {
		t.Fatal("SMTP 451 must be transient")
	}
	if IsTransient(classifySMTPError(&textproto.Error{Code: 550, Msg: "rejected"})) {
		t.Fatal("SMTP 550 must be permanent")
	}
	if !IsTransient(classifySMTPError(&net.DNSError{IsTimeout: true})) {
		t.Fatal("network timeout must be transient")
	}
	if IsTransient(errors.New("invalid message")) {
		t.Fatal("plain error unexpectedly classified transient")
	}
}

func TestSMTPClientRequiresAndUsesSTARTTLS(t *testing.T) {
	certificate, roots := testSMTPServerCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		reader := textproto.NewReader(bufio.NewReader(conn))
		writer := textproto.NewWriter(bufio.NewWriter(conn))
		if err := writer.PrintfLine("220 localhost test SMTP"); err != nil {
			serverErr <- err
			return
		}
		if _, err := reader.ReadLine(); err != nil {
			serverErr <- err
			return
		}
		if err := writer.PrintfLine("250-localhost"); err != nil {
			serverErr <- err
			return
		}
		if err := writer.PrintfLine("250-STARTTLS"); err != nil {
			serverErr <- err
			return
		}
		if err := writer.PrintfLine("250 AUTH PLAIN"); err != nil {
			serverErr <- err
			return
		}
		line, err := reader.ReadLine()
		if err != nil || line != "STARTTLS" {
			serverErr <- errors.New("client did not request STARTTLS")
			return
		}
		if err := writer.PrintfLine("220 ready"); err != nil {
			serverErr <- err
			return
		}
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		if err := tlsConn.Handshake(); err != nil {
			serverErr <- err
			return
		}
		reader = textproto.NewReader(bufio.NewReader(tlsConn))
		writer = textproto.NewWriter(bufio.NewWriter(tlsConn))
		if _, err := reader.ReadLine(); err != nil {
			serverErr <- err
			return
		}
		_ = writer.PrintfLine("250-localhost")
		_ = writer.PrintfLine("250 AUTH PLAIN")
		if line, err = reader.ReadLine(); err != nil || !strings.HasPrefix(line, "AUTH PLAIN ") {
			serverErr <- errors.New("client did not authenticate after STARTTLS")
			return
		}
		_ = writer.PrintfLine("235 authenticated")
		for _, prefix := range []string{"MAIL FROM:", "RCPT TO:"} {
			line, err = reader.ReadLine()
			if err != nil || !strings.HasPrefix(line, prefix) {
				serverErr <- errors.New("unexpected SMTP envelope")
				return
			}
			_ = writer.PrintfLine("250 ok")
		}
		if line, err = reader.ReadLine(); err != nil || line != "DATA" {
			serverErr <- errors.New("missing DATA command")
			return
		}
		_ = writer.PrintfLine("354 continue")
		body, err := reader.ReadDotBytes()
		if err != nil {
			serverErr <- err
			return
		}
		received <- string(body)
		_ = writer.PrintfLine("250 accepted")
		if _, err := reader.ReadLine(); err == nil {
			_ = writer.PrintfLine("221 bye")
		}
		serverErr <- nil
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewSMTPClient(SMTPConfig{
		Host: "localhost", Port: port, Username: "user", Password: "password",
		Encryption: "starttls", Timeout: 5 * time.Second,
		TLSConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := Message{
		EnvelopeFrom: "no-reply@example.com", Recipient: "user@example.com",
		Data: []byte("From: no-reply@example.com\r\nTo: user@example.com\r\nSubject: test\r\n\r\nhello\r\n"),
	}
	if err := client.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if body := <-received; !strings.Contains(body, "Subject: test") || !strings.Contains(body, "hello") {
		t.Fatalf("unexpected delivered message: %q", body)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func testSMTPServerCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("could not add test certificate")
	}
	return certificate, roots
}
