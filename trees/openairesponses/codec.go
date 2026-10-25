package openairesponses

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Leeminjing/go-forest-agent/agent"
)

// Native JSON is deliberately separate from the product's integer-only codec.
// Number tokens and original item bytes retain provider precision.
func checkJSON(data []byte) error {
	if !utf8.Valid(data) || !json.Valid(data) || !validEscapes(data) || len(data) > MaxRecordBytes {
		return ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return ErrProtocol
		}
		token, err := d.Token()
		if err != nil {
			return ErrProtocol
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				keys := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return ErrProtocol
					}
					name, ok := key.(string)
					if !ok || keys[name] {
						return ErrProtocol
					}
					keys[name] = true
					if err := value(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := value(depth + 1); err != nil {
						return err
					}
				}
			default:
				return ErrProtocol
			}
			if _, err := d.Token(); err != nil {
				return ErrProtocol
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrProtocol
	}
	return nil
}

func validEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
func validObject(data []byte) bool {
	if checkJSON(data) != nil {
		return false
	}
	data = bytes.TrimSpace(data)
	return len(data) > 0 && data[0] == '{'
}

type outputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Status    string `json:"status"`
	Role      string `json:"role"`
	Phase     string `json:"phase"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Encrypted string `json:"encrypted_content"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func standardResult(output []json.RawMessage, tools []agent.ToolSpec) (agent.ModelResult, error) {
	var result agent.ModelResult
	seen := map[string]bool{}
	names := map[string]bool{}
	final := false
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, raw := range output {
		if err := checkJSON(raw); err != nil {
			return result, err
		}
		var item outputItem
		if json.Unmarshal(raw, &item) != nil || item.ID == "" {
			return result, ErrProtocol
		}
		switch item.Type {
		case "reasoning":
			if item.Encrypted == "" {
				if len(item.Content) == 0 {
					return result, ErrContinuation
				}
				for _, part := range item.Content {
					if part.Type != "reasoning_text" {
						return result, ErrProtocol
					}
				}
			}
		case "message":
			if item.Status != "completed" || item.Role != "assistant" || (item.Phase != "" && item.Phase != "commentary" && item.Phase != "final_answer") {
				return result, ErrProtocol
			}
			for _, part := range item.Content {
				if part.Type == "refusal" {
					return result, ErrRefused
				}
				if part.Type != "output_text" {
					return result, ErrProtocol
				}
				result.Content += part.Text
			}
			if item.Phase != "commentary" && len(item.Content) > 0 {
				final = true
			}
		case "function_call":
			if item.Status != "completed" || item.CallID == "" || len(item.CallID) > 256 || seen[item.CallID] || !names[item.Name] || !validObject([]byte(item.Arguments)) {
				return result, ErrProtocol
			}
			seen[item.CallID] = true
			var compact bytes.Buffer
			if json.Compact(&compact, []byte(item.Arguments)) != nil {
				return result, ErrProtocol
			}
			result.Calls = append(result.Calls, agent.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(compact.Bytes())})
		default:
			return result, ErrProtocol
		}
	}
	if len(result.Content) > 1<<20 || len(result.Calls) > 16 || (len(result.Calls) == 0 && (!final || strings.TrimSpace(result.Content) == "")) {
		return result, ErrProtocol
	}
	return result, nil
}
