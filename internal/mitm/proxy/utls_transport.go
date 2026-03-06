package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// UTLSProfile selects which TLS fingerprint to mimic.
type UTLSProfile string

const (
	ProfileChromium UTLSProfile = "chromium"
	ProfileGo      UTLSProfile = "go"
)

// NewUTLSTransport creates an http.RoundTripper that uses uTLS to mimic
// a specific browser's TLS fingerprint for HTTP/2 connections to upstream.
func NewUTLSTransport(profile UTLSProfile, serverName string) http.RoundTripper {
	if profile == ProfileGo || profile == "" {
		return &http2.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				NextProtos: []string{"h2"},
			},
		}
	}

	return &utlsH2Transport{
		profile:    profile,
		serverName: serverName,
	}
}

type utlsH2Transport struct {
	profile    UTLSProfile
	serverName string
	mu         sync.Mutex
	h2t        *http2.Transport
}

func (t *utlsH2Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if t.h2t == nil {
		t.h2t = &http2.Transport{
			DialTLSContext: t.dialTLSContext,
		}
	}
	h2t := t.h2t
	t.mu.Unlock()

	return h2t.RoundTrip(req)
}

func (t *utlsH2Transport) dialTLSContext(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
	dialer := &net.Dialer{}
	rawConn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	host := t.serverName
	if host == "" {
		host, _, _ = net.SplitHostPort(addr)
	}

	clientHelloID := t.getClientHelloID()

	tlsConn := utls.UClient(rawConn, &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2"},
		MinVersion: tls.VersionTLS12,
	}, clientHelloID)

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}

	state := tlsConn.ConnectionState()
	if state.NegotiatedProtocol != "h2" {
		tlsConn.Close()
		return nil, fmt.Errorf("utls: expected h2, got %q", state.NegotiatedProtocol)
	}

	return tlsConn, nil
}

func (t *utlsH2Transport) getClientHelloID() utls.ClientHelloID {
	switch t.profile {
	case ProfileChromium:
		return utls.HelloChrome_Auto
	default:
		return utls.HelloGolang
	}
}
