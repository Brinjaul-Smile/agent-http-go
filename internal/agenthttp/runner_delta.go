package agenthttp

import (
	"encoding/json"
	"strings"
)

type jsonLineStreamWriter struct {
	downstream StreamWriter
	parser     jsonLineDeltaParser
	buffer     strings.Builder
}

type jsonLineDeltaParser interface {
	Deltas(string) []string
}

func newJSONLineStreamWriter(downstream StreamWriter, parser jsonLineDeltaParser) *jsonLineStreamWriter {
	return &jsonLineStreamWriter{
		downstream: downstream,
		parser:     parser,
	}
}

func (w *jsonLineStreamWriter) WriteDelta(chunk string) error {
	w.buffer.WriteString(chunk)
	for {
		pending := w.buffer.String()
		index := strings.IndexByte(pending, '\n')
		if index < 0 {
			return nil
		}

		line := pending[:index]
		w.buffer.Reset()
		w.buffer.WriteString(pending[index+1:])
		if err := w.writeLine(line); err != nil {
			return err
		}
	}
}

func (w *jsonLineStreamWriter) Flush() error {
	line := w.buffer.String()
	w.buffer.Reset()
	return w.writeLine(line)
}

func (w *jsonLineStreamWriter) writeLine(line string) error {
	if w.downstream == nil || w.parser == nil {
		return nil
	}
	for _, delta := range w.parser.Deltas(strings.TrimSpace(line)) {
		if delta == "" {
			continue
		}
		if err := w.downstream.WriteDelta(delta); err != nil {
			return err
		}
	}
	return nil
}

type claudeJSONLDeltaParser struct {
	lastAssistantText string
}

func newClaudeJSONLDeltaParser() *claudeJSONLDeltaParser {
	return &claudeJSONLDeltaParser{}
}

func (p *claudeJSONLDeltaParser) Deltas(line string) []string {
	if line == "" {
		return nil
	}
	var payload any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		return nil
	}

	if deltas := extractExplicitDeltas(payload); len(deltas) > 0 {
		normalized := make([]string, 0, len(deltas))
		for _, delta := range deltas {
			nextDelta, nextText := normalizeTextDelta(p.lastAssistantText, delta)
			p.lastAssistantText = nextText
			if nextDelta != "" {
				normalized = append(normalized, nextDelta)
			}
		}
		return normalized
	}

	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	if eventType, _ := root["type"].(string); eventType != "assistant" {
		return nil
	}

	text := collectClaudeAssistantText(root["message"])
	if text == "" || text == p.lastAssistantText {
		return nil
	}
	if strings.HasPrefix(text, p.lastAssistantText) {
		delta := strings.TrimPrefix(text, p.lastAssistantText)
		p.lastAssistantText = text
		return []string{delta}
	}
	p.lastAssistantText = text
	return []string{text}
}

func normalizeTextDelta(previous string, incoming string) (string, string) {
	if incoming == "" {
		return "", previous
	}
	if previous == "" {
		return incoming, incoming
	}
	if incoming == previous || strings.HasPrefix(previous, incoming) {
		return "", previous
	}
	if strings.HasPrefix(incoming, previous) {
		return strings.TrimPrefix(incoming, previous), incoming
	}
	return incoming, previous + incoming
}

func extractExplicitDeltas(value any) []string {
	var deltas []string
	walkJSON(value, func(key string, current any) {
		if key != "delta" {
			return
		}
		switch typed := current.(type) {
		case string:
			deltas = append(deltas, typed)
		case map[string]any:
			if text, ok := typed["text"].(string); ok {
				deltas = append(deltas, text)
			}
		}
	})
	return deltas
}

func collectClaudeAssistantText(value any) string {
	message, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	content, ok := message["content"].([]any)
	if !ok {
		return ""
	}

	var builder strings.Builder
	for _, item := range content {
		contentItem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := contentItem["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func walkJSON(value any, visit func(string, any)) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			visit(key, child)
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkJSON(child, visit)
		}
	}
}
