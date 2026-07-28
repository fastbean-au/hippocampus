package summarise

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultTimeout bounds one summarisation call. Small local models on CPU can take many
	// seconds per generation, so this is generous; the caller's context deadline still wins when
	// it is shorter.
	defaultTimeout = 120 * time.Second

	// defaultMaxBodies caps how many memory bodies are sent in one prompt, keeping the prompt
	// inside a small model's context window. Bodies beyond this are dropped (a summary of the
	// most recent/first N is still useful).
	defaultMaxBodies = 200

	// defaultPromptCharLimit caps the total characters of memory bodies placed in the prompt, a
	// second bound (independent of body count) against overrunning the model's context.
	defaultPromptCharLimit = 32000

	// defaultSystemPrompt instructs the model to behave as a memory-consolidation assistant. It is
	// overridable via Config.SystemPrompt.
	defaultSystemPrompt = "You are a memory-consolidation assistant. You are given a set of related " +
		"memories that belong to a single event. Write one concise summary that preserves the most " +
		"significant facts across them, in plain prose. Respond with only the summary text, no " +
		"preamble, headings, or commentary."
)

// Config configures the Ollama-backed summariser. Address and Model are required; the rest have
// defaults applied by NewOllama when zero.
type Config struct {
	// Address is the base URL of the Ollama server, e.g. "http://localhost:11434".
	Address string

	// Model is the Ollama model tag used for generation, e.g. "llama3.2".
	Model string

	// Timeout bounds one summarisation HTTP call (0 -> defaultTimeout).
	Timeout time.Duration

	// MaxBodies caps how many memory bodies go into one prompt (0 -> defaultMaxBodies).
	MaxBodies int

	// PromptCharLimit caps the total characters of memory bodies in one prompt (0 ->
	// defaultPromptCharLimit).
	PromptCharLimit int

	// SystemPrompt overrides the built-in instruction (empty -> defaultSystemPrompt).
	SystemPrompt string

	// Temperature is passed to the model's sampling options; a low value keeps summaries
	// deterministic and faithful. 0 lets Ollama use the model default.
	Temperature float64
}

// Ollama is a Summariser backed by a running Ollama server, reached over its /api/generate HTTP
// endpoint. It holds no state beyond the resolved config and an http.Client; a single instance is
// safe for concurrent use.
type Ollama struct {
	address         string
	model           string
	systemPrompt    string
	maxBodies       int
	promptCharLimit int
	temperature     float64
	client          *http.Client
}

// generateRequest is the body of an Ollama POST /api/generate call. stream is always false so the
// whole response arrives as a single JSON object rather than a token stream.
type generateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	System  string         `json:"system,omitempty"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

// generateResponse is the (non-streamed) response from /api/generate. Error carries an Ollama-side
// error message (e.g. an unknown model) even on some 200 responses.
type generateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Error    string `json:"error"`
}

// NewOllama builds an Ollama summariser from cfg, applying defaults for the optional fields. It
// fails only on unusable configuration (a missing address/model or an unparseable address); an
// unreachable server must not prevent startup, since summarisation is optional and best-effort, so
// no connectivity check is made here.
func NewOllama(cfg Config) (*Ollama, error) {
	address := strings.TrimRight(strings.TrimSpace(cfg.Address), "/")

	if address == "" {
		return nil, fmt.Errorf("ollama.address must be set when ollama.enabled is true")
	}

	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("ollama.model must be set when ollama.enabled is true")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	maxBodies := cfg.MaxBodies
	if maxBodies <= 0 {
		maxBodies = defaultMaxBodies
	}

	promptCharLimit := cfg.PromptCharLimit
	if promptCharLimit <= 0 {
		promptCharLimit = defaultPromptCharLimit
	}

	systemPrompt := cfg.SystemPrompt
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = defaultSystemPrompt
	}

	o := &Ollama{
		address:         address,
		model:           strings.TrimSpace(cfg.Model),
		systemPrompt:    systemPrompt,
		maxBodies:       maxBodies,
		promptCharLimit: promptCharLimit,
		temperature:     cfg.Temperature,
		client:          &http.Client{Timeout: timeout},
	}

	return o, nil
}

// Summarise sends the request's bodies to the Ollama server and returns the generated summary. It
// bounds the prompt by MaxBodies and PromptCharLimit, and returns an error when there is nothing to
// summarise, when the server is unreachable/returns a non-2xx status, or when the model reports an
// error or an empty response.
func (o *Ollama) Summarise(ctx context.Context, req Request) (string, error) {
	if len(req.Bodies) == 0 {
		return "", fmt.Errorf("no memory bodies to summarise")
	}

	prompt := o.buildPrompt(req)

	body := generateRequest{
		Model:  o.model,
		Prompt: prompt,
		System: o.systemPrompt,
		Stream: false,
	}

	if o.temperature > 0 {
		body.Options = map[string]any{"temperature": o.temperature}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("failed to marshal ollama request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.address+"/api/generate", bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("failed to build ollama request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read ollama response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out generateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("failed to decode ollama response: %w", err)
	}

	if out.Error != "" {
		return "", fmt.Errorf("ollama error: %s", out.Error)
	}

	summary := strings.TrimSpace(out.Response)
	if summary == "" {
		return "", fmt.Errorf("ollama returned an empty summary")
	}

	return summary, nil
}

// buildPrompt lays the request's bodies out as a numbered list under a light context header,
// respecting maxBodies and promptCharLimit. It is deterministic given the same request.
func (o *Ollama) buildPrompt(req Request) string {
	var b strings.Builder

	if req.EventName != "" {
		fmt.Fprintf(&b, "Event: %s\n", req.EventName)
	}

	if req.Group != "" {
		fmt.Fprintf(&b, "Group: %s\n", req.Group)
	}

	b.WriteString("Memories:\n")

	used := 0

	for i, body := range req.Bodies {
		if i >= o.maxBodies {
			break
		}

		if used+len(body) > o.promptCharLimit {
			break
		}

		fmt.Fprintf(&b, "%d. %s\n", i+1, body)

		used += len(body)
	}

	return b.String()
}

// Enabled reports that a real summariser is configured.
func (o *Ollama) Enabled() bool {
	return true
}

// Compile-time check that *Ollama satisfies Summariser.
var _ Summariser = (*Ollama)(nil)
