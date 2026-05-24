package vertex

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers/common"
	"github.com/sipeed/picoclaw/pkg/providers/openai_compat"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

type (
	LLMResponse    = protocoltypes.LLMResponse
	StreamChunk    = protocoltypes.StreamChunk
	Message        = protocoltypes.Message
	ToolDefinition = protocoltypes.ToolDefinition
)

const (
	// defaultLocation is the Vertex AI region used when none is configured.
	defaultLocation = "us-central1"

	// modelPrefix is prepended to model names before sending to the Vertex API.
	// Vertex's OpenAI-compatible endpoint requires "google/gemini-3-flash" etc.
	modelPrefix = "google/"

	defaultRequestTimeout = 120 * time.Second
)

// Provider implements picoclaw's LLMProvider interface for Vertex AI using
// the Vertex OpenAI-compatible Chat Completions endpoint. Authentication uses
// a service-account JSON key (or Application Default Credentials) with
// automatic token refresh via golang.org/x/oauth2/google.
//
// The endpoint URL has the form:
//
//	https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/endpoints/openapi
//
// Request/response format is OpenAI Chat Completions, delegated to openai_compat.Provider.
type Provider struct {
	delegate *openai_compat.Provider
}

// Config holds the parameters needed to construct a Vertex provider.
type Config struct {
	// ProjectID is the GCP project. Falls back to GCP_PROJECT_ID env var.
	ProjectID string
	// Location is the GCP region (e.g. "europe-west1"). Falls back to GCP_LOCATION or defaultLocation.
	Location string
	// CredentialsPath is the path to a service-account JSON key.
	// Falls back to GOOGLE_APPLICATION_CREDENTIALS env var, then ADC chain.
	CredentialsPath string
	// Proxy is an optional HTTP proxy URL.
	Proxy string
	// RequestTimeout overrides the default 120 s per-request timeout.
	RequestTimeout time.Duration
	// UserAgent is sent in the User-Agent header.
	UserAgent string
}

// NewProvider constructs a Provider from the given Config.
// It resolves credentials and builds an oauth2-backed HTTP client.
// This function performs no network I/O; the first token fetch happens on
// the first API call.
func NewProvider(cfg Config) (*Provider, error) {
	projectID := cfg.ProjectID
	if projectID == "" {
		projectID = os.Getenv("GCP_PROJECT_ID")
	}
	if projectID == "" {
		return nil, fmt.Errorf("vertex: project_id is required (set GCP_PROJECT_ID or pass project_id in config)")
	}

	location := cfg.Location
	if location == "" {
		location = os.Getenv("GCP_LOCATION")
	}
	if location == "" {
		location = defaultLocation
	}

	credPath := cfg.CredentialsPath
	if credPath == "" {
		credPath = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	}

	// Build the Vertex AI OpenAI-compatible base URL.
	// POST .../chat/completions will be appended by openai_compat.
	apiBase := buildAPIBase(location, projectID)

	// Create an auto-refreshing OAuth2 token source.
	ts, err := newTokenSource(context.Background(), credPath)
	if err != nil {
		return nil, err
	}

	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	// Build a proxy-aware base transport and wrap it with oauth2 so every
	// request gets a valid Bearer token injected automatically.
	httpClient := common.NewHTTPClient(cfg.Proxy)
	httpClient.Transport = newOAuthTransport(ts, httpClient.Transport)
	httpClient.Timeout = timeout

	// Construct an openai_compat.Provider with no static API key
	// (authentication is handled by the oauth2 transport).
	delegate := openai_compat.NewProvider(
		"",      // no static API key — bearer token injected by transport
		apiBase,
		"",      // proxy is handled by httpClient.Transport
		openai_compat.WithHTTPClient(httpClient),
		openai_compat.WithUserAgent(cfg.UserAgent),
		openai_compat.WithProviderName("vertex"),
	)

	return &Provider{delegate: delegate}, nil
}

// Chat implements LLMProvider.Chat. The model name is automatically prefixed
// with "google/" before the request is sent (e.g. "gemini-3-flash" becomes
// "google/gemini-3-flash" on the wire).
func (p *Provider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	return p.delegate.Chat(ctx, messages, tools, addPrefix(model), options)
}

// ChatStream implements StreamingProvider.ChatStream.
func (p *Provider) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(accumulated string),
) (*LLMResponse, error) {
	return p.delegate.ChatStream(ctx, messages, tools, addPrefix(model), options, onChunk)
}

// ChatStreamEvents implements StreamingProvider.ChatStreamEvents.
func (p *Provider) ChatStreamEvents(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(StreamChunk),
) (*LLMResponse, error) {
	return p.delegate.ChatStreamEvents(ctx, messages, tools, addPrefix(model), options, onChunk)
}

// GetDefaultModel returns an empty string; model is always provided by config.
func (p *Provider) GetDefaultModel() string { return "" }

// SupportsNativeSearch reports false — Vertex AI does not support the
// OpenAI native search extension.
func (p *Provider) SupportsNativeSearch() bool { return false }

// SupportsThinking reports false for Vertex AI.
func (p *Provider) SupportsThinking() bool { return false }

// addPrefix prepends "google/" to the model name if not already present.
func addPrefix(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, modelPrefix) {
		return model
	}
	return modelPrefix + model
}

// buildAPIBase constructs the Vertex AI OpenAI-compatible base URL.
func buildAPIBase(location, projectID string) string {
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/endpoints/openapi",
		location, projectID, location,
	)
}
