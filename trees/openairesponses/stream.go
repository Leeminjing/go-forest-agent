package openairesponses

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
)

type completion struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
}

func readStream(ctx context.Context, body io.Reader, emit func(string) error) (completion, error) {
	reader := bufio.NewReader(io.LimitReader(body, MaxRecordBytes+1))
	total := 0
	data := ""
	identities := map[int]string{}
	arguments := map[int]string{}
	for {
		if err := ctx.Err(); err != nil {
			return completion{}, err
		}
		line, readErr := reader.ReadString('\n')
		total += len(line)
		if total > MaxRecordBytes || len(data)+len(line) > 2<<20 {
			return completion{}, ErrProtocol
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data != "" {
				data += "\n"
			}
			data += value
		}
		if line == "" && data != "" {
			if checkJSON([]byte(data)) != nil {
				return completion{}, ErrProtocol
			}
			var event struct {
				Type     string          `json:"type"`
				Delta    string          `json:"delta"`
				ItemID   string          `json:"item_id"`
				Index    int             `json:"output_index"`
				Item     json.RawMessage `json:"item"`
				Response completion      `json:"response"`
			}
			if json.Unmarshal([]byte(data), &event) != nil {
				return completion{}, ErrProtocol
			}
			data = ""
			switch event.Type {
			case "response.output_text.delta":
				if emit != nil {
					if err := emit(event.Delta); err != nil {
						return completion{}, err
					}
				}
			case "response.output_item.added", "response.output_item.done":
				var item outputItem
				if json.Unmarshal(event.Item, &item) != nil || item.ID == "" {
					return completion{}, ErrProtocol
				}
				if old := identities[event.Index]; old != "" && old != item.ID {
					return completion{}, ErrProtocol
				}
				identities[event.Index] = item.ID
			case "response.function_call_arguments.delta":
				if old := identities[event.Index]; old != "" && old != event.ItemID {
					return completion{}, ErrProtocol
				}
				if event.ItemID == "" {
					return completion{}, ErrProtocol
				}
				identities[event.Index] = event.ItemID
				arguments[event.Index] += event.Delta
			case "response.completed":
				if event.Response.ID == "" || event.Response.Status != "completed" || event.Response.Output == nil {
					return completion{}, ErrProtocol
				}
				for index, id := range identities {
					if index < 0 || index >= len(event.Response.Output) {
						return completion{}, ErrProtocol
					}
					var item outputItem
					if json.Unmarshal(event.Response.Output[index], &item) != nil || item.ID != id {
						return completion{}, ErrProtocol
					}
					if text, ok := arguments[index]; ok && text != item.Arguments {
						return completion{}, ErrProtocol
					}
				}
				return event.Response, nil
			case "response.failed", "response.incomplete", "error":
				return completion{}, ErrProtocol
			}
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return completion{}, ctx.Err()
			}
			return completion{}, ErrProtocol
		}
	}
}
