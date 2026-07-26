package emaildelivery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

type SMTPConfig struct {
	Host       string
	Port       string
	Username   string
	Password   string
	Encryption string
	Timeout    time.Duration
	TLSConfig  *tls.Config
}

type SMTPClient struct {
	config SMTPConfig
}

type DeliveryError struct {
	Err       error
	Transient bool
}

func (e *DeliveryError) Error() string { return e.Err.Error() }
func (e *DeliveryError) Unwrap() error { return e.Err }

func NewSMTPClient(config SMTPConfig) (*SMTPClient, error) {
	config.Host = strings.TrimSpace(config.Host)
	config.Port = strings.TrimSpace(config.Port)
	config.Encryption = strings.ToLower(strings.TrimSpace(config.Encryption))
	if config.Host == "" {
		return nil, errors.New("SMTP_HOST is required")
	}
	if config.Port == "" {
		config.Port = "587"
	}
	switch config.Encryption {
	case "starttls", "none":
	default:
		return nil, errors.New("SMTP_ENCRYPTION must be starttls or none")
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	return &SMTPClient{config: config}, nil
}

func (s *SMTPClient) Send(ctx context.Context, message Message) error {
	address := net.JoinHostPort(s.config.Host, s.config.Port)
	conn, err := (&net.Dialer{Timeout: s.config.Timeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return classifySMTPError(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(s.config.Timeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return classifySMTPError(err)
	}
	client, err := smtp.NewClient(conn, s.config.Host)
	if err != nil {
		return classifySMTPError(err)
	}
	defer client.Close()

	if s.config.Encryption == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return &DeliveryError{Err: errors.New("SMTP server does not advertise STARTTLS"), Transient: false}
		}
		tlsConfig := s.config.TLSConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		} else {
			tlsConfig = tlsConfig.Clone()
		}
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = s.config.Host
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return classifySMTPError(err)
		}
	}
	if s.config.Username != "" || s.config.Password != "" {
		if err := client.Auth(smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)); err != nil {
			return classifySMTPError(err)
		}
	}
	if err := client.Mail(message.EnvelopeFrom); err != nil {
		return classifySMTPError(err)
	}
	if err := client.Rcpt(message.Recipient); err != nil {
		return classifySMTPError(err)
	}
	writer, err := client.Data()
	if err != nil {
		return classifySMTPError(err)
	}
	if _, err := writer.Write(message.Data); err != nil {
		_ = writer.Close()
		return classifySMTPError(err)
	}
	if err := writer.Close(); err != nil {
		return classifySMTPError(err)
	}
	if err := client.Quit(); err != nil {
		return classifySMTPError(err)
	}
	return nil
}

func IsTransient(err error) bool {
	var deliveryErr *DeliveryError
	return errors.As(err, &deliveryErr) && deliveryErr.Transient
}

func classifySMTPError(err error) error {
	if err == nil {
		return nil
	}
	var protocolErr *textproto.Error
	if errors.As(err, &protocolErr) {
		return &DeliveryError{Err: err, Transient: protocolErr.Code >= 400 && protocolErr.Code < 500}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return &DeliveryError{Err: err, Transient: true}
	}
	if errors.Is(err, io.EOF) {
		return &DeliveryError{Err: err, Transient: true}
	}
	return &DeliveryError{Err: fmt.Errorf("SMTP delivery failed: %w", err), Transient: false}
}
