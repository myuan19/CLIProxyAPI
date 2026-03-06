package grpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// Well-known Protobuf field numbers for Antigravity's GenerateContent RPC.
// These are derived from reverse engineering the Language Server's gRPC traffic.
// The field numbers correspond to the GenerateContentRequest and GenerateContentResponse
// messages used by the cloudcode-pa.googleapis.com service.
const (
	// GenerateContentRequest fields
	FieldModel            = 1  // string: model name
	FieldContents         = 2  // repeated Content: conversation messages
	FieldSystemInstruct   = 3  // Content: system instruction
	FieldTools            = 4  // repeated Tool definitions
	FieldGenerationConfig = 7  // GenerationConfig sub-message

	// Content fields (nested in GenerateContentRequest.contents)
	FieldContentParts = 1 // repeated Part
	FieldContentRole  = 2 // string: "user" or "model"

	// Part fields (nested in Content.parts)
	FieldPartText = 1 // string: text content

	// GenerateContentResponse fields
	FieldCandidates    = 1  // repeated Candidate
	FieldUsageMetadata = 3  // UsageMetadata
	FieldModelVersion  = 10 // string

	// Candidate fields
	FieldCandidateContent     = 1 // Content
	FieldCandidateFinishReson = 2 // enum FinishReason
	FieldCandidateIndex       = 3 // int32
)

// InterceptorConfig controls the interceptor behavior.
type InterceptorConfig struct {
	// SystemMode: "native" (preserve LS system prompt, replace user content),
	// "stealth" (strip Antigravity identity), "minimal" (minimal system prompt).
	SystemMode string
}

// PendingRequest represents a user's request waiting to be injected into
// the next LS → Google gRPC call.
type PendingRequest struct {
	Model          string
	Messages       []Message
	SystemPrompt   string
	Temperature    float64
	MaxTokens      int
	Stream         bool
	ResponseCh     chan *InterceptedResponse
	StreamCh       chan *StreamChunk
	DoneCh         chan struct{}
}

// Message is a simplified chat message for injection.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// InterceptedResponse holds the extracted response from a gRPC response.
type InterceptedResponse struct {
	Text         string
	FinishReason string
	Model        string
	Usage        *UsageInfo
	Error        error
}

// StreamChunk holds a single streaming chunk extracted from gRPC.
type StreamChunk struct {
	Text         string
	FinishReason string
	Error        error
}

// UsageInfo contains token usage information.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Interceptor manages request injection and response extraction
// for gRPC traffic flowing through the MITM proxy.
type Interceptor struct {
	cfg InterceptorConfig

	mu             sync.Mutex
	pendingRequest *PendingRequest
	activeRequest  *PendingRequest
}

// NewInterceptor creates a new gRPC interceptor.
func NewInterceptor(cfg InterceptorConfig) *Interceptor {
	if cfg.SystemMode == "" {
		cfg.SystemMode = "native"
	}
	return &Interceptor{cfg: cfg}
}

// InjectRequest queues a request for injection into the next LS → Google gRPC call.
// Returns channels for receiving the response.
func (i *Interceptor) InjectRequest(req *PendingRequest) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if req.ResponseCh == nil {
		req.ResponseCh = make(chan *InterceptedResponse, 1)
	}
	if req.StreamCh == nil {
		req.StreamCh = make(chan *StreamChunk, 100)
	}
	if req.DoneCh == nil {
		req.DoneCh = make(chan struct{})
	}

	i.pendingRequest = req
}

// HasPendingRequest returns true if there's a request waiting for injection.
func (i *Interceptor) HasPendingRequest() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.pendingRequest != nil
}

// InterceptRequest processes an outgoing request from LS to Google.
// If there's a pending user request, it modifies the gRPC payload to include
// the user's content instead of the LS's default/placeholder content.
func (i *Interceptor) InterceptRequest(req *http.Request, body []byte) ([]byte, error) {
	if !isGRPCRequest(req) {
		return body, nil
	}

	path := req.URL.Path
	if !isGenerateContentPath(path) {
		log.WithField("path", path).Debug("grpc interceptor: non-generate path, passthrough")
		return body, nil
	}

	i.mu.Lock()
	pending := i.pendingRequest
	if pending != nil {
		i.pendingRequest = nil
		i.activeRequest = pending
	}
	i.mu.Unlock()

	if pending == nil {
		return body, nil
	}

	log.WithField("model", pending.Model).Info("grpc interceptor: injecting request")

	contentType := req.Header.Get("Content-Type")

	if strings.Contains(contentType, "application/grpc") {
		return i.modifyGRPCBody(body, pending)
	}

	if strings.Contains(contentType, "application/json") {
		return i.modifyJSONBody(body, pending)
	}

	// If content type is proto binary without grpc framing
	if strings.Contains(contentType, "application/proto") || strings.Contains(contentType, "application/x-protobuf") {
		return i.modifyProtoBody(body, pending)
	}

	return body, nil
}

// InterceptResponse processes a response from Google to the LS.
func (i *Interceptor) InterceptResponse(req *http.Request, resp *http.Response, body []byte) ([]byte, error) {
	if !isGenerateContentPath(req.URL.Path) {
		return body, nil
	}

	i.mu.Lock()
	active := i.activeRequest
	i.mu.Unlock()

	if active == nil {
		return body, nil
	}

	contentType := resp.Header.Get("Content-Type")

	if strings.Contains(contentType, "application/grpc") {
		return i.extractGRPCResponse(body, active)
	}

	if strings.Contains(contentType, "application/json") {
		return i.extractJSONResponse(body, active)
	}

	return body, nil
}

// InterceptStreamChunk processes a streaming response chunk.
func (i *Interceptor) InterceptStreamChunk(req *http.Request, chunk []byte) ([]byte, error) {
	if !isGenerateContentPath(req.URL.Path) {
		return chunk, nil
	}

	i.mu.Lock()
	active := i.activeRequest
	i.mu.Unlock()

	if active == nil {
		return chunk, nil
	}

	text, finishReason, err := extractTextFromChunk(chunk)
	if err != nil {
		log.WithError(err).Debug("grpc interceptor: failed to extract text from stream chunk")
		return chunk, nil
	}

	if active.StreamCh != nil && (text != "" || finishReason != "") {
		select {
		case active.StreamCh <- &StreamChunk{
			Text:         text,
			FinishReason: finishReason,
		}:
		default:
		}
	}

	if finishReason != "" {
		i.finalizeActiveRequest(active)
	}

	return chunk, nil
}

func (i *Interceptor) finalizeActiveRequest(active *PendingRequest) {
	if active.StreamCh != nil {
		close(active.StreamCh)
	}
	if active.DoneCh != nil {
		close(active.DoneCh)
	}

	i.mu.Lock()
	if i.activeRequest == active {
		i.activeRequest = nil
	}
	i.mu.Unlock()
}

func (i *Interceptor) modifyGRPCBody(body []byte, pending *PendingRequest) ([]byte, error) {
	frame, consumed, err := ParseGRPCFrame(body)
	if err != nil {
		return body, fmt.Errorf("grpc interceptor: parse frame: %w", err)
	}

	modified, err := i.modifyProtoBody(frame.Data, pending)
	if err != nil {
		return body, err
	}

	result := EncodeGRPCFrame(modified, frame.Compressed)
	if consumed < len(body) {
		result = append(result, body[consumed:]...)
	}
	return result, nil
}

func (i *Interceptor) modifyProtoBody(data []byte, pending *PendingRequest) ([]byte, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return data, fmt.Errorf("grpc interceptor: parse proto: %w", err)
	}

	var result []byte

	for _, f := range fields {
		switch f.Number {
		case FieldContents:
			// Skip original contents; we'll add our own below.
			continue

		case FieldSystemInstruct:
			if i.cfg.SystemMode == "stealth" {
				continue
			}
			if i.cfg.SystemMode == "minimal" && pending.SystemPrompt != "" {
				systemContent := buildContentProto("user", pending.SystemPrompt)
				result = append(result, EncodeSubmessageField(FieldSystemInstruct, systemContent)...)
				continue
			}
			// "native" mode: keep original system instruction
			result = append(result, f.Raw...)

		case FieldModel:
			if pending.Model != "" {
				result = append(result, EncodeStringField(FieldModel, pending.Model)...)
			} else {
				result = append(result, f.Raw...)
			}

		default:
			result = append(result, f.Raw...)
		}
	}

	for _, msg := range pending.Messages {
		contentBytes := buildContentProto(msg.Role, msg.Content)
		result = append(result, EncodeSubmessageField(FieldContents, contentBytes)...)
	}

	if i.cfg.SystemMode == "stealth" && pending.SystemPrompt != "" {
		systemContent := buildContentProto("user", pending.SystemPrompt)
		result = append(result, EncodeSubmessageField(FieldSystemInstruct, systemContent)...)
	}

	return result, nil
}

func (i *Interceptor) modifyJSONBody(body []byte, pending *PendingRequest) ([]byte, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return body, nil
	}

	if pending.Model != "" {
		req["model"] = pending.Model
	}

	var contents []map[string]interface{}
	for _, msg := range pending.Messages {
		contents = append(contents, map[string]interface{}{
			"role": msg.Role,
			"parts": []map[string]interface{}{
				{"text": msg.Content},
			},
		})
	}
	req["contents"] = contents

	if pending.SystemPrompt != "" {
		if i.cfg.SystemMode == "stealth" || i.cfg.SystemMode == "minimal" {
			req["systemInstruction"] = map[string]interface{}{
				"parts": []map[string]interface{}{
					{"text": pending.SystemPrompt},
				},
			}
		}
	}

	modified, err := json.Marshal(req)
	if err != nil {
		return body, err
	}
	return modified, nil
}

func (i *Interceptor) extractGRPCResponse(body []byte, active *PendingRequest) ([]byte, error) {
	frame, _, err := ParseGRPCFrame(body)
	if err != nil {
		return body, nil
	}

	text, finishReason := extractFromProtoResponse(frame.Data)

	if active.ResponseCh != nil {
		select {
		case active.ResponseCh <- &InterceptedResponse{
			Text:         text,
			FinishReason: finishReason,
		}:
		default:
		}
	}

	i.finalizeActiveRequest(active)
	return body, nil
}

func (i *Interceptor) extractJSONResponse(body []byte, active *PendingRequest) ([]byte, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return body, nil
	}

	text := ""
	finishReason := ""

	if candidates, ok := resp["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]interface{}); ok {
						if t, ok := part["text"].(string); ok {
							text = t
						}
					}
				}
			}
			if fr, ok := candidate["finishReason"].(string); ok {
				finishReason = fr
			}
		}
	}

	if active.ResponseCh != nil {
		select {
		case active.ResponseCh <- &InterceptedResponse{
			Text:         text,
			FinishReason: finishReason,
		}:
		default:
		}
	}

	i.finalizeActiveRequest(active)
	return body, nil
}

func buildContentProto(role, text string) []byte {
	partText := EncodeStringField(FieldPartText, text)
	part := EncodeSubmessageField(FieldContentParts, partText)

	roleBytes := EncodeStringField(FieldContentRole, role)

	var content []byte
	content = append(content, part...)
	content = append(content, roleBytes...)
	return content
}

func extractFromProtoResponse(data []byte) (text string, finishReason string) {
	fields, err := ParseFields(data)
	if err != nil {
		return "", ""
	}

	candidateField := FindField(fields, FieldCandidates)
	if candidateField == nil {
		return "", ""
	}

	candidateData, err := GetBytesValue(*candidateField)
	if err != nil {
		return "", ""
	}

	candidateFields, err := ParseFields(candidateData)
	if err != nil {
		return "", ""
	}

	// Extract finish reason.
	if frField := FindField(candidateFields, FieldCandidateFinishReson); frField != nil {
		v, err := GetVarintValue(*frField)
		if err == nil {
			switch v {
			case 1:
				finishReason = "STOP"
			case 2:
				finishReason = "MAX_TOKENS"
			case 3:
				finishReason = "SAFETY"
			case 4:
				finishReason = "RECITATION"
			default:
				finishReason = fmt.Sprintf("UNKNOWN_%d", v)
			}
		}
	}

	// Extract text content.
	contentField := FindField(candidateFields, FieldCandidateContent)
	if contentField == nil {
		return text, finishReason
	}

	contentData, err := GetBytesValue(*contentField)
	if err != nil {
		return text, finishReason
	}

	contentFields, err := ParseFields(contentData)
	if err != nil {
		return text, finishReason
	}

	// Look through all parts for text.
	for _, partField := range FindAllFields(contentFields, FieldContentParts) {
		partData, err := GetBytesValue(partField)
		if err != nil {
			continue
		}
		partFields, err := ParseFields(partData)
		if err != nil {
			continue
		}
		if textField := FindField(partFields, FieldPartText); textField != nil {
			textBytes, err := GetBytesValue(*textField)
			if err == nil {
				text += string(textBytes)
			}
		}
	}

	return text, finishReason
}

func extractTextFromChunk(chunk []byte) (string, string, error) {
	// Try gRPC frame first.
	if len(chunk) >= 5 {
		frame, _, err := ParseGRPCFrame(chunk)
		if err == nil {
			text, fr := extractFromProtoResponse(frame.Data)
			return text, fr, nil
		}
	}

	// Try JSON (for SSE-style streaming).
	if len(chunk) > 0 && (chunk[0] == '{' || chunk[0] == '[') {
		var resp map[string]interface{}
		if err := json.Unmarshal(chunk, &resp); err == nil {
			text := ""
			finishReason := ""
			if candidates, ok := resp["candidates"].([]interface{}); ok && len(candidates) > 0 {
				if candidate, ok := candidates[0].(map[string]interface{}); ok {
					if content, ok := candidate["content"].(map[string]interface{}); ok {
						if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
							if part, ok := parts[0].(map[string]interface{}); ok {
								if t, ok := part["text"].(string); ok {
									text = t
								}
							}
						}
					}
					if fr, ok := candidate["finishReason"].(string); ok {
						finishReason = fr
					}
				}
			}
			return text, finishReason, nil
		}
	}

	// Try SSE data line.
	s := string(chunk)
	if strings.HasPrefix(s, "data: ") {
		jsonData := strings.TrimPrefix(s, "data: ")
		jsonData = strings.TrimSpace(jsonData)
		if jsonData == "[DONE]" {
			return "", "STOP", nil
		}
		return extractTextFromChunk([]byte(jsonData))
	}

	return "", "", fmt.Errorf("unrecognized chunk format")
}

func isGRPCRequest(req *http.Request) bool {
	ct := req.Header.Get("Content-Type")
	return strings.Contains(ct, "application/grpc") ||
		strings.Contains(ct, "application/proto") ||
		strings.Contains(ct, "application/json")
}

func isGenerateContentPath(path string) bool {
	return strings.Contains(path, "generateContent") ||
		strings.Contains(path, "streamGenerateContent") ||
		strings.Contains(path, "GenerateContent") ||
		strings.Contains(path, "StreamGenerateContent")
}
