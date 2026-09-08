package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// pkiSocketTransport preserves the configured remote origin as Host while
// transporting HTTP only over the private local socket. No network fallback.
type pkiSocketTransport struct {
	origin    *url.URL
	transport *http.Transport
}

func privatePKISocket(path string, requireSocket bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("absolute canonical PKI socket required")
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	st, ok := dir.Sys().(*syscall.Stat_t)
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("PKI socket parent must be private and owned by this UID")
	}
	if !requireSocket {
		return nil
	}
	socket, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok = socket.Sys().(*syscall.Stat_t)
	if socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm()&0077 != 0 || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("PKI socket must be private and owned by this UID")
	}
	return nil
}

func newPKISocketClient(origin *url.URL, socket string) (*http.Client, error) {
	if err := privatePKISocket(socket, false); err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := privatePKISocket(socket, true); err != nil {
			return nil, err
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 30 * time.Second}
	return &http.Client{Timeout: 30 * time.Second, Transport: &pkiSocketTransport{origin: origin, transport: transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func (t *pkiSocketTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != t.origin.Scheme || r.URL.Host != t.origin.Host || r.URL.User != nil || (r.Host != "" && r.Host != t.origin.Host) {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, fmt.Errorf("PKI socket origin changed")
	}
	request := r.Clone(r.Context())
	request.URL.Scheme = "http"
	request.Host = t.origin.Host
	return t.transport.RoundTrip(request)
}
func (t *pkiSocketTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }
