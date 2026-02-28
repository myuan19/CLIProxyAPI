package compat

// scanTopKeys scans the JSON body once and returns a bitmask of which
// distinguishing fields are present at the top level.
func scanTopKeys(body []byte) uint16 {
	var present uint16
	n := len(body)
	i := skipWS(body, 0)
	if i >= n || body[i] != '{' {
		return 0
	}
	i++

	for {
		i = skipWS(body, i)
		if i >= n || body[i] == '}' {
			break
		}
		if body[i] == ',' {
			i++
			i = skipWS(body, i)
		}
		if i >= n || body[i] != '"' {
			break
		}

		keyStart := i + 1
		keyEnd := scanStringEnd(body, keyStart)
		if keyEnd >= n {
			break
		}
		bit := matchKey(body[keyStart:keyEnd])
		if bit != 0 {
			present |= bit
		}
		i = keyEnd + 1

		i = skipWS(body, i)
		if i >= n || body[i] != ':' {
			break
		}
		i++
		i = skipWS(body, i)

		if bit == bRequest && i < n && body[i] == '{' {
			if scanNestedHasKey(body, i, "contents") {
				present |= bRequestContents
			}
		}

		i = skipValue(body, i)
	}

	return present
}

// matchKey maps a key byte slice to its field bit. Returns 0 for untracked keys.
// Go optimises string(key) == "literal" to avoid allocation.
func matchKey(key []byte) uint16 {
	switch len(key) {
	case 5:
		if string(key) == "input" {
			return bInput
		}
	case 6:
		if string(key) == "system" {
			return bSystem
		}
	case 7:
		if string(key) == "request" {
			return bRequest
		}
	case 8:
		switch key[0] {
		case 'c':
			if string(key) == "contents" {
				return bContents
			}
		case 'm':
			if string(key) == "messages" {
				return bMessages
			}
		case 't':
			if string(key) == "thinking" {
				return bThinking
			}
		}
	case 9:
		if string(key) == "userAgent" {
			return bUserAgent
		}
	case 11:
		if string(key) == "requestType" {
			return bRequestType
		}
	case 12:
		if string(key) == "instructions" {
			return bInstructions
		}
	case 14:
		if string(key) == "stop_sequences" {
			return bStopSequences
		}
	}
	return 0
}

// scanNestedHasKey checks whether the object starting at body[start] (must be '{')
// contains a first-level key equal to target.
func scanNestedHasKey(body []byte, start int, target string) bool {
	n := len(body)
	i := start + 1
	for {
		i = skipWS(body, i)
		if i >= n || body[i] == '}' {
			return false
		}
		if body[i] == ',' {
			i++
			i = skipWS(body, i)
		}
		if i >= n || body[i] != '"' {
			return false
		}
		keyStart := i + 1
		keyEnd := scanStringEnd(body, keyStart)
		if keyEnd >= n {
			return false
		}
		if string(body[keyStart:keyEnd]) == target {
			return true
		}
		i = keyEnd + 1
		i = skipWS(body, i)
		if i >= n || body[i] != ':' {
			return false
		}
		i++
		i = skipWS(body, i)
		i = skipValue(body, i)
	}
}

func skipWS(body []byte, i int) int {
	for i < len(body) && body[i] <= ' ' {
		i++
	}
	return i
}

func scanStringEnd(body []byte, start int) int {
	for i := start; i < len(body); i++ {
		if body[i] == '\\' {
			i++
			continue
		}
		if body[i] == '"' {
			return i
		}
	}
	return len(body)
}

func skipValue(body []byte, start int) int {
	n := len(body)
	if start >= n {
		return n
	}
	switch body[start] {
	case '"':
		return scanStringEnd(body, start+1) + 1
	case '{', '[':
		return skipBracketed(body, start)
	case 't', 'n':
		if start+4 > n {
			return n
		}
		return start + 4
	case 'f':
		if start+5 > n {
			return n
		}
		return start + 5
	default:
		i := start
		for i < n && body[i] != ',' && body[i] != '}' && body[i] != ']' && body[i] > ' ' {
			i++
		}
		return i
	}
}

func skipBracketed(body []byte, start int) int {
	n := len(body)
	depth := 1
	inStr := false
	for i := start + 1; i < n; i++ {
		if inStr {
			if body[i] == '\\' {
				i++
				continue
			}
			if body[i] == '"' {
				inStr = false
			}
			continue
		}
		switch body[i] {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return n
}
