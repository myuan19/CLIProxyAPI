package lsclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
)

const (
	lsServicePath = "/exa.language_server_pb.LanguageServerService"
	connectProtoVersion = "1"
)

// Client communicates with the Language Server's ConnectRPC service using
// JSON format over HTTPS/H2. This simulates the Antigravity Extension's role.
type Client struct {
	mu          sync.Mutex
	baseURL     string
	csrfToken   string
	httpClient  *http.Client
	requestID   atomic.Int64
	accessToken string
}

// NewClient creates a ConnectRPC client that connects to the LS on the given port.
// The LS uses a self-signed TLS certificate, so we skip verification.
func NewClient(httpsPort int, csrfToken string) *Client {
	transport := &http2.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	c := &Client{
		baseURL:   fmt.Sprintf("https://127.0.0.1:%d", httpsPort),
		csrfToken: csrfToken,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
	c.requestID.Store(0)
	return c
}

func (c *Client) nextRequestID() int64 {
	return c.requestID.Add(1)
}

func (c *Client) metadata() map[string]interface{} {
	c.mu.Lock()
	token := c.accessToken
	c.mu.Unlock()
	return map[string]interface{}{
		"api_key":           token,
		"ide_name":          "antigravity",
		"ide_version":       "1.19.6",
		"extension_name":    "antigravity",
		"extension_version": "1.19.6",
		"request_id":        c.nextRequestID(),
	}
}

// SetAccessToken updates the OAuth access token used in ConnectRPC metadata.
func (c *Client) SetAccessToken(token string) {
	c.mu.Lock()
	c.accessToken = token
	c.mu.Unlock()
}

// callUnary sends a unary ConnectRPC request using JSON format.
func (c *Client) callUnary(ctx context.Context, method string, payload interface{}) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("lsclient: marshal %s: %w", method, err)
	}

	log.WithFields(log.Fields{"method": method, "body": truncate(string(body), 800)}).Info("lsclient: sending request")

	url := c.baseURL + lsServicePath + "/" + method

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", connectProtoVersion)
	if c.csrfToken != "" {
		req.Header.Set("x-codeium-csrf-token", c.csrfToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lsclient: %s request: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lsclient: %s read response: %w", method, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lsclient: %s HTTP %d: %s", method, resp.StatusCode, truncate(string(respBody), 200))
	}

	return respBody, nil
}

// Heartbeat sends a heartbeat to the LS to verify connectivity.
func (c *Client) Heartbeat(ctx context.Context) error {
	payload := map[string]interface{}{
		"metadata": c.metadata(),
	}
	_, err := c.callUnary(ctx, "Heartbeat", payload)
	if err != nil {
		log.WithError(err).Debug("lsclient: heartbeat failed")
	}
	return err
}

// GetCompletions sends a code completion request to trigger the LS to call Google.
// The document content is a "dummy" that the MITM interceptor will replace.
func (c *Client) GetCompletions(ctx context.Context, dummyText string) ([]byte, error) {
	doc := map[string]interface{}{
		"absolute_path":  "/tmp/cliproxy_dummy.txt",
		"relative_path":  "cliproxy_dummy.txt",
		"text":           dummyText,
		"editor_language": "plaintext",
		"language":       0,
		"cursor_offset":  len(dummyText),
		"line_ending":    "\n",
	}

	editorOptions := map[string]interface{}{
		"tab_size":      4,
		"insert_spaces": true,
	}

	payload := map[string]interface{}{
		"metadata":       c.metadata(),
		"document":       doc,
		"editor_options": editorOptions,
	}

	return c.callUnary(ctx, "GetCompletions", payload)
}

// StartCascade initiates a new Cascade (chat) session and returns the cascadeId.
func (c *Client) StartCascade(ctx context.Context) (string, error) {
	payload := map[string]interface{}{
		"metadata": c.metadata(),
		"source":   "cliproxy",
	}

	resp, err := c.callUnary(ctx, "StartCascade", payload)
	if err != nil {
		return "", err
	}

	var result struct {
		CascadeID string `json:"cascadeId"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("lsclient: parse StartCascade response: %w", err)
	}

	return result.CascadeID, nil
}

// SendCascadeMessage sends a user message to an existing Cascade session.
// This triggers the LS to call Google's GenerateContent API. The MITM
// interceptor will replace the dummy message with the real user content.
func (c *Client) SendCascadeMessage(ctx context.Context, cascadeID, dummyText, modelID string) ([]byte, error) {
	// The LS requires at least one item with user text to trigger a Google API call.
	// The actual content will be replaced by the MITM interceptor.
	userItem := map[string]interface{}{
		"humanMessage": map[string]interface{}{
			"rawContent": dummyText,
		},
	}

	enumModel := toModelEnum(modelID)

	payload := map[string]interface{}{
		"metadata":   c.metadata(),
		"cascadeId":  cascadeID,
		"items":      []interface{}{userItem},
		"cascadeConfig": map[string]interface{}{
			"plannerConfig": map[string]interface{}{
				"plannerTypeConfig": map[string]interface{}{
					"conversational": map[string]interface{}{},
				},
				"requestedModel": map[string]interface{}{
					"model": enumModel,
				},
			},
		},
	}

	return c.callUnary(ctx, "SendUserCascadeMessage", payload)
}

// WaitForReady polls the LS with heartbeats until it responds successfully
// or the context is cancelled.
func (c *Client) WaitForReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := c.Heartbeat(ctx); err == nil {
			log.Info("lsclient: LS is ready")
			return nil
		} else {
			log.WithError(err).Warn("lsclient: heartbeat attempt failed")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("lsclient: LS not ready after %v", timeout)
}

var modelEnumMap = map[string]int{
	"gemini-2.5-flash":                312,
	"gemini-2.5-flash-thinking":       313,
	"gemini-2.5-flash-thinking-tools": 329,
	"gemini-2.5-flash-lite":           330,
	"gemini-2.5-pro":                  246,
	"gemini-2.5-pro-eval":             331,
	"gemini-2.5-flash-image-preview":  332,
}

const defaultModelEnum = 312 // gemini-2.5-flash

func toModelEnum(modelID string) int {
	modelID = strings.TrimPrefix(modelID, "models/")
	if e, ok := modelEnumMap[modelID]; ok {
		return e
	}
	return defaultModelEnum
}

// GetCascadeModelConfigs queries the LS for available model configurations.
func (c *Client) GetCascadeModelConfigs(ctx context.Context) ([]byte, error) {
	payload := map[string]interface{}{
		"metadata": c.metadata(),
	}
	return c.callUnary(ctx, "GetCascadeModelConfigs", payload)
}

// SaveOAuthTokenInfo provides OAuth credentials to the LS directly.
func (c *Client) SaveOAuthTokenInfo(ctx context.Context, accessToken, refreshToken string) error {
	tokenInfo := map[string]interface{}{
		"accessToken":  accessToken,
		"refreshToken": refreshToken,
	}
	payload := map[string]interface{}{
		"metadata":       c.metadata(),
		"oauthTokenInfo": tokenInfo,
	}
	_, err := c.callUnary(ctx, "SaveOAuthTokenInfo", payload)
	return err
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
