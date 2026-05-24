package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers/common"
)

// nativeContent mirrors Gemini's contents[].
type nativeContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []nativePart `json:"parts"`
}

// nativePart is one element of contents[].parts.
type nativePart struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *nativeInlineData `json:"inlineData,omitempty"`
	FunctionCall     *nativeFuncCall   `json:"functionCall,omitempty"`
	FunctionResponse *nativeFuncResp   `json:"functionResponse,omitempty"`
}

type nativeInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type nativeFuncCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type nativeFuncResp struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// nativeTool carries function declarations.
type nativeTool struct {
	FunctionDeclarations []nativeFuncDecl `json:"functionDeclarations"`
}

type nativeFuncDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// nativeRequest is the full generateContent request body.
type nativeRequest struct {
	Contents          []nativeContent `json:"contents"`
	SystemInstruction *nativeContent  `json:"systemInstruction,omitempty"`
	Tools             []nativeTool    `json:"tools,omitempty"`
	GenerationConfig  map[string]any  `json:"generationConfig,omitempty"`
}

// nativeResponse mirrors generateContent response.
type nativeResponse struct {
	Candidates []struct {
		Content struct {
			Role  string       `json:"role"`
			Parts []nativePart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// hasAudioContent reports whether any message in the slice carries audio media.
func hasAudioContent(messages []Message) bool {
	for _, msg := range messages {
		for _, mediaURL := range msg.Media {
			if strings.HasPrefix(mediaURL, "data:audio/") {
				return true
			}
		}
	}
	return false
}

// stripPrefix removes the "google/" prefix from model names used by the
// openai-compat path. The native Gemini endpoint needs the bare model ID.
func stripPrefix(model string) string {
	return strings.TrimPrefix(strings.TrimSpace(model), modelPrefix)
}

// buildNativeURL returns the generateContent endpoint for a specific model.
func buildNativeURL(location, projectID, model string) string {
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		location, projectID, location, model,
	)
}

// buildNativeRequest translates picoclaw messages and tools into a Gemini
// generateContent request body. model is accepted for future thinking-config
// gating but is not used in v1.
func buildNativeRequest(
	messages []Message,
	tools []ToolDefinition,
	_ string,
	options map[string]any,
) *nativeRequest {
	contents := make([]nativeContent, 0, len(messages))
	toolCallNames := make(map[string]string)
	systemTexts := make([]string, 0, 1)

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			if t := strings.TrimSpace(msg.Content); t != "" {
				systemTexts = append(systemTexts, t)
			}

		case "user":
			// Tool-response message.
			if msg.ToolCallID != "" {
				toolName := common.ResolveToolResponseName(msg.ToolCallID, toolCallNames)
				contents = append(contents, nativeContent{
					Role: "user",
					Parts: []nativePart{{
						FunctionResponse: &nativeFuncResp{
							ID:   msg.ToolCallID,
							Name: toolName,
							Response: map[string]any{
								"result": msg.Content,
							},
						},
					}},
				})
				continue
			}

			// Ordinary user message (may carry audio).
			parts := make([]nativePart, 0, 1+len(msg.Media))
			if t := strings.TrimSpace(msg.Content); t != "" {
				parts = append(parts, nativePart{Text: t})
			}
			for _, mediaURL := range msg.Media {
				mimeType, data, ok := parseNativeDataURL(mediaURL)
				if !ok {
					continue
				}
				parts = append(parts, nativePart{
					InlineData: &nativeInlineData{
						MIMEType: mimeType,
						Data:     data,
					},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, nativeContent{Role: "user", Parts: parts})
			}

		case "assistant", "model":
			c := nativeContent{Role: "model"}
			if t := strings.TrimSpace(msg.Content); t != "" {
				c.Parts = append(c.Parts, nativePart{Text: t})
			}
			for _, tc := range msg.ToolCalls {
				name, args, _ := common.NormalizeStoredToolCall(tc)
				if name == "" {
					continue
				}
				if tc.ID != "" {
					toolCallNames[tc.ID] = name
				}
				c.Parts = append(c.Parts, nativePart{
					FunctionCall: &nativeFuncCall{
						ID:   tc.ID,
						Name: name,
						Args: args,
					},
				})
			}
			if len(c.Parts) > 0 {
				contents = append(contents, c)
			}

		case "tool":
			toolName := common.ResolveToolResponseName(msg.ToolCallID, toolCallNames)
			contents = append(contents, nativeContent{
				Role: "user",
				Parts: []nativePart{{
					FunctionResponse: &nativeFuncResp{
						ID:   msg.ToolCallID,
						Name: toolName,
						Response: map[string]any{
							"result": msg.Content,
						},
					},
				}},
			})
		}
	}

	req := &nativeRequest{Contents: contents}

	// System instruction.
	if len(systemTexts) > 0 {
		sysParts := make([]nativePart, 0, len(systemTexts))
		for _, t := range systemTexts {
			sysParts = append(sysParts, nativePart{Text: t})
		}
		req.SystemInstruction = &nativeContent{Parts: sysParts}
	}

	// Tools.
	if len(tools) > 0 {
		decls := make([]nativeFuncDecl, 0, len(tools))
		for _, t := range tools {
			if t.Type != "function" {
				continue
			}
			decls = append(decls, nativeFuncDecl{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
		if len(decls) > 0 {
			req.Tools = []nativeTool{{FunctionDeclarations: decls}}
		}
	}

	// Generation config.
	genConfig := make(map[string]any)
	if val, ok := options["max_tokens"]; ok {
		if n, ok := common.AsInt(val); ok && n > 0 {
			genConfig["maxOutputTokens"] = n
		}
	}
	if temp, ok := common.AsFloat(options["temperature"]); ok {
		genConfig["temperature"] = temp
	}
	if len(genConfig) > 0 {
		req.GenerationConfig = genConfig
	}

	return req
}

// callNativeGenerateContent POSTs a generateContent request and parses the
// response back into the picoclaw LLMResponse shape.
func (p *Provider) callNativeGenerateContent(
	ctx context.Context,
	location, projectID string,
	model string,
	req *nativeRequest,
) (*LLMResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("vertex native: marshal request: %w", err)
	}

	url := buildNativeURL(location, projectID, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vertex native: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.userAgent != "" {
		httpReq.Header.Set("User-Agent", p.userAgent)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex native: HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, common.HandleErrorResponse(resp, url)
	}

	var apiResp nativeResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("vertex native: decode response: %w", err)
	}

	return parseNativeResponse(&apiResp), nil
}

// parseNativeResponse converts a generateContent response to LLMResponse.
func parseNativeResponse(resp *nativeResponse) *LLMResponse {
	var contentParts []string
	var reasoningParts []string
	toolCalls := make([]ToolCall, 0)
	finishReason := ""

	for _, candidate := range resp.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				contentParts = append(contentParts, part.Text)
			}
			if part.FunctionCall != nil {
				args := part.FunctionCall.Args
				if args == nil {
					args = make(map[string]any)
				}
				argsJSON, _ := json.Marshal(args)
				id := part.FunctionCall.ID
				if id == "" {
					id = fmt.Sprintf("call_%s_%d", part.FunctionCall.Name, time.Now().UnixNano())
				}
				toolCalls = append(toolCalls, ToolCall{
					ID:        id,
					Name:      part.FunctionCall.Name,
					Arguments: args,
					Function: &FunctionCall{
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}
		if candidate.FinishReason != "" {
			finishReason = candidate.FinishReason
		}
	}

	var usage *UsageInfo
	if resp.UsageMetadata.TotalTokenCount > 0 {
		usage = &UsageInfo{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}

	return &LLMResponse{
		Content:          strings.Join(contentParts, ""),
		ReasoningContent: strings.Join(reasoningParts, ""),
		ToolCalls:        toolCalls,
		FinishReason:     normalizeNativeFinishReason(finishReason, len(toolCalls)),
		Usage:            usage,
	}
}

func normalizeNativeFinishReason(reason string, numToolCalls int) string {
	if numToolCalls > 0 {
		return "tool_calls"
	}
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return "length"
	case "", "STOP":
		return "stop"
	default:
		return strings.ToLower(strings.TrimSpace(reason))
	}
}

// parseNativeDataURL extracts the full MIME type and base64 data from a
// data:<mimeType>;base64,<data> URL.  The MIME type is returned as-is
// (e.g. "audio/ogg"), which is what Vertex's native API requires.
func parseNativeDataURL(mediaURL string) (mimeType string, data string, ok bool) {
	if !strings.HasPrefix(mediaURL, "data:") {
		return "", "", false
	}
	payload := strings.TrimPrefix(mediaURL, "data:")
	header, d, found := strings.Cut(payload, ",")
	if !found {
		return "", "", false
	}
	mt, params, _ := strings.Cut(header, ";")
	mt = strings.TrimSpace(mt)
	d = strings.TrimSpace(d)
	if mt == "" || d == "" {
		return "", "", false
	}
	if !strings.Contains(strings.ToLower(params), "base64") {
		return "", "", false
	}
	return mt, d, true
}
