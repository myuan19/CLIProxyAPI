package responses

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	inputResult := gjson.GetBytes(rawJSON, "input")
	if inputResult.Type == gjson.String {
		input, _ := sjson.Set(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`, "0.content.0.text", inputResult.String())
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "input", []byte(input))
	}

	rawJSON, _ = sjson.SetBytes(rawJSON, "stream", true)
	rawJSON, _ = sjson.SetBytes(rawJSON, "store", false)
	rawJSON, _ = sjson.SetBytes(rawJSON, "parallel_tool_calls", true)
	rawJSON, _ = sjson.SetBytes(rawJSON, "include", []string{"reasoning.encrypted_content"})
	// Codex Responses rejects token limit fields, so strip them out before forwarding.
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "max_output_tokens")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "max_completion_tokens")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "temperature")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "top_p")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "service_tier")

	// Delete the user field as it is not supported by the Codex upstream.
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "user")

	// Strip function_call items with empty name (often caused by clients confusing the
	// Responses API "id" field with "call_id"), their orphaned function_call_output items,
	// and any duplicate function_call entries sharing the same call_id.
	rawJSON = stripInvalidFunctionCalls(rawJSON)

	// Convert role "system" to "developer" in input array to comply with Codex API requirements.
	rawJSON = convertSystemRoleToDeveloper(rawJSON)

	return rawJSON
}

// stripInvalidFunctionCalls removes malformed function_call items from the input array.
//
// Some clients incorrectly treat the Responses API item "id" field (e.g. "fc_04f15afa…")
// as a separate "call_id", producing duplicate function_call entries with empty "name".
// The upstream rejects these with "Invalid 'input[N].name': empty string".
//
// This function:
//  1. Removes function_call items whose name is empty.
//  2. Removes the corresponding function_call_output items (matched by call_id).
//  3. Deduplicates function_call items that share the same call_id (keeps first).
func stripInvalidFunctionCalls(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}

	items := inputResult.Array()
	if len(items) == 0 {
		return rawJSON
	}

	// Pass 1: collect call_ids of function_call items with empty name.
	emptyNameCallIDs := map[string]struct{}{}
	for _, item := range items {
		if item.Get("type").String() == "function_call" && item.Get("name").String() == "" {
			if cid := item.Get("call_id").String(); cid != "" {
				emptyNameCallIDs[cid] = struct{}{}
			}
		}
	}

	// Pass 2: detect duplicate function_call items sharing the same call_id.
	seenCallIDs := map[string]struct{}{}
	dupIndices := map[int]struct{}{}
	for i, item := range items {
		if item.Get("type").String() != "function_call" {
			continue
		}
		cid := item.Get("call_id").String()
		if cid == "" {
			continue
		}
		if _, seen := seenCallIDs[cid]; seen {
			dupIndices[i] = struct{}{}
		} else {
			seenCallIDs[cid] = struct{}{}
		}
	}

	if len(emptyNameCallIDs) == 0 && len(dupIndices) == 0 {
		return rawJSON
	}

	// Pass 3: collect indices to remove (empty-name items + their outputs + duplicates).
	var removeIdx []int
	for i, item := range items {
		cid := item.Get("call_id").String()
		if _, bad := emptyNameCallIDs[cid]; bad {
			removeIdx = append(removeIdx, i)
			continue
		}
		if _, dup := dupIndices[i]; dup {
			removeIdx = append(removeIdx, i)
		}
	}

	// Pass 4: delete in reverse index order so earlier indices stay valid.
	result := rawJSON
	for i := len(removeIdx) - 1; i >= 0; i-- {
		result, _ = sjson.DeleteBytes(result, fmt.Sprintf("input.%d", removeIdx[i]))
	}
	return result
}

// convertSystemRoleToDeveloper traverses the input array and converts any message items
// with role "system" to role "developer". This is necessary because Codex API does not
// accept "system" role in the input array.
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}

	inputArray := inputResult.Array()
	result := rawJSON

	// Directly modify role values for items with "system" role
	for i := 0; i < len(inputArray); i++ {
		rolePath := fmt.Sprintf("input.%d.role", i)
		if gjson.GetBytes(result, rolePath).String() == "system" {
			result, _ = sjson.SetBytes(result, rolePath, "developer")
		}
	}

	return result
}
