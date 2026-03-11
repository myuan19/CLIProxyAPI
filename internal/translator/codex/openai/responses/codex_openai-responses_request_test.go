package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertSystemRoleToDeveloper_BasicConversion tests the basic system -> developer role conversion
func TestConvertSystemRoleToDeveloper_BasicConversion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are a pirate."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Say hello."}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that system role was converted to developer
	firstItemRole := gjson.Get(outputStr, "input.0.role")
	if firstItemRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstItemRole.String())
	}

	// Check that user role remains unchanged
	secondItemRole := gjson.Get(outputStr, "input.1.role")
	if secondItemRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", secondItemRole.String())
	}

	// Check content is preserved
	firstItemContent := gjson.Get(outputStr, "input.0.content.0.text")
	if firstItemContent.String() != "You are a pirate." {
		t.Errorf("Expected content 'You are a pirate.', got '%s'", firstItemContent.String())
	}
}

// TestConvertSystemRoleToDeveloper_MultipleSystemMessages tests conversion with multiple system messages
func TestConvertSystemRoleToDeveloper_MultipleSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "Be concise."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that both system roles were converted
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "developer" {
		t.Errorf("Expected second role 'developer', got '%s'", secondRole.String())
	}

	// Check that user role is unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "user" {
		t.Errorf("Expected third role 'user', got '%s'", thirdRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_NoSystemMessages tests that requests without system messages are unchanged
func TestConvertSystemRoleToDeveloper_NoSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi there!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that user and assistant roles are unchanged
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "assistant" {
		t.Errorf("Expected role 'assistant', got '%s'", secondRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_EmptyInput tests that empty input arrays are handled correctly
func TestConvertSystemRoleToDeveloper_EmptyInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": []
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that input is still an empty array
	inputArray := gjson.Get(outputStr, "input")
	if !inputArray.IsArray() {
		t.Error("Input should still be an array")
	}
	if len(inputArray.Array()) != 0 {
		t.Errorf("Expected empty array, got %d items", len(inputArray.Array()))
	}
}

// TestConvertSystemRoleToDeveloper_NoInputField tests that requests without input field are unchanged
func TestConvertSystemRoleToDeveloper_NoInputField(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that other fields are still set correctly
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true by conversion")
	}

	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be set to false by conversion")
	}
}

// TestConvertOpenAIResponsesRequestToCodex_OriginalIssue tests the exact issue reported by the user
func TestConvertOpenAIResponsesRequestToCodex_OriginalIssue(t *testing.T) {
	// This is the exact input that was failing with "System messages are not allowed"
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": "You are a pirate. Always respond in pirate speak."
			},
			{
				"type": "message",
				"role": "user",
				"content": "Say hello."
			}
		],
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify system role was converted to developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstRole.String())
	}

	// Verify stream was set to true (as required by Codex)
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true")
	}

	// Verify other required fields for Codex
	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be false")
	}

	parallelCalls := gjson.Get(outputStr, "parallel_tool_calls")
	if !parallelCalls.Bool() {
		t.Error("parallel_tool_calls should be true")
	}

	include := gjson.Get(outputStr, "include")
	if !include.IsArray() || len(include.Array()) != 1 {
		t.Error("include should be an array with one element")
	} else if include.Array()[0].String() != "reasoning.encrypted_content" {
		t.Errorf("Expected include[0] to be 'reasoning.encrypted_content', got '%s'", include.Array()[0].String())
	}
}

// TestConvertSystemRoleToDeveloper_AssistantRole tests that assistant role is preserved
func TestConvertSystemRoleToDeveloper_AssistantRole(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check system -> developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	// Check user unchanged
	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "user" {
		t.Errorf("Expected second role 'user', got '%s'", secondRole.String())
	}

	// Check assistant unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "assistant" {
		t.Errorf("Expected third role 'assistant', got '%s'", thirdRole.String())
	}
}

func TestUserFieldDeletion(t *testing.T) {  
	inputJSON := []byte(`{  
		"model": "gpt-5.2",  
		"user": "test-user",  
		"input": [{"role": "user", "content": "Hello"}]  
	}`)  
	  
	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)  
	outputStr := string(output)  
	  
	// Verify user field is deleted  
	userField := gjson.Get(outputStr, "user")  
	if userField.Exists() {
		t.Errorf("user field should be deleted, but it was found with value: %s", userField.Raw)
	}
}

// TestStripInvalidFunctionCalls_EmptyName tests removal of function_call items with empty name
// and their corresponding function_call_output items.
func TestStripInvalidFunctionCalls_EmptyName(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"thinking"}]},
			{"type":"function_call","call_id":"call_abc","name":"DocSearch","arguments":"{\"q\":\"test\"}"},
			{"type":"function_call","call_id":"fc_04f15afa","name":"","arguments":"{\"q\":\"test\"}"},
			{"type":"function_call_output","call_id":"call_abc","output":"results here"},
			{"type":"function_call_output","call_id":"fc_04f15afa","output":"未知工具: "}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5", inputJSON, false)
	outputStr := string(output)

	inputArr := gjson.Get(outputStr, "input")
	if !inputArr.IsArray() {
		t.Fatal("input should be an array")
	}
	items := inputArr.Array()

	// Should have 4 items: user msg, assistant msg, valid function_call, valid function_call_output
	if len(items) != 4 {
		t.Fatalf("Expected 4 items after cleanup, got %d: %s", len(items), inputArr.Raw)
	}

	// The function_call with empty name and its output should be gone
	for i, item := range items {
		if item.Get("call_id").String() == "fc_04f15afa" {
			t.Errorf("item %d still references removed call_id fc_04f15afa: %s", i, item.Raw)
		}
	}

	// The valid function_call should remain
	if items[2].Get("type").String() != "function_call" || items[2].Get("name").String() != "DocSearch" {
		t.Errorf("Expected valid function_call at index 2, got: %s", items[2].Raw)
	}
	if items[3].Get("type").String() != "function_call_output" || items[3].Get("call_id").String() != "call_abc" {
		t.Errorf("Expected valid function_call_output at index 3, got: %s", items[3].Raw)
	}
}

// TestStripInvalidFunctionCalls_DuplicateCallID tests deduplication of function_call items
// sharing the same call_id (keeps first occurrence).
func TestStripInvalidFunctionCalls_DuplicateCallID(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"function_call","id":"fc_aaa","call_id":"call_123","name":"Search","arguments":"{}"},
			{"type":"function_call","call_id":"call_123","name":"Search","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_123","output":"ok"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5", inputJSON, false)
	outputStr := string(output)

	items := gjson.Get(outputStr, "input").Array()
	if len(items) != 3 {
		t.Fatalf("Expected 3 items after dedup, got %d: %s", len(items), gjson.Get(outputStr, "input").Raw)
	}

	// Only one function_call should remain (the first one, with id field)
	fc := items[1]
	if fc.Get("type").String() != "function_call" {
		t.Errorf("Expected function_call at index 1, got: %s", fc.Raw)
	}
	if fc.Get("id").String() != "fc_aaa" {
		t.Errorf("Expected first occurrence (with id fc_aaa) to be kept, got: %s", fc.Raw)
	}
}

// TestStripInvalidFunctionCalls_NoChanges tests that clean input passes through unchanged.
func TestStripInvalidFunctionCalls_NoChanges(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"function_call","call_id":"call_1","name":"Func1","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"done"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5", inputJSON, false)
	items := gjson.GetBytes(output, "input").Array()

	if len(items) != 3 {
		t.Fatalf("Expected 3 items (no removal), got %d", len(items))
	}
}

// TestStripInvalidFunctionCalls_RealWorldDuplicate reproduces the exact pattern reported:
// interleaved call_ and fc_ items with empty names.
func TestStripInvalidFunctionCalls_RealWorldDuplicate(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"stream": true,
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"灵狐是啥"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"让我搜索一下"}]},
			{"type":"function_call","call_id":"call_IvYn","name":"DocSearch","arguments":"{\"query\":\"灵狐\"}"},
			{"type":"function_call","call_id":"fc_04f15afa","name":"","arguments":"{\"query\":\"灵狐\"}"},
			{"type":"function_call","call_id":"call_2C0r","name":"DocGrep","arguments":"{\"pattern\":\"灵狐\"}"},
			{"type":"function_call","call_id":"fc_04f15afb","name":"","arguments":"{\"pattern\":\"灵狐\"}"},
			{"type":"function_call","call_id":"call_VMUw","name":"DocGlob","arguments":"{\"pattern\":\"*灵狐*\"}"},
			{"type":"function_call","call_id":"fc_04f15afc","name":"","arguments":"{\"pattern\":\"*灵狐*\"}"},
			{"type":"function_call_output","call_id":"call_IvYn","output":"搜索结果..."},
			{"type":"function_call_output","call_id":"fc_04f15afa","output":"未知工具: "},
			{"type":"function_call_output","call_id":"call_2C0r","output":"匹配结果..."},
			{"type":"function_call_output","call_id":"fc_04f15afb","output":"未知工具: "},
			{"type":"function_call_output","call_id":"call_VMUw","output":"文件列表..."},
			{"type":"function_call_output","call_id":"fc_04f15afc","output":"未知工具: "}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	items := gjson.Get(outputStr, "input").Array()

	// Should keep: 2 messages + 3 valid function_calls + 3 valid function_call_outputs = 8
	if len(items) != 8 {
		t.Fatalf("Expected 8 items after cleanup, got %d: %s", len(items), gjson.Get(outputStr, "input").Raw)
	}

	// Verify no fc_ call_ids remain
	for i, item := range items {
		cid := item.Get("call_id").String()
		if len(cid) > 0 && cid[:3] == "fc_" {
			t.Errorf("item %d still has fc_ call_id %q", i, cid)
		}
	}

	// Verify all three valid function_calls remain with correct names
	names := map[string]bool{}
	for _, item := range items {
		if item.Get("type").String() == "function_call" {
			names[item.Get("name").String()] = true
		}
	}
	for _, expected := range []string{"DocSearch", "DocGrep", "DocGlob"} {
		if !names[expected] {
			t.Errorf("Expected function_call with name %q to survive cleanup", expected)
		}
	}
}
