package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/certmanager"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
)

// RequestInterceptor is called for each HTTP request flowing through the proxy.
// Returning a non-nil *http.Response short-circuits the request to upstream.
// Modifying the request in place alters what gets forwarded.
type RequestInterceptor func(req *http.Request, body []byte) (*http.Response, []byte, error)

// ResponseInterceptor is called for each HTTP response before it's returned to the client.
type ResponseInterceptor func(req *http.Request, resp *http.Response, body []byte) ([]byte, error)

// StreamInterceptor is called for streaming responses, processing chunks as they arrive.
type StreamInterceptor func(req *http.Request, chunk []byte) ([]byte, error)

// Config controls the MITM proxy behavior.
type Config struct {
	// CertManager provides TLS certificates for intercepted connections.
	CertManager *certmanager.Manager

	// ListenAddr is the address to listen on (default: ":0" for random port).
	ListenAddr string

	// TargetHost is the upstream host to forward traffic to (e.g. "cloudcode-pa.googleapis.com:443").
	TargetHost string

	// OnRequest is called for each request flowing through the proxy.
	OnRequest RequestInterceptor

	// OnResponse is called for each non-streaming response.
	OnResponse ResponseInterceptor

	// OnStreamChunk is called for each chunk in a streaming response.
	OnStreamChunk StreamInterceptor

	// UpstreamTLSConfig is the TLS config for connecting to the real upstream.
	// If nil, a default config is used.
	UpstreamTLSConfig *tls.Config

	// H2Profile controls the TLS fingerprint for upstream connections.
	// "chromium" uses uTLS to mimic Chrome, "go" uses native Go TLS.
	H2Profile UTLSProfile
}

// Proxy is an HTTP/2 MITM proxy that intercepts TLS connections from
// a Language Server and forwards them to the real Google API.
type Proxy struct {
	cfg           Config
	listener      net.Listener
	server        *http.Server
	port          int32
	wg            sync.WaitGroup
	closed        atomic.Bool
	transport     http.RoundTripper
	transportOnce sync.Once
}

// New creates a new MITM proxy with the given configuration.
func New(cfg Config) *Proxy {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.TargetHost == "" {
		cfg.TargetHost = "cloudcode-pa.googleapis.com:443"
	}
	return &Proxy{cfg: cfg}
}

// Start begins listening and serving MITM proxy connections.
func (p *Proxy) Start(ctx context.Context) error {
	tlsCfg := p.cfg.CertManager.TLSConfig()

	ln, err := tls.Listen("tcp", p.cfg.ListenAddr, tlsCfg)
	if err != nil {
		return fmt.Errorf("mitm proxy: listen: %w", err)
	}
	p.listener = ln

	addr := ln.Addr().(*net.TCPAddr)
	atomic.StoreInt32(&p.port, int32(addr.Port))

	h2srv := &http2.Server{
		MaxConcurrentStreams: 250,
		IdleTimeout:         120 * time.Second,
	}

	handler := http.HandlerFunc(p.handleRequest)

	p.server = &http.Server{
		Handler:     handler,
		IdleTimeout: 120 * time.Second,
	}

	// Configure HTTP/2 on the server.
	if err := http2.ConfigureServer(p.server, h2srv); err != nil {
		ln.Close()
		return fmt.Errorf("mitm proxy: configure h2: %w", err)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.server.Serve(ln); err != nil && !p.closed.Load() {
			log.WithError(err).Error("mitm proxy: serve error")
		}
	}()

	go func() {
		<-ctx.Done()
		p.Stop()
	}()

	log.WithField("port", addr.Port).Info("mitm proxy: listening")
	return nil
}

// Port returns the port the proxy is listening on.
func (p *Proxy) Port() int {
	return int(atomic.LoadInt32(&p.port))
}

// Stop gracefully shuts down the proxy.
func (p *Proxy) Stop() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if p.server != nil {
		p.server.Shutdown(ctx)
	}
	p.wg.Wait()
	log.Info("mitm proxy: stopped")
}

func (p *Proxy) handleRequest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.WithError(err).Error("mitm proxy: read request body")
		http.Error(w, "read body failed", http.StatusBadGateway)
		return
	}
	r.Body.Close()

	if p.cfg.OnRequest != nil {
		resp, modifiedBody, err := p.cfg.OnRequest(r, body)
		if err != nil {
			log.WithError(err).Error("mitm proxy: request interceptor error")
			http.Error(w, "interceptor error", http.StatusBadGateway)
			return
		}
		if resp != nil {
			copyResponse(w, resp)
			return
		}
		if modifiedBody != nil {
			body = modifiedBody
		}
	}

	isStreaming := isStreamingRequest(r)

	upstreamReq, err := p.buildUpstreamRequest(r, body)
	if err != nil {
		log.WithError(err).Error("mitm proxy: build upstream request")
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	transport := p.upstreamTransport()
	resp, err := transport.RoundTrip(upstreamReq)
	if err != nil {
		log.WithError(err).Error("mitm proxy: upstream round trip")
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if isStreaming && resp.StatusCode == http.StatusOK {
		p.handleStreamingResponse(w, r, resp)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Error("mitm proxy: read upstream response")
		http.Error(w, "read upstream response failed", http.StatusBadGateway)
		return
	}

	if p.cfg.OnResponse != nil {
		modified, err := p.cfg.OnResponse(r, resp, respBody)
		if err != nil {
			log.WithError(err).Error("mitm proxy: response interceptor error")
		} else if modified != nil {
			respBody = modified
		}
	}

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func (p *Proxy) handleStreamingResponse(w http.ResponseWriter, req *http.Request, resp *http.Response) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error("mitm proxy: ResponseWriter does not support Flusher")
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]

			if p.cfg.OnStreamChunk != nil {
				modified, intErr := p.cfg.OnStreamChunk(req, chunk)
				if intErr != nil {
					log.WithError(intErr).Error("mitm proxy: stream interceptor error")
				} else if modified != nil {
					chunk = modified
				}
			}

			w.Write(chunk)
			flusher.Flush()
		}
		if err != nil {
			if err != io.EOF {
				log.WithError(err).Debug("mitm proxy: stream read error")
			}
			break
		}
	}
}

func (p *Proxy) buildUpstreamRequest(r *http.Request, body []byte) (*http.Request, error) {
	targetHost := p.cfg.TargetHost
	if !strings.Contains(targetHost, ":") {
		targetHost += ":443"
	}
	host := strings.Split(targetHost, ":")[0]

	url := fmt.Sprintf("https://%s%s", host, r.URL.Path)
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, url, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return nil, err
	}

	for k, vv := range r.Header {
		kl := strings.ToLower(k)
		if kl == "host" || kl == "connection" || kl == "transfer-encoding" ||
			kl == "te" || kl == "trailer" || kl == "upgrade" {
			continue
		}
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}
	upReq.Host = host
	upReq.ContentLength = int64(len(body))

	return upReq, nil
}

func (p *Proxy) upstreamTransport() http.RoundTripper {
	p.transportOnce.Do(func() {
		host := strings.Split(p.cfg.TargetHost, ":")[0]
		p.transport = NewUTLSTransport(p.cfg.H2Profile, host)
	})
	return p.transport
}

func isStreamingRequest(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.Contains(r.URL.Path, "stream") ||
		strings.Contains(ct, "application/grpc")
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		io.Copy(w, resp.Body)
		resp.Body.Close()
	}
}
