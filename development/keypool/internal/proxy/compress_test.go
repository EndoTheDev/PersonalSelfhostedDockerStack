package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompressBody_HappyPath(t *testing.T) {
	// Build a chat request with: excessive whitespace, duplicate system
	// messages, a long tool result, back-to-back duplicate user messages,
	// and a base64 image. Verify all 5 passes reduce the body size without
	// corrupting the JSON structure.
	body := mustJSON(t, map[string]any{
		"model": "llama3",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "Hello\n\n\n\n\n   world   \n  \n"},
			map[string]any{"role": "user", "content": "Hello\n\n\n\n\n   world   \n  \n"},
			map[string]any{"role": "tool", "content": strings.Repeat("x", 5000)},
			map[string]any{"role": "user", "content": "what is this?", "images": []any{"iVBORw0KGgo="}},
		},
	})

	cfg := CompressionSettings{
		Enabled:        true,
		PreserveSystem: false, // allow system dedup
		Whitespace:     true,
		DedupSystem:    true,
		ToolTruncate:   2000,
		RemoveDupes:    true,
		ImageReplace:   true,
	}

	result := compressBody(body, "llama3", cfg)
	if len(result) >= len(body) {
		t.Fatalf("expected compressed body to be smaller: got %d, original %d", len(result), len(body))
	}

	// Verify the JSON is valid and structurally intact.
	var raw map[string]any
	if err := json.Unmarshal(result, &raw); err != nil {
		t.Fatalf("compressed body is not valid JSON: %v", err)
	}
	msgs, ok := raw["messages"].([]any)
	if !ok {
		t.Fatal("messages field missing or wrong type")
	}

	// System dedup: 2 system messages → 1
	sysCount := 0
	for _, m := range msgs {
		if msg, ok := m.(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "system" {
				sysCount++
			}
		}
	}
	if sysCount != 1 {
		t.Fatalf("expected 1 system message after dedup, got %d", sysCount)
	}

	// Tool truncation: content should be truncated
	for _, m := range msgs {
		if msg, ok := m.(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "tool" {
				content, _ := msg["content"].(string)
				if len(content) > 2100 { // 2000 + truncation suffix
					t.Fatalf("tool content not truncated: %d chars", len(content))
				}
				if !strings.Contains(content, "[truncated]") {
					t.Fatal("tool content missing [truncated] marker")
				}
			}
		}
	}

	// Image replacement: base64 should be replaced with text placeholder
	for _, m := range msgs {
		if msg, ok := m.(map[string]any); ok {
			if images, ok := msg["images"].([]any); ok && len(images) > 0 {
				first, _ := images[0].(string)
				if !strings.HasPrefix(first, "[image:") {
					t.Fatalf("image not replaced: %s", first)
				}
			}
		}
	}
}

func TestCompressBody_Disabled(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model":    "llama3",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	cfg := defaultCompression() // Enabled: false
	result := compressBody(body, "llama3", cfg)
	if string(result) != string(body) {
		t.Fatal("expected body unchanged when disabled")
	}
}

func TestCompressBody_NoMessages(t *testing.T) {
	// /api/generate has no messages field — should pass through.
	body := mustJSON(t, map[string]any{
		"model":  "llama3",
		"prompt": "hello",
	})
	cfg := CompressionSettings{Enabled: true, Whitespace: true}
	result := compressBody(body, "llama3", cfg)
	if string(result) != string(body) {
		t.Fatal("expected body unchanged when no messages field")
	}
}

func TestCompressBody_EmptyMessages(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model":    "llama3",
		"messages": []any{},
	})
	cfg := CompressionSettings{Enabled: true, Whitespace: true}
	result := compressBody(body, "llama3", cfg)
	if string(result) != string(body) {
		t.Fatal("expected body unchanged when messages is empty")
	}
}

func TestCompressBody_MalformedJSON(t *testing.T) {
	body := []byte(`{not valid json}`)
	cfg := CompressionSettings{Enabled: true, Whitespace: true}
	result := compressBody(body, "llama3", cfg)
	if string(result) != string(body) {
		t.Fatal("expected body unchanged when JSON is malformed")
	}
}

func TestCompressBody_VisionModelSkipsImageReplace(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "llava",
		"messages": []any{
			map[string]any{"role": "user", "content": "what is this?", "images": []any{"iVBORw0KGgo="}},
		},
	})
	cfg := CompressionSettings{
		Enabled:      true,
		ImageReplace: true,
	}
	result := compressBody(body, "llava", cfg)

	var raw map[string]any
	json.Unmarshal(result, &raw)
	msgs, _ := raw["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	images, _ := msg["images"].([]any)
	first, _ := images[0].(string)
	if first != "iVBORw0KGgo=" {
		t.Fatalf("image was replaced on a vision model: %s", first)
	}
}

func TestCompressBody_PreserveSystem(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "llama3",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
		},
	})
	cfg := CompressionSettings{
		Enabled:        true,
		PreserveSystem: true,
		DedupSystem:    true,
	}
	result := compressBody(body, "llama3", cfg)

	var raw map[string]any
	json.Unmarshal(result, &raw)
	msgs, _ := raw["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 system messages (preserve_system), got %d", len(msgs))
	}
}

func TestCompressBody_OpenAIArrayContent(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "llama3",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Hello\n\n\n\n   world   "},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBOR"}},
				},
			},
		},
	})
	cfg := CompressionSettings{
		Enabled:      true,
		Whitespace:   true,
		ImageReplace: true,
	}
	result := compressBody(body, "llama3", cfg)

	var raw map[string]any
	json.Unmarshal(result, &raw)
	msgs, _ := raw["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	content, _ := msg["content"].([]any)

	// Text block: whitespace collapsed
	textBlock, _ := content[0].(map[string]any)
	text, _ := textBlock["text"].(string)
	if strings.Contains(text, "\n\n\n") {
		t.Fatalf("whitespace not collapsed in array content: %q", text)
	}

	// Image block: URL replaced
	imgBlock, _ := content[1].(map[string]any)
	iu, _ := imgBlock["image_url"].(map[string]any)
	url, _ := iu["url"].(string)
	if !strings.HasPrefix(url, "[image:") {
		t.Fatalf("image_url not replaced: %s", url)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}