package proxy

// Compression (lite mode) — 5 deterministic structural passes on the request
// body before forwarding upstream. Ported from OmniRoute's lite.ts: zero regex
// content rewriting, zero LLM, ~15% token savings, <1ms.
//
// Handles both Ollama native (/api/chat) and OpenAI-compatible
// (/v1/chat/completions) message formats. /api/generate requests (no messages
// field) pass through uncompressed — ponytail: ceiling — only /api/chat and
// /v1/chat/completions are handled. Add generate-specific passes if needed.

import (
	"encoding/json"
	"strings"
)

// CompressionSettings is the persistent configuration for the lite compression
// engine. Stored as one JSON blob in the settings table (key "compression").
type CompressionSettings struct {
	Enabled        bool `json:"enabled"`         // master toggle
	PreserveSystem bool `json:"preserve_system"`  // skip system msgs in passes 1, 2, 4
	Whitespace     bool `json:"whitespace"`       // pass 1: collapse whitespace
	DedupSystem     bool `json:"dedup_system"`     // pass 2: drop duplicate system messages
	ToolTruncate   int  `json:"tool_truncate"`    // pass 3: max chars for tool content (0 = off)
	RemoveDupes    bool `json:"remove_dupes"`     // pass 4: drop consecutive identical messages
	ImageReplace   bool `json:"image_replace"`    // pass 5: replace base64 images with text
}

// defaultCompression returns the safe-on startup defaults: all passes enabled,
// tool truncate at 2000 chars, preserve_system on (don't touch system prompts).
func defaultCompression() CompressionSettings {
	return CompressionSettings{
		Enabled:        false, // off by default — user opts in
		PreserveSystem: true,
		Whitespace:     true,
		DedupSystem:    true,
		ToolTruncate:   2000,
		RemoveDupes:    true,
		ImageReplace:   true,
	}
}

// visionModelSubstrings lists known vision-capable Ollama model name fragments.
// Substring match, case-insensitive. False positive (images survive on a
// vision model) is harmless. False negative (images stripped from a vision
// model) is data loss — so we err broad. Keep this list updated as Ollama
// adds vision models.
var visionModelSubstrings = []string{
	"llava", "bakllava", "moondream", "qwen-vl", "qwen2.5-vl",
	"qwen2-vl", "minicpm-v", "llama3.2-vision", "mllama", "pixtral",
	"vision",
}

// isVisionModel reports whether the model name matches a known vision-capable
// model. Case-insensitive substring match.
func isVisionModel(model string) bool {
	m := strings.ToLower(model)
	for _, s := range visionModelSubstrings {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// compressBody applies lite compression to a request body. Returns the
// (possibly modified) body bytes. If the body is not JSON, has no messages
// field, or compression is disabled, the original body is returned unchanged.
func compressBody(body []byte, model string, cfg CompressionSettings) []byte {
	if !cfg.Enabled || len(body) == 0 {
		return body
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body // not JSON — pass through
	}

	msgs, ok := raw["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body // no messages array — /api/generate or empty
	}

	// Work on a copy so we can rebuild the array (passes 2 and 4 drop entries).
	out := make([]any, 0, len(msgs))

	// Pass 1: collapseWhitespace — operates on string content or text blocks
	// in array content. Skips system messages if preserve_system.
	if cfg.Whitespace {
		for _, m := range msgs {
			out = append(out, passWhitespace(m, cfg.PreserveSystem))
		}
	} else {
		out = append(out, msgs...)
	}

	// Pass 2: dedupSystemPrompt — drop duplicate system messages (keyed on
	// first 200 chars of trimmed content). Skipped entirely if preserve_system.
	if cfg.DedupSystem && !cfg.PreserveSystem {
		out = passDedupSystem(out)
	}

	// Pass 3: compressToolResults — truncate role: "tool" content > tool_truncate
	// chars. Also handles OpenAI tool role.
	if cfg.ToolTruncate > 0 {
		for i, m := range out {
			out[i] = passTruncateTool(m, cfg.ToolTruncate)
		}
	}

	// Pass 4: removeRedundantContent — drop consecutive messages with identical
	// role + content. Skips system messages if preserve_system.
	if cfg.RemoveDupes {
		out = passRemoveDupes(out, cfg.PreserveSystem)
	}

	// Pass 5: replaceImageUrls — replace base64 image data with text placeholders
	// if the model is not vision-capable.
	if cfg.ImageReplace && !isVisionModel(model) {
		for i, m := range out {
			out[i] = passReplaceImages(m)
		}
	}

	raw["messages"] = out
	result, err := json.Marshal(raw)
	if err != nil {
		return body // marshal failed — return original (safe fallback)
	}
	return result
}

// ---------- pass 1: collapseWhitespace ----------

// passWhitespace collapses 3+ newlines to 2, trims trailing spaces/tabs per
// line. Operates on string content or text blocks in array content. Skips
// system messages if preserveSystem is true.
func passWhitespace(m any, preserveSystem bool) any {
	msg, ok := m.(map[string]any)
	if !ok {
		return m
	}

	role, _ := msg["role"].(string)
	if preserveSystem && role == "system" {
		return m
	}

	switch c := msg["content"].(type) {
	case string:
		msg["content"] = collapseWhitespaceStr(c)
	case []any:
		for i, block := range c {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["type"].(string); ok && t == "text" {
					if text, ok := b["text"].(string); ok {
						b["text"] = collapseWhitespaceStr(text)
					}
				}
				c[i] = b
			}
		}
	}
	return msg
}

// collapseWhitespaceStr collapses 3+ newlines to 2 and trims trailing
// spaces/tabs per line.
func collapseWhitespaceStr(s string) string {
	// Collapse 3+ newlines to 2
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	// Trim trailing spaces/tabs per line
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.Join(lines, "\n")
}

// ---------- pass 2: dedupSystemPrompt ----------

// passDedupSystem drops duplicate system messages, keyed on the first 200
// chars of trimmed content. Only system messages are deduped.
func passDedupSystem(msgs []any) []any {
	seen := make(map[string]bool)
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" {
			out = append(out, m)
			continue
		}
		key := systemKey(msg)
		if seen[key] {
			continue // drop duplicate
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

// systemKey extracts a dedup key from a system message: first 200 chars of
// trimmed content (string or first text block in array).
func systemKey(msg map[string]any) string {
	var text string
	switch c := msg["content"].(type) {
	case string:
		text = c
	case []any:
		for _, block := range c {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["type"].(string); ok && t == "text" {
					if s, ok := b["text"].(string); ok {
						text = s
						break
					}
				}
			}
		}
	}
	text = strings.TrimSpace(text)
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// ---------- pass 3: compressToolResults ----------

// passTruncateTool truncates role: "tool" message content to maxChars,
// appending "\n...[truncated]" if cut. Handles both string and array content.
func passTruncateTool(m any, maxChars int) any {
	msg, ok := m.(map[string]any)
	if !ok {
		return m
	}
	role, _ := msg["role"].(string)
	if role != "tool" {
		return m
	}

	switch c := msg["content"].(type) {
	case string:
		if len(c) > maxChars {
			msg["content"] = c[:maxChars] + "\n...[truncated]"
		}
	case []any:
		for i, block := range c {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["type"].(string); ok && t == "text" {
					if text, ok := b["text"].(string); ok && len(text) > maxChars {
						b["text"] = text[:maxChars] + "\n...[truncated]"
					}
				}
				c[i] = b
			}
		}
	}
	return msg
}

// ---------- pass 4: removeRedundantContent ----------

// passRemoveDupes drops consecutive messages with identical role + content.
// Skips system messages if preserveSystem is true.
func passRemoveDupes(msgs []any, preserveSystem bool) []any {
	if len(msgs) <= 1 {
		return msgs
	}
	out := make([]any, 0, len(msgs))
	out = append(out, msgs[0])
	for i := 1; i < len(msgs); i++ {
		prev := msgs[i-1]
		cur := msgs[i]
		if isDupe(prev, cur, preserveSystem) {
			continue
		}
		out = append(out, cur)
	}
	return out
}

// isDupe reports whether prev and cur are consecutive messages with the same
// role and content. System messages are never considered dupes if
// preserveSystem is true.
func isDupe(prev, cur any, preserveSystem bool) bool {
	p, ok1 := prev.(map[string]any)
	c, ok2 := cur.(map[string]any)
	if !ok1 || !ok2 {
		return false
	}

	pRole, _ := p["role"].(string)
	cRole, _ := c["role"].(string)
	if preserveSystem && (pRole == "system" || cRole == "system") {
		return false
	}
	if pRole != cRole {
		return false
	}

	return contentKey(p["content"]) == contentKey(c["content"])
}

// contentKey produces a comparable string from a message's content field,
// handling both string and array formats.
func contentKey(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		// For array content, join all text block values.
		var sb strings.Builder
		for _, block := range c {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["type"].(string); ok && t == "text" {
					if text, ok := b["text"].(string); ok {
						sb.WriteString(text)
					}
				}
			}
		}
		return sb.String()
	}
	return ""
}

// ---------- pass 5: replaceImageUrls ----------

// passReplaceImages replaces base64 image data with text placeholders.
// Handles both Ollama format (message.images — array of base64 strings) and
// OpenAI format (content array with image_url blocks containing data URLs).
func passReplaceImages(m any) any {
	msg, ok := m.(map[string]any)
	if !ok {
		return m
	}

	// Ollama format: message.images is an array of base64 strings.
	if images, ok := msg["images"].([]any); ok && len(images) > 0 {
		placeholders := make([]any, 0, len(images))
		for _, img := range images {
			if s, ok := img.(string); ok {
				placeholders = append(placeholders, imagePlaceholder(s))
			} else {
				placeholders = append(placeholders, img)
			}
		}
		msg["images"] = placeholders
	}

	// OpenAI format: content is an array with image_url blocks.
	if blocks, ok := msg["content"].([]any); ok {
		for i, block := range blocks {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["type"].(string); ok && t == "image_url" {
					if iu, ok := b["image_url"].(map[string]any); ok {
						if url, ok := iu["url"].(string); ok {
							iu["url"] = imagePlaceholder(url)
						}
					}
				}
				blocks[i] = b
			}
		}
	}

	return msg
}

// imagePlaceholder replaces a base64 image string or data URL with a text
// placeholder. Detects the format from the prefix.
func imagePlaceholder(s string) string {
	// Data URL: data:image/png;base64,...
	if strings.HasPrefix(s, "data:image/") {
		rest := s[len("data:image/"):]
		if idx := strings.Index(rest, ";"); idx >= 0 {
			format := rest[:idx]
			return "[image: " + format + "]"
		}
		return "[image: unknown]"
	}
	// Raw base64 — detect image format from the first bytes if possible.
	// ponytail: ceiling — magic byte detection covers common formats; a
	// full detector would use content.SniffedMIMEType. Upgrade path:
	// use http.DetectContentType on decoded bytes.
	if strings.HasPrefix(s, "/9j/") {
		return "[image: jpeg]"
	}
	if strings.HasPrefix(s, "iVBOR") {
		return "[image: png]"
	}
	if strings.HasPrefix(s, "R0lGOD") {
		return "[image: gif]"
	}
	if strings.HasPrefix(s, "UklGR") {
		return "[image: webp]"
	}
	return "[image: unknown]"
}