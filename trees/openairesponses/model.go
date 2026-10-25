package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest/forest"
)

type Config struct {
	Endpoint        string        `json:"endpoint"`
	APIKey          string        `json:"-"`
	Model           string        `json:"model"`
	Instructions    string        `json:"instructions"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens"`
	Timeout         time.Duration `json:"timeout"`
	Client          *http.Client  `json:"-"`
}

func normalize(config Config) (Config, error) {
	if config.Endpoint == "" {
		config.Endpoint = "https://api.openai.com/v1/responses"
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (!strings.HasSuffix(u.Path, "/responses")) {
		return config, ErrProtocol
	}
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1")) {
		return config, ErrProtocol
	}
	if config.Model == "" || config.APIKey == "" || !utf8.ValidString(config.Model) || !utf8.ValidString(config.Instructions) || strings.ContainsAny(config.APIKey, "\r\n") {
		return config, ErrProtocol
	}
	if config.MaxOutputTokens == 0 {
		config.MaxOutputTokens = 4096
	}
	if config.MaxOutputTokens < 16 || config.MaxOutputTokens > 32768 {
		return config, ErrProtocol
	}
	if config.Timeout == 0 {
		config.Timeout = 90 * time.Second
	}
	if config.Timeout < time.Millisecond || config.Timeout > 10*time.Minute {
		return config, ErrProtocol
	}
	switch config.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return config, ErrProtocol
	}
	return config, nil
}
func Profile(config Config) (string, error) {
	// Credentials are deliberately omitted from the identity and all records.
	if config.APIKey == "" {
		config.APIKey = "profile-only"
	}
	config, err := normalize(config)
	if err != nil {
		return "", err
	}
	return Fingerprint(config)
}

type Model struct {
	config  Config
	profile string
	client  *http.Client
	use     func(context.Context, func(Journal) error) error
}

func New(config Config, journal Journal) (*Model, error) {
	if journal == nil {
		return nil, ErrContinuation
	}
	return newModel(config, func(ctx context.Context, fn func(Journal) error) error { return fn(journal) })
}
func newModel(config Config, use func(context.Context, func(Journal) error) error) (*Model, error) {
	config, err := normalize(config)
	if err != nil {
		return nil, err
	}
	profile, err := Profile(config)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: config.Timeout}
	if config.Client != nil {
		client = *config.Client
		client.Timeout = config.Timeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Model{config: config, profile: profile, client: &client, use: use}, nil
}

// HTTPError contains only safe metadata, never the provider body or credential.
type HTTPError struct {
	Status     int
	RequestID  string
	RetryAfter string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Responses HTTP status %d (request %s)", e.Status, e.RequestID)
}
func (m *Model) Generate(ctx context.Context, request agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
	if request.RunID == "" || len(request.RunID) > 256 || len(request.Messages) > 512 || len(request.Tools) > 64 {
		return agent.ModelResult{}, ErrProtocol
	}
	for _, message := range request.Messages {
		if !utf8.ValidString(message.Content) {
			return agent.ModelResult{}, ErrProtocol
		}
	}
	if err := ctx.Err(); err != nil {
		return agent.ModelResult{}, err
	}
	requestHash, err := Fingerprint(request)
	if err != nil {
		return agent.ModelResult{}, err
	}
	var records []Record
	if err = m.use(ctx, func(j Journal) error { var err error; records, err = j.Load(ctx, request.RunID); return err }); err != nil {
		return agent.ModelResult{}, err
	}
	if err = CheckChain(request.RunID, records); err != nil {
		return agent.ModelResult{}, err
	}
	for _, record := range records {
		if record.Profile != m.profile {
			return agent.ModelResult{}, ErrContinuation
		}
		if record.RequestHash == requestHash {
			return record.Result, nil
		}
	}
	var input []json.RawMessage
	parent := ""
	if len(records) == 0 {
		input, err = initialInput(request.Messages)
	} else {
		last := records[len(records)-1]
		parent = last.Digest
		input, err = nextInput(last, request)
	}
	if err != nil {
		return agent.ModelResult{}, err
	}
	tools := []map[string]any{}
	names := map[string]bool{}
	for _, tool := range request.Tools {
		if tool.Name == "" || len(tool.Name) > 64 || names[tool.Name] || !validObject(tool.Parameters) {
			return agent.ModelResult{}, ErrProtocol
		}
		names[tool.Name] = true
		var schema map[string]json.RawMessage
		_ = json.Unmarshal(tool.Parameters, &schema)
		if string(schema["type"]) != `"object"` || string(schema["additionalProperties"]) != "false" {
			return agent.ModelResult{}, ErrProtocol
		}
		tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.Parameters, "strict": true})
	}
	body := map[string]any{"model": m.config.Model, "input": input, "tools": tools, "stream": true, "store": false, "truncation": "disabled", "parallel_tool_calls": false, "include": []string{"reasoning.encrypted_content"}, "max_output_tokens": m.config.MaxOutputTokens}
	if m.config.Instructions != "" {
		body["instructions"] = m.config.Instructions
	}
	if m.config.ReasoningEffort != "" {
		body["reasoning"] = map[string]string{"effort": m.config.ReasoningEffort}
	}
	data, err := json.Marshal(body)
	if err != nil || len(data) > MaxRecordBytes {
		return agent.ModelResult{}, ErrProtocol
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.Endpoint, bytes.NewReader(data))
	if err != nil {
		return agent.ModelResult{}, ErrProtocol
	}
	httpRequest.Header.Set("Authorization", "Bearer "+m.config.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := m.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return agent.ModelResult{}, ctx.Err()
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return agent.ModelResult{}, fmt.Errorf("Responses timeout: %w", context.DeadlineExceeded)
		}
		return agent.ModelResult{}, errors.New("Responses transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return agent.ModelResult{}, &HTTPError{response.StatusCode, safeHeader(response.Header.Get("x-request-id")), safeHeader(response.Header.Get("Retry-After"))}
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return agent.ModelResult{}, ErrProtocol
	}
	completed, err := readStream(ctx, response.Body, emit)
	if err != nil {
		return agent.ModelResult{}, err
	}
	result, err := standardResult(completed.Output, request.Tools)
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, message := range request.Messages {
		for _, call := range message.Calls {
			seen[call.ID] = true
		}
	}
	for _, call := range result.Calls {
		if seen[call.ID] {
			return agent.ModelResult{}, ErrProtocol
		}
	}
	record := Record{Version: "openai-responses-journal-v1", RunID: request.RunID, Sequence: uint64(len(records) + 1), Parent: parent, Profile: m.profile, RequestHash: requestHash, Request: request, Result: result, Input: input, Output: completed.Output, ResponseID: completed.ID, RequestID: safeHeader(response.Header.Get("x-request-id"))}
	record.Digest, err = Fingerprint(record)
	if err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return agent.ModelResult{}, err
	}
	err = m.use(ctx, func(j Journal) error { return j.Append(ctx, request.RunID, uint64(len(records)), record) })
	if err != nil {
		return agent.ModelResult{}, err
	}
	return result, nil
}
func safeHeader(value string) string {
	if len(value) > 256 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}
func Definition(config Config) forest.Definition {
	return forest.Definition{TypeID: "agent.model.openai-responses", Version: "1", Ports: []forest.PortDescriptor{forest.Port[Journal]("journal", forest.Required(), forest.During(forest.Construction))}, Capabilities: []forest.CapabilityDescriptor{forest.Capability[agent.Model]("model", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		ref, err := forest.Reference[Journal](env, "journal")
		if err != nil {
			return nil, err
		}
		m, err := newModel(config, func(ctx context.Context, fn func(Journal) error) error { return ref.Use(ctx, fn) })
		if err != nil {
			return nil, err
		}
		return forest.Hooks{OnStop: func(context.Context) error { m.client.CloseIdleConnections(); return nil }}, forest.Provide(env, "model", m)
	}}
}
