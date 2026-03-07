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
	FieldCandidateFinishReason = 2 // enum FinishReason
	FieldCandidateIndex       = 3 // int32
)

// Bidirectional tool name mapping between AG tool names and standard/client
// tool names. In native mode, responses may contain AG-specific tool names
// that need mapping back to client equivalents.
var agToolNameMap = map[string]string{
	"view_file":        "read_file",
	"write_to_file":    "write_file",
	"edit_file":        "edit_file",
	"run_command":      "execute_command",
	"search_files":     "search",
	"list_directory":   "list_dir",
	"browser_subagent": "browser",
	"generate_image":   "generate_image",
}

var clientToolNameMap = map[string]string{}

func init() {
	for ag, client := range agToolNameMap {
		clientToolNameMap[client] = ag
	}
}

// InterceptorConfig controls the interceptor behavior.
type InterceptorConfig struct {
	// SystemMode: "native" (preserve LS system prompt, replace user content),
	// "stealth" (strip Antigravity identity), "minimal" (minimal system prompt).
	SystemMode string

	// DummyPromptText is the placeholder text sent via cascade that gets
	// replaced with the real user input in "native" mode.
	DummyPromptText string

	// SensitiveWords lists client names to obfuscate with zero-width chars
	// to reduce detection in server-side logs (e.g. "Cursor", "OpenCode").
	SensitiveWords []string
}

// MapToolNameToClient maps an AG tool name to its client equivalent.
func MapToolNameToClient(agName string) string {
	if mapped, ok := agToolNameMap[agName]; ok {
		return mapped
	}
	return agName
}

// MapToolNameToAG maps a client tool name to its AG equivalent.
func MapToolNameToAG(clientName string) string {
	if mapped, ok := clientToolNameMap[clientName]; ok {
		return mapped
	}
	return clientName
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
	finalizeOnce   sync.Once

	accumulatedText  string
	lastFinishReason string
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
// If a previous pending request exists, it is cancelled with an error.
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

	if old := i.pendingRequest; old != nil {
		old.finalizeOnce.Do(func() {
			if old.ResponseCh != nil {
				select {
				case old.ResponseCh <- &InterceptedResponse{Error: fmt.Errorf("superseded by new request")}:
				default:
				}
			}
			if old.StreamCh != nil {
				close(old.StreamCh)
			}
			if old.DoneCh != nil {
				close(old.DoneCh)
			}
		})
	}

	i.pendingRequest = req
}

// HasPendingRequest returns true if there's a request waiting for injection.
func (i *Interceptor) HasPendingRequest() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.pendingRequest != nil
}

// CancelPending cancels the pending (not yet activated) request with an error.
// This should be called when the trigger fails (e.g., LS client not ready).
func (i *Interceptor) CancelPending(err error) {
	i.mu.Lock()
	pending := i.pendingRequest
	i.pendingRequest = nil
	i.mu.Unlock()

	if pending == nil {
		return
	}

	pending.finalizeOnce.Do(func() {
		if pending.ResponseCh != nil {
			select {
			case pending.ResponseCh <- &InterceptedResponse{Error: err}:
			default:
			}
		}
		if pending.StreamCh != nil {
			close(pending.StreamCh)
		}
		if pending.DoneCh != nil {
			close(pending.DoneCh)
		}
	})
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

	// Handle error responses (4xx, 5xx) by forwarding them to the pending request.
	if resp.StatusCode >= 400 {
		errMsg := fmt.Sprintf("upstream error %d: %s", resp.StatusCode, truncateBytes(body, 300))
		log.WithField("status", resp.StatusCode).Warn("grpc interceptor: upstream error response")
		active.finalizeOnce.Do(func() {
			if active.ResponseCh != nil {
				select {
				case active.ResponseCh <- &InterceptedResponse{Error: fmt.Errorf(errMsg)}:
				default:
				}
			}
			if active.StreamCh != nil {
				close(active.StreamCh)
			}
			if active.DoneCh != nil {
				close(active.DoneCh)
			}
		})
		i.mu.Lock()
		if i.activeRequest == active {
			i.activeRequest = nil
		}
		i.mu.Unlock()
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

	if text != "" {
		active.accumulatedText += text
	}
	if finishReason != "" {
		active.lastFinishReason = finishReason
	}

	if active.StreamCh != nil && (text != "" || finishReason != "") {
		select {
		case active.StreamCh <- &StreamChunk{
			Text:         text,
			FinishReason: finishReason,
		}:
		default:
			log.Warn("grpc interceptor: StreamCh full, dropping chunk")
		}
	}

	if finishReason != "" {
		i.finalizeActiveRequest(active)
	}

	return chunk, nil
}

func (i *Interceptor) finalizeActiveRequest(active *PendingRequest) {
	active.finalizeOnce.Do(func() {
		if active.ResponseCh != nil && active.accumulatedText != "" {
			fr := active.lastFinishReason
			if fr == "" {
				fr = "STOP"
			}
			select {
			case active.ResponseCh <- &InterceptedResponse{
				Text:         active.accumulatedText,
				FinishReason: fr,
			}:
			default:
			}
		}
		if active.StreamCh != nil {
			close(active.StreamCh)
		}
		if active.DoneCh != nil {
			close(active.DoneCh)
		}
	})

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
	switch i.cfg.SystemMode {
	case "native":
		return i.nativeJSONReplace(body, pending)
	default:
		return i.fullJSONReplace(body, pending)
	}
}

// nativeJSONReplace does a precise in-place replacement of the dummy prompt
// while preserving the LS's full agent framework, tools, system instructions,
// and generation config. This matches ZeroGravity's "native" approach.
func (i *Interceptor) nativeJSONReplace(body []byte, pending *PendingRequest) ([]byte, error) {
	var userText strings.Builder
	for idx, msg := range pending.Messages {
		if idx > 0 {
			userText.WriteString("\n\n")
		}
		userText.WriteString(msg.Content)
	}

	replacement := userText.String()
	if len(i.cfg.SensitiveWords) > 0 {
		replacement = obfuscateSensitiveWords(replacement, i.cfg.SensitiveWords)
	}

	dummy := i.cfg.DummyPromptText
	if dummy == "" {
		dummy = "Say hello."
	}

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return body, nil
	}

	target := req
	if inner, ok := req["request"].(map[string]interface{}); ok {
		target = inner
	}

	if i.replaceDummyInContents(target, dummy, replacement) {
		mergeToolDeclarations(target)
		enforceMaxOutputTokens(target)

		modified, err := json.Marshal(req)
		if err != nil {
			return body, err
		}
		log.WithField("originalLen", len(body)).WithField("modifiedLen", len(modified)).Info("grpc interceptor: native JSON replace done")
		return modified, nil
	}

	log.Warn("grpc interceptor: dummy prompt not found, falling back to full replace")
	return i.fullJSONReplace(body, pending)
}

// replaceDummyInContents walks the contents array from the end and replaces
// the dummy prompt text or fills an empty <USER_REQUEST> block with the real
// user input. The LS agent framework may leave <USER_REQUEST></USER_REQUEST>
// empty when the cascade text isn't forwarded into the request body.
func (i *Interceptor) replaceDummyInContents(target map[string]interface{}, dummy, replacement string) bool {
	contents, ok := target["contents"].([]interface{})
	if !ok {
		return false
	}

	emptyUserRequest := "<USER_REQUEST>\n\n</USER_REQUEST>"
	filledUserRequest := "<USER_REQUEST>\n" + replacement + "\n</USER_REQUEST>"

	for ci := len(contents) - 1; ci >= 0; ci-- {
		entry, ok := contents[ci].(map[string]interface{})
		if !ok {
			continue
		}
		parts, ok := entry["parts"].([]interface{})
		if !ok {
			continue
		}
		for _, part := range parts {
			p, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			text, ok := p["text"].(string)
			if !ok {
				continue
			}

			// Strategy 1: exact dummy prompt match
			if text == dummy || strings.Contains(text, dummy) {
				p["text"] = strings.Replace(text, dummy, replacement, 1)
				log.WithField("contentIdx", ci).Info("grpc interceptor: replaced dummy prompt in-place")
				return true
			}

			// Strategy 2: fill empty <USER_REQUEST> block
			if strings.Contains(text, emptyUserRequest) {
				p["text"] = strings.Replace(text, emptyUserRequest, filledUserRequest, 1)
				log.WithField("contentIdx", ci).Info("grpc interceptor: filled empty USER_REQUEST block")
				return true
			}
		}
	}
	return false
}

// fullJSONReplace replaces the entire contents array (stealth/minimal modes).
func (i *Interceptor) fullJSONReplace(body []byte, pending *PendingRequest) ([]byte, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return body, nil
	}

	var contents []map[string]interface{}
	for _, msg := range pending.Messages {
		text := msg.Content
		if len(i.cfg.SensitiveWords) > 0 {
			text = obfuscateSensitiveWords(text, i.cfg.SensitiveWords)
		}
		contents = append(contents, map[string]interface{}{
			"role": msg.Role,
			"parts": []map[string]interface{}{
				{"text": text},
			},
		})
	}

	inner, hasInner := req["request"].(map[string]interface{})
	if hasInner {
		inner["contents"] = contents
		delete(inner, "tools")
		delete(inner, "toolConfig")
		if pending.SystemPrompt != "" {
			inner["systemInstruction"] = map[string]interface{}{
				"parts": []map[string]interface{}{
					{"text": pending.SystemPrompt},
				},
			}
		}
	} else {
		req["contents"] = contents
		delete(req, "tools")
		delete(req, "toolConfig")
		if pending.Model != "" {
			req["model"] = pending.Model
		}
		if pending.SystemPrompt != "" {
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
	log.WithField("originalLen", len(body)).WithField("modifiedLen", len(modified)).Info("grpc interceptor: full JSON replace done")
	return modified, nil
}

// mergeToolDeclarations consolidates multiple tool entries (each containing
// functionDeclarations) into a single tool entry with all declarations merged.
// The LS sends each function as a separate {"functionDeclarations": [fn]} entry.
// Google's API rejects multiple tool entries unless they're all search tools.
func mergeToolDeclarations(root map[string]interface{}) {
	tools, ok := root["tools"].([]interface{})
	if !ok || len(tools) <= 1 {
		return
	}

	var allDecls []interface{}
	for _, tool := range tools {
		t, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		if decls, ok := t["functionDeclarations"].([]interface{}); ok {
			allDecls = append(allDecls, decls...)
		}
		if decls, ok := t["function_declarations"].([]interface{}); ok {
			allDecls = append(allDecls, decls...)
		}
	}

	if len(allDecls) > 0 {
		root["tools"] = []interface{}{
			map[string]interface{}{
				"functionDeclarations": allDecls,
			},
		}
		log.WithField("functions", len(allDecls)).WithField("originalEntries", len(tools)).Info("grpc interceptor: merged tool declarations into single entry")
	}
}

// enforceMaxOutputTokens ensures generationConfig.maxOutputTokens has a
// minimum of 4096 and defaults to 64000 if missing. This matches ZG's
// policy of ensuring adequate response length.
func enforceMaxOutputTokens(target map[string]interface{}) {
	const defaultMax = 64000
	const minMax = 4096

	gc, ok := target["generationConfig"].(map[string]interface{})
	if !ok {
		target["generationConfig"] = map[string]interface{}{
			"maxOutputTokens": float64(defaultMax),
		}
		return
	}

	current, ok := gc["maxOutputTokens"].(float64)
	if !ok || current == 0 {
		gc["maxOutputTokens"] = float64(defaultMax)
	} else if current < minMax {
		gc["maxOutputTokens"] = float64(minMax)
	}
}

// obfuscateSensitiveWords inserts zero-width spaces into sensitive client
// names to reduce detectability in server-side logs.
func obfuscateSensitiveWords(text string, words []string) string {
	for _, word := range words {
		if len(word) < 2 {
			continue
		}
		obfuscated := string(word[0]) + "\u200b" + word[1:]
		text = strings.ReplaceAll(text, word, obfuscated)
	}
	return text
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

	// Cloud Code API wraps response in {"response": {...}}
	if inner, ok := resp["response"].(map[string]interface{}); ok {
		resp = inner
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
	if frField := FindField(candidateFields, FieldCandidateFinishReason); frField != nil {
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
			// Cloud Code API wraps response in {"response": {...}}
			if inner, ok := resp["response"].(map[string]interface{}); ok {
				resp = inner
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
			if text != "" && finishReason == "" {
				finishReason = "STOP"
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

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
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
