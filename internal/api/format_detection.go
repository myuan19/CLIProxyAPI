package api

import (
	"fmt"
	"net/http"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// forbiddenField represents a field that should not be present in a given format.
// When detected, it links to one or more alternative formats that do use this field.
type forbiddenField struct {
	path    string
	linksTo []sdktranslator.Format
}

// formatRule defines the validation rules for a specific API format.
type formatRule struct {
	mustHave    []string
	mustNotHave []forbiddenField
}

var formatRules = map[sdktranslator.Format]formatRule{
	sdktranslator.FormatAntigravity: {
		mustHave: []string{"request"},
		mustNotHave: []forbiddenField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
	},
	sdktranslator.FormatGeminiCLI: {
		mustHave: []string{"request", "request.contents"},
		mustNotHave: []forbiddenField{
			{"userAgent", []sdktranslator.Format{sdktranslator.FormatAntigravity}},
			{"requestType", []sdktranslator.Format{sdktranslator.FormatAntigravity}},
			{"requestId", []sdktranslator.Format{sdktranslator.FormatAntigravity}},
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
	},
	sdktranslator.FormatGemini: {
		mustHave: []string{"contents"},
		mustNotHave: []forbiddenField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
	},
	sdktranslator.FormatClaude: {
		mustHave: []string{"messages"},
		mustNotHave: []forbiddenField{
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
			{"instructions", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
	},
	sdktranslator.FormatOpenAI: {
		mustHave: []string{"messages"},
		mustNotHave: []forbiddenField{
			{"system", []sdktranslator.Format{sdktranslator.FormatClaude}},
			{"stop_sequences", []sdktranslator.Format{sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
			{"instructions", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
	},
	sdktranslator.FormatOpenAIResponse: {
		mustHave: []string{"input"},
		mustNotHave: []forbiddenField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
	},
	sdktranslator.FormatCodex: {
		mustHave: []string{"input"},
		mustNotHave: []forbiddenField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
	},
}

// formatPriority defines the commonality ranking used to break ties when multiple
// candidate formats remain after intersection.
var formatPriority = []sdktranslator.Format{
	sdktranslator.FormatOpenAI,
	sdktranslator.FormatClaude,
	sdktranslator.FormatOpenAIResponse,
	sdktranslator.FormatGemini,
	sdktranslator.FormatCodex,
	sdktranslator.FormatGeminiCLI,
	sdktranslator.FormatAntigravity,
}

type formatValidationResult struct {
	valid           bool
	correctedFormat sdktranslator.Format
	wasCorrected    bool
	httpStatus      int
	errorMessage    string
}

const (
	ginKeyFormatInfo = "FORMAT_DETECTION_INFO"
)

// validateAndCorrectFormat checks whether rawBody conforms to the given endpointFormat.
// If the body matches, it returns valid=true with the original format.
// If must_not_have fields are violated, it computes the intersection of linked formats
// and returns the best-matching corrected format.
// If must_have fields are missing and no alternative can be inferred, it returns an error.
func validateAndCorrectFormat(rawBody []byte, endpointFormat sdktranslator.Format) formatValidationResult {
	rule, ok := formatRules[endpointFormat]
	if !ok {
		return formatValidationResult{valid: true, correctedFormat: endpointFormat}
	}

	mustHaveOK := true
	for _, field := range rule.mustHave {
		if !gjson.GetBytes(rawBody, field).Exists() {
			mustHaveOK = false
			break
		}
	}

	// Collect all violated must_not_have fields and their linked formats.
	var violatedLinks [][]sdktranslator.Format
	for _, f := range rule.mustNotHave {
		if gjson.GetBytes(rawBody, f.path).Exists() {
			violatedLinks = append(violatedLinks, f.linksTo)
		}
	}

	mustNotHaveOK := len(violatedLinks) == 0

	// Case 1: everything passes — format matches.
	if mustHaveOK && mustNotHaveOK {
		return formatValidationResult{valid: true, correctedFormat: endpointFormat}
	}

	// Case 2: must_not_have violated — try to infer the correct format via intersection.
	if !mustNotHaveOK {
		candidates := intersectFormatSets(violatedLinks)
		if len(candidates) == 1 {
			corrected := candidates[0]
			log.Debugf("[FormatDetection] Auto-corrected format: %s → %s", endpointFormat, corrected)
			return formatValidationResult{valid: true, correctedFormat: corrected, wasCorrected: true}
		}
		if len(candidates) > 1 {
			corrected := pickByPriority(candidates)
			log.Debugf("[FormatDetection] Auto-corrected format: %s → %s (from %d candidates)", endpointFormat, corrected, len(candidates))
			return formatValidationResult{valid: true, correctedFormat: corrected, wasCorrected: true}
		}
		// Intersection is empty — ambiguous request.
		return formatValidationResult{
			valid:      false,
			httpStatus: http.StatusBadRequest,
			errorMessage: fmt.Sprintf(
				"Ambiguous request format: the request body contains fields incompatible with %s format, and no single alternative format could be determined.",
				endpointFormat,
			),
		}
	}

	// Case 3: must_have missing but must_not_have all passed — genuinely malformed request.
	missingFields := []string{}
	for _, field := range rule.mustHave {
		if !gjson.GetBytes(rawBody, field).Exists() {
			missingFields = append(missingFields, field)
		}
	}
	return formatValidationResult{
		valid:      false,
		httpStatus: http.StatusBadRequest,
		errorMessage: fmt.Sprintf(
			"Invalid request for %s format: missing required field(s): %v",
			endpointFormat, missingFields,
		),
	}
}

// intersectFormatSets computes the intersection of multiple format slices.
func intersectFormatSets(sets [][]sdktranslator.Format) []sdktranslator.Format {
	if len(sets) == 0 {
		return nil
	}

	counts := make(map[sdktranslator.Format]int)
	for _, set := range sets {
		seen := make(map[sdktranslator.Format]bool)
		for _, f := range set {
			if !seen[f] {
				counts[f]++
				seen[f] = true
			}
		}
	}

	var result []sdktranslator.Format
	for f, c := range counts {
		if c == len(sets) {
			result = append(result, f)
		}
	}
	return result
}

// pickByPriority selects the highest-priority format from a set of candidates.
func pickByPriority(candidates []sdktranslator.Format) sdktranslator.Format {
	candidateSet := make(map[sdktranslator.Format]bool)
	for _, c := range candidates {
		candidateSet[c] = true
	}
	for _, f := range formatPriority {
		if candidateSet[f] {
			return f
		}
	}
	return candidates[0]
}
