package extserver

import (
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server implements a minimal Extension Server that the Language Server
// connects back to for callbacks (ExtensionServerService). In the real
// Antigravity IDE, the Extension runs this server; we simulate it to
// keep the LS happy.
type Server struct {
	mu        sync.Mutex
	listener  net.Listener
	port      int
	csrfToken string
	server    *http.Server

	accessToken  string
	refreshToken string

	lsStartedCh chan struct{}
	lsStarted    bool
}

// New creates a new Extension Server with a random CSRF token.
func New() *Server {
	return &Server{
		csrfToken:    uuid.New().String(),
		lsStartedCh: make(chan struct{}, 1),
	}
}

// Port returns the port the server is listening on.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// CSRFToken returns the CSRF token for LS → Extension Server validation.
func (s *Server) CSRFToken() string {
	return s.csrfToken
}

// LSStartedCh returns a channel that is closed when the LS signals it has started.
func (s *Server) LSStartedCh() <-chan struct{} {
	return s.lsStartedCh
}

// Start begins listening for LS callbacks on a random local port.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.listener = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRequest)

	// Use h2c (HTTP/2 cleartext) — the LS connects to Extension Server without TLS
	h2s := &http2.Server{}
	handler := h2c.NewHandler(mux, h2s)

	s.server = &http.Server{Handler: handler}

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Error("extserver: serve error")
		}
	}()

	log.WithFields(log.Fields{
		"port": s.port,
		"csrf": s.csrfToken[:8] + "...",
	}).Info("extserver: started")

	return nil
}

// Stop shuts down the Extension Server.
func (s *Server) Stop() {
	if s.server != nil {
		s.server.Close()
	}
	log.Info("extserver: stopped")
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	csrf := r.Header.Get("x-codeium-csrf-token")
	if csrf != "" && csrf != s.csrfToken {
		log.WithField("path", r.URL.Path).Warn("extserver: invalid CSRF token")
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	path := r.URL.Path
	reqCT := r.Header.Get("Content-Type")

	log.WithFields(log.Fields{"path": path, "ct": reqCT}).Info("extserver: received callback")

	// Server-streaming RPCs: request is application/proto but response must be application/connect+proto
	pathLower := strings.ToLower(path)
	if strings.Contains(pathLower, "subscribetounifiedstatesynctopic") {
		s.respondConnectProto(w, r)
		return
	}

	if strings.Contains(pathLower, "languageserverstarted") {
		s.mu.Lock()
		if !s.lsStarted {
			s.lsStarted = true
			close(s.lsStartedCh)
		}
		s.mu.Unlock()
		log.Info("extserver: LS reported started")
	}

	// Consume the request body
	io.Copy(io.Discard, r.Body)
	r.Body.Close()

	// Respond based on Content-Type
	if strings.Contains(reqCT, "application/proto") || strings.Contains(reqCT, "application/connect+proto") {
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte{})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
}

// SetTokens sets the OAuth tokens for state sync responses.
func (s *Server) SetTokens(accessToken, refreshToken string) {
	s.mu.Lock()
	s.accessToken = accessToken
	s.refreshToken = refreshToken
	s.mu.Unlock()
}

// respondConnectProto responds with ConnectRPC proto format.
// For streaming RPCs (subscribeToUnifiedStateSyncTopic), we send an
// initial response frame and then a proper end-of-stream trailer.
func (s *Server) respondConnectProto(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)

	// This method is only called for subscribeToUnifiedStateSyncTopic
	s.handleStateSyncSubscription(w, r)
}

func (s *Server) handleStateSyncSubscription(w http.ResponseWriter, r *http.Request) {
	reqBody, _ := io.ReadAll(r.Body)
	r.Body.Close()

	// Proto definitions (from extension_server.proto + unified_state_sync.proto):
	//   message UnifiedStateSyncUpdate {
	//     oneof update_type {
	//       Topic initial_state = 1;
	//       AppliedUpdate applied_update = 2;
	//     }
	//   }
	//   message Topic { map<string, Row> data = 1; }
	//   message Row { string value = 1; int64 e_tag = 2; }

	s.mu.Lock()
	accessToken := s.accessToken
	refreshToken := s.refreshToken
	s.mu.Unlock()

	var topicData []byte
	if accessToken != "" && containsBytes(reqBody, []byte("uss-oauth")) {
		// OAuthTokenInfo proto:
		//   string access_token = 1;
		//   string token_type = 2;
		//   string refresh_token = 3;
		//   google.protobuf.Timestamp expiry = 4;
		//   bool is_gcp_tos = 5;
		var oauthProto []byte
		oauthProto = append(oauthProto, encodeStringField(1, accessToken)...)
		oauthProto = append(oauthProto, encodeStringField(2, "Bearer")...)
		if refreshToken != "" {
			oauthProto = append(oauthProto, encodeStringField(3, refreshToken)...)
		}
		// Timestamp: { int64 seconds = 1; } — set to 1 hour from now
		expSeconds := time.Now().Add(1 * time.Hour).Unix()
		timestamp := encodeVarintField(1, uint64(expSeconds))
		oauthProto = append(oauthProto, encodeSubmessage(4, timestamp)...)

		// Row.value = base64(OAuthTokenInfo binary)
		rowValue := base64.StdEncoding.EncodeToString(oauthProto)
		topicData = append(topicData, buildMapEntryRow("oauthTokenInfoSentinelKey", rowValue)...)
		log.Info("extserver: providing OAuth token via state sync")
	}

	// UnifiedStateSyncUpdate { initial_state (field 1) = Topic { data entries } }
	response := encodeSubmessage(1, topicData)
	writeConnectProtoFrame(w, response)
	writeConnectProtoEndStream(w, []byte("{}"))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func containsBytes(data, needle []byte) bool {
	return len(data) > 0 && len(needle) > 0 && strings.Contains(string(data), string(needle))
}

// buildMapEntryRow builds a protobuf map<string, Row> entry for Topic.data (field 1).
// MapEntry: { string key = 1; Row value = 2; }
// Row:      { string value = 1; }
func buildMapEntryRow(key string, value string) []byte {
	row := encodeStringField(1, value)
	var entry []byte
	entry = append(entry, encodeStringField(1, key)...)
	entry = append(entry, encodeSubmessage(2, row)...)
	return encodeSubmessage(1, entry)
}

func encodeVarintField(fieldNum int, val uint64) []byte {
	tag := byte((fieldNum << 3) | 0) // wire type 0 = varint
	result := []byte{tag}
	for val > 0x7f {
		result = append(result, byte(val&0x7f)|0x80)
		val >>= 7
	}
	result = append(result, byte(val))
	return result
}

func encodeStringField(fieldNum int, s string) []byte {
	return encodeBytesField(fieldNum, []byte(s))
}

func encodeBytesField(fieldNum int, data []byte) []byte {
	tag := byte((fieldNum << 3) | 2)
	result := []byte{tag}
	n := len(data)
	for n > 0x7f {
		result = append(result, byte(n&0x7f)|0x80)
		n >>= 7
	}
	result = append(result, byte(n))
	result = append(result, data...)
	return result
}

func writeConnectProtoFrame(w http.ResponseWriter, data []byte) {
	// Connect streaming frame: [flags:1byte] [length:4byte big-endian] [data]
	header := make([]byte, 5)
	header[0] = 0x00 // data frame
	binary.BigEndian.PutUint32(header[1:5], uint32(len(data)))
	w.Write(header)
	w.Write(data)
}

func writeConnectProtoEndStream(w http.ResponseWriter, trailers []byte) {
	header := make([]byte, 5)
	header[0] = 0x02 // end-of-stream frame
	binary.BigEndian.PutUint32(header[1:5], uint32(len(trailers)))
	w.Write(header)
	w.Write(trailers)
}

func encodeSubmessage(fieldNum int, data []byte) []byte {
	tag := byte((fieldNum << 3) | 2) // wire type 2 = length-delimited
	result := []byte{tag}
	// varint encode length
	n := len(data)
	for n > 0x7f {
		result = append(result, byte(n&0x7f)|0x80)
		n >>= 7
	}
	result = append(result, byte(n))
	result = append(result, data...)
	return result
}
