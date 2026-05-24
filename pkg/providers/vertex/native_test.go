package vertex

import (
	"encoding/base64"
	"testing"
)

// TestHasAudioContent verifies that hasAudioContent correctly identifies
// messages carrying audio data URLs.
func TestHasAudioContent(t *testing.T) {
	ogg := "data:audio/ogg;base64,abc123"
	wav := "data:audio/wav;base64,abc123"
	img := "data:image/png;base64,abc123"

	cases := []struct {
		name     string
		messages []Message
		want     bool
	}{
		{
			name:     "empty",
			messages: nil,
			want:     false,
		},
		{
			name: "text only",
			messages: []Message{
				{Role: "user", Content: "hello"},
			},
			want: false,
		},
		{
			name: "image only",
			messages: []Message{
				{Role: "user", Content: "look", Media: []string{img}},
			},
			want: false,
		},
		{
			name: "ogg audio",
			messages: []Message{
				{Role: "user", Media: []string{ogg}},
			},
			want: true,
		},
		{
			name: "wav audio",
			messages: []Message{
				{Role: "user", Media: []string{wav}},
			},
			want: true,
		},
		{
			name: "audio in earlier turn",
			messages: []Message{
				{Role: "user", Media: []string{ogg}},
				{Role: "assistant", Content: "I heard you"},
				{Role: "user", Content: "thanks"},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		got := hasAudioContent(tc.messages)
		if got != tc.want {
			t.Errorf("%s: hasAudioContent = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestStripPrefix verifies that stripPrefix removes "google/" and is idempotent.
func TestStripPrefix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"google/gemini-2.5-pro", "gemini-2.5-pro"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"google/gemini-3-flash", "gemini-3-flash"},
		{"  google/gemini-2.5-flash  ", "gemini-2.5-flash"},
		{"gemini-3-flash", "gemini-3-flash"},
	}
	for _, tc := range cases {
		got := stripPrefix(tc.input)
		if got != tc.want {
			t.Errorf("stripPrefix(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

// TestBuildNativeURL verifies the generateContent endpoint URL shape.
func TestBuildNativeURL(t *testing.T) {
	got := buildNativeURL("europe-west1", "picobot-497313", "gemini-2.5-pro")
	want := "https://europe-west1-aiplatform.googleapis.com/v1/projects/picobot-497313/locations/europe-west1/publishers/google/models/gemini-2.5-pro:generateContent"
	if got != want {
		t.Errorf("buildNativeURL:\n got  %q\n want %q", got, want)
	}
}

// TestBuildNativeRequest_SystemAndAudio verifies the request structure for a
// conversation with a system message and a user message carrying OGG audio.
func TestBuildNativeRequest_SystemAndAudio(t *testing.T) {
	audioData := base64.StdEncoding.EncodeToString([]byte("fake-ogg-bytes"))
	audioURL := "data:audio/ogg;base64," + audioData

	messages := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "transcribe this", Media: []string{audioURL}},
	}

	req := buildNativeRequest(messages, nil, "gemini-2.5-pro", nil)

	// System message must become systemInstruction, not a content entry.
	if req.SystemInstruction == nil {
		t.Fatal("SystemInstruction is nil; expected system message to be extracted")
	}
	if len(req.SystemInstruction.Parts) == 0 || req.SystemInstruction.Parts[0].Text != "You are a helpful assistant." {
		t.Errorf("SystemInstruction.Parts[0].Text = %q; want %q",
			req.SystemInstruction.Parts[0].Text, "You are a helpful assistant.")
	}

	// Only the user message should be in contents.
	if len(req.Contents) != 1 {
		t.Fatalf("Contents length = %d; want 1", len(req.Contents))
	}
	c := req.Contents[0]
	if c.Role != "user" {
		t.Errorf("Contents[0].Role = %q; want %q", c.Role, "user")
	}

	// Expect text part first, then inlineData part.
	if len(c.Parts) != 2 {
		t.Fatalf("Contents[0].Parts length = %d; want 2", len(c.Parts))
	}
	if c.Parts[0].Text != "transcribe this" {
		t.Errorf("Parts[0].Text = %q; want %q", c.Parts[0].Text, "transcribe this")
	}
	if c.Parts[1].InlineData == nil {
		t.Fatal("Parts[1].InlineData is nil; expected inlineData block")
	}
	if c.Parts[1].InlineData.MIMEType != "audio/ogg" {
		t.Errorf("InlineData.MIMEType = %q; want %q", c.Parts[1].InlineData.MIMEType, "audio/ogg")
	}
	if c.Parts[1].InlineData.Data != audioData {
		t.Errorf("InlineData.Data mismatch")
	}
}

// TestBuildNativeRequest_TextOnly verifies that a text-only conversation
// produces no systemInstruction when there is no system message.
func TestBuildNativeRequest_TextOnly(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "hello"},
	}
	req := buildNativeRequest(messages, nil, "gemini-2.5-pro", nil)

	if req.SystemInstruction != nil {
		t.Errorf("SystemInstruction should be nil for text-only conversation without system message")
	}
	if len(req.Contents) != 1 {
		t.Fatalf("Contents length = %d; want 1", len(req.Contents))
	}
	if req.Contents[0].Parts[0].Text != "hello" {
		t.Errorf("Contents[0].Parts[0].Text = %q; want %q", req.Contents[0].Parts[0].Text, "hello")
	}
}

// TestParseNativeDataURL verifies that full MIME types (not just format slugs)
// are returned — the native API needs "audio/ogg", not just "ogg".
func TestParseNativeDataURL(t *testing.T) {
	cases := []struct {
		url      string
		wantMime string
		wantOK   bool
	}{
		{"data:audio/ogg;base64,abc123", "audio/ogg", true},
		{"data:audio/wav;base64,abc123", "audio/wav", true},
		{"data:audio/mp3;base64,abc123", "audio/mp3", true},
		{"data:image/png;base64,abc123", "image/png", true},
		{"data:audio/ogg;charset=utf8,abc", "", false}, // no base64
		{"https://example.com/audio.ogg", "", false},   // not a data URL
	}
	for _, tc := range cases {
		mime, _, ok := parseNativeDataURL(tc.url)
		if ok != tc.wantOK {
			t.Errorf("parseNativeDataURL(%q) ok=%v; want %v", tc.url, ok, tc.wantOK)
			continue
		}
		if ok && mime != tc.wantMime {
			t.Errorf("parseNativeDataURL(%q) mimeType=%q; want %q", tc.url, mime, tc.wantMime)
		}
	}
}

// TestNormalizeNativeFinishReason verifies reason normalization.
func TestNormalizeNativeFinishReason(t *testing.T) {
	cases := []struct {
		reason       string
		numToolCalls int
		want         string
	}{
		{"STOP", 0, "stop"},
		{"", 0, "stop"},
		{"MAX_TOKENS", 0, "length"},
		{"SAFETY", 0, "safety"},
		{"STOP", 1, "tool_calls"},
		{"MAX_TOKENS", 2, "tool_calls"},
	}
	for _, tc := range cases {
		got := normalizeNativeFinishReason(tc.reason, tc.numToolCalls)
		if got != tc.want {
			t.Errorf("normalizeNativeFinishReason(%q, %d) = %q; want %q",
				tc.reason, tc.numToolCalls, got, tc.want)
		}
	}
}

// TestBuildNativeRequest_GenerationConfig verifies that max_tokens and
// temperature are passed to generationConfig.
func TestBuildNativeRequest_GenerationConfig(t *testing.T) {
	messages := []Message{{Role: "user", Content: "hi"}}
	options := map[string]any{
		"max_tokens":  2048,
		"temperature": 0.5,
	}
	req := buildNativeRequest(messages, nil, "gemini-2.5-pro", options)

	if req.GenerationConfig == nil {
		t.Fatal("GenerationConfig is nil")
	}
	if maxTok, ok := req.GenerationConfig["maxOutputTokens"].(int); !ok || maxTok != 2048 {
		t.Errorf("maxOutputTokens = %v; want 2048", req.GenerationConfig["maxOutputTokens"])
	}
	if temp, ok := req.GenerationConfig["temperature"].(float64); !ok || temp != 0.5 {
		t.Errorf("temperature = %v; want 0.5", req.GenerationConfig["temperature"])
	}
}

// TestBuildNativeRequest_Tools verifies that tool definitions are serialized
// into the Gemini functionDeclarations shape.
func TestBuildNativeRequest_Tools(t *testing.T) {
	messages := []Message{{Role: "user", Content: "hi"}}
	tools := []ToolDefinition{
		{
			Type: "function",
			Function: ToolFunctionDefinition{
				Name:        "get_weather",
				Description: "Return weather for a location",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	req := buildNativeRequest(messages, tools, "gemini-2.5-pro", nil)

	if len(req.Tools) != 1 {
		t.Fatalf("Tools length = %d; want 1", len(req.Tools))
	}
	decls := req.Tools[0].FunctionDeclarations
	if len(decls) != 1 {
		t.Fatalf("FunctionDeclarations length = %d; want 1", len(decls))
	}
	if decls[0].Name != "get_weather" {
		t.Errorf("FunctionDeclarations[0].Name = %q; want %q", decls[0].Name, "get_weather")
	}
}

// TestBuildNativeRequest_AudioMIMEFullType verifies that various audio MIME types
// are preserved verbatim (not reduced to a slug).
func TestBuildNativeRequest_AudioMIMEFullType(t *testing.T) {
	for _, mime := range []string{"audio/ogg", "audio/wav", "audio/mp3", "audio/aac", "audio/flac"} {
		data := base64.StdEncoding.EncodeToString([]byte("bytes"))
		url := "data:" + mime + ";base64," + data
		messages := []Message{{Role: "user", Media: []string{url}}}
		req := buildNativeRequest(messages, nil, "gemini-2.5-pro", nil)
		if len(req.Contents) == 0 || len(req.Contents[0].Parts) == 0 {
			t.Errorf("%s: no parts in request", mime)
			continue
		}
		var found bool
		for _, p := range req.Contents[0].Parts {
			if p.InlineData != nil && p.InlineData.MIMEType == mime {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: inlineData.mimeType not found or wrong in parts", mime)
		}
	}
}
