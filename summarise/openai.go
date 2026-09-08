package summarise

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAI is a Summariser backed by any endpoint speaking the OpenAI chat-completions API: OpenAI
// itself, Azure OpenAI, vLLM, llama.cpp's server, LiteLLM, OpenRouter, a Bedrock or Anthropic
// gateway, a corporate proxy that fronts a model with a key and an audit trail - and Ollama, which
// serves /v1 alongside its native API.
//
// It exists because the constraint the native Ollama client imposes is not "you must run Ollama",
// it is that a deployment which already has a sanctioned model endpoint cannot use it, and would
// have to stand up a second inference server beside the memory store to summarise anything.
//
// Like the Ollama client this is a small hand-rolled HTTP client rather than an SDK: the endpoint
// is one JSON POST, and the alternative is a large dependency tree for a request struct with four
// fields.
type OpenAI struct {
	address         string
	model           string
	apiKey          string
	systemPrompt    string
	maxBodies       int
	promptCharLimit int
	temperature     float64
	client          *http.Client
}

// chatRequest is the body of a POST {address}/chat/completions call. stream is always false so the
// whole response arrives as one JSON object rather than a server-sent-event stream.
//
// Temperature is omitted when zero rather than sent as 0: several current models reject any
// explicit temperature at all, and "let the model decide" is the behaviour the Ollama client
// already gives a zero value.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature,omitempty"`
}

// chatMessage is one turn of the conversation. The summariser sends exactly two: the system
// instruction and the prompt.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the non-streamed response. Error is populated on failures, and some gateways
// return it with a 200 status, so it is checked regardless of the code.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`

	Error *apiError `json:"error"`
}

// apiError is the OpenAI error envelope, shared by the chat and embedding endpoints.
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// String renders the envelope for an error message, preferring the human-readable message.
func (e *apiError) String() string {
	if e == nil {
		return ""
	}

	if e.Message != "" {
		return e.Message
	}

	return strings.TrimSpace(e.Type + " " + e.Code)
}

// NewOpenAI builds an OpenAI-compatible summariser from cfg, applying the same defaults NewOllama
// does. It fails only on unusable configuration and makes no connectivity check, for the reason
// NewOllama gives: summarisation is optional and best-effort, and an unreachable model server must
// not stop the service starting.
//
// Address is the base the endpoint path is appended to, and is taken exactly as given - so it
// normally ends in /v1 ("https://api.openai.com/v1", "http://localhost:11434/v1"). That is the
// convention every OpenAI SDK uses for its base_url, which is what an operator already has to hand;
// guessing a /v1 on the end instead would break every gateway that mounts the API somewhere else.
func NewOpenAI(cfg Config) (*OpenAI, error) {
	address := strings.TrimRight(strings.TrimSpace(cfg.Address), "/")

	if address == "" {
		return nil, fmt.Errorf("llm.address must be set when llm.enabled is true")
	}

	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm.model must be set when llm.enabled is true")
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

	o := &OpenAI{
		address:         address,
		model:           strings.TrimSpace(cfg.Model),
		apiKey:          strings.TrimSpace(cfg.APIKey),
		systemPrompt:    systemPrompt,
		maxBodies:       maxBodies,
		promptCharLimit: promptCharLimit,
		temperature:     cfg.Temperature,
		client:          &http.Client{Timeout: timeout},
	}

	return o, nil
}

// Summarise sends the request's bodies to the chat-completions endpoint and returns the generated
// summary. The prompt is built and bounded by the same shared buildPrompt the Ollama client uses,
// so which provider is configured cannot change how much of the store leaves the process.
func (o *OpenAI) Summarise(ctx context.Context, req Request) (string, error) {
	if len(req.Bodies) == 0 {
		return "", fmt.Errorf("no memory bodies to summarise")
	}

	body := chatRequest{
		Model:  o.model,
		Stream: false,
		Messages: []chatMessage{
			{Role: "system", Content: o.systemPrompt},
			{Role: "user", Content: buildPrompt(req, o.maxBodies, o.promptCharLimit)},
		},
		Temperature: o.temperature,
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("failed to marshal the chat-completions request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.address+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("failed to build the chat-completions request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	setBearer(httpReq, o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("chat-completions request to %s failed: %w", o.address, err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read the chat-completions response: %w", err)
	}

	var out chatResponse

	// Decoded before the status check so a structured error envelope can be preferred over the raw
	// body, which for most providers is a wall of JSON with one useful sentence in it. A body that
	// does not decode is not an error yet - the status check below reports it verbatim.
	decodeErr := json.Unmarshal(raw, &out)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", statusError(o.address, resp.StatusCode, out.Error, raw, o.apiKey != "")
	}

	if decodeErr != nil {
		return "", fmt.Errorf("failed to decode the chat-completions response: %w", decodeErr)
	}

	if out.Error != nil {
		return "", fmt.Errorf("the model server at %s reported an error: %s", o.address, out.Error)
	}

	if len(out.Choices) == 0 {
		return "", fmt.Errorf("the model server at %s returned no choices", o.address)
	}

	summary := strings.TrimSpace(out.Choices[0].Message.Content)
	if summary == "" {
		return "", fmt.Errorf("the model server at %s returned an empty summary", o.address)
	}

	return summary, nil
}

// Enabled reports that a real summariser is configured.
func (o *OpenAI) Enabled() bool {
	return true
}

// Ping reports whether the endpoint is reachable and carries the configured model, for the
// deployment topology view. It is deliberately not part of the Summariser interface; callers assert
// for it optionally.
//
// It is more forgiving than the Ollama probe, because /models is the one part of the OpenAI surface
// a compatible endpoint is free not to implement: a 404 or 405 means a gateway that routes chat
// completions and nothing else, which is reachable and working, so it reports healthy rather than
// claiming a fault the operator cannot fix. A model missing from a list that IS served is degraded,
// the same judgement the Ollama probe makes about an unpulled model.
//
// An authentication failure is the exception and is reported as an outright error: a rejected key is
// not a degraded dependency, it is a configuration that will fail every call, and it is the single
// most likely thing to be wrong about a hosted endpoint.
func (o *OpenAI) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.address+"/models", nil)
	if err != nil {
		return fmt.Errorf("failed to build the request: %w", err)
	}

	setBearer(req, o.apiKey)

	res, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("the model server is unreachable at %s: %w", o.address, err)
	}

	defer func() { _ = res.Body.Close() }()

	// Not every OpenAI-compatible endpoint serves a model list; one that does not is still serving
	// the endpoint that matters.
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 2048))

		return statusError(o.address, res.StatusCode, nil, raw, o.apiKey != "")
	}

	var list modelsResponse

	// An unparseable or empty list is not evidence of anything: the endpoint answered. Only a list
	// that is served, non-empty, and missing the model says something worth reporting.
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil || len(list.Data) == 0 {
		return nil
	}

	if !list.has(o.model) {
		return fmt.Errorf("%w: model %q is not served by the endpoint at %s", ErrDegraded, o.model, o.address)
	}

	return nil
}

// modelsResponse is the response from GET {address}/models.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// has reports whether the endpoint serves a model. The comparison is exact, unlike the Ollama
// probe's: there is no ":latest" convention here, and a provider's ids are the literal strings its
// API accepts.
func (m modelsResponse) has(model string) bool {
	for _, served := range m.Data {
		if served.ID == model {
			return true
		}
	}

	return false
}

// setBearer adds an Authorization header when a key is configured. Shared with the Ollama clients,
// which send it too when one is set - see summarise.Config.APIKey.
func setBearer(req *http.Request, apiKey string) {
	if apiKey == "" {
		return
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
}

// statusError renders a non-2xx response, preferring a decoded error envelope over the raw body and
// naming the likely cause for the two status codes that have one.
//
// 401 and 403 get their own sentence because they are by far the most common failure against a
// hosted endpoint and the rawest bodies: a bare "Incorrect API key provided" says nothing about
// which of the service's settings is wrong, and an operator who has not configured a key at all
// needs to be told that rather than shown a 401.
func statusError(address string, code int, envelope *apiError, raw []byte, haveKey bool) error {
	detail := envelope.String()
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}

	switch code {

	case http.StatusUnauthorized, http.StatusForbidden:
		if !haveKey {
			return fmt.Errorf("the model server at %s rejected the request with %d and no llm.apiKey is set: %s", address, code, detail)
		}

		return fmt.Errorf("the model server at %s rejected the configured llm.apiKey with %d: %s", address, code, detail)

	}

	return fmt.Errorf("the model server at %s returned status %d: %s", address, code, detail)
}

// Compile-time check that *OpenAI satisfies Summariser.
var _ Summariser = (*OpenAI)(nil)
