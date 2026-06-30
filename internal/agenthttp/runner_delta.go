package agenthttp

import (
	"encoding/json"
	"strings"
)

// jsonLineStreamWriter 把任意 stdout chunk 重组为 JSONL 行并提取文本 delta。
type jsonLineStreamWriter struct {
	// downstream 接收解析后的正文增量。
	downstream StreamWriter
	// parser 负责从单行 JSON 中提取正文 delta。
	parser jsonLineDeltaParser
	// buffer 保存尚未凑齐换行符的半行 stdout。
	buffer strings.Builder
}

// jsonLineDeltaParser 从一行 JSONL 文本中提取零个或多个正文增量。
type jsonLineDeltaParser interface {
	// Deltas 返回该行里明确可转发给客户端的正文增量。
	Deltas(string) []string
}

// newJSONLineStreamWriter 创建面向 JSONL stdout 的 StreamWriter 适配器。
func newJSONLineStreamWriter(downstream StreamWriter, parser jsonLineDeltaParser) *jsonLineStreamWriter {
	return &jsonLineStreamWriter{
		downstream: downstream,
		parser:     parser,
	}
}

// WriteDelta 缓冲 stdout chunk，并在遇到换行时解析完整 JSONL 行。
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

// Flush 解析最后一个没有换行结尾的缓冲行。
func (w *jsonLineStreamWriter) Flush() error {
	line := w.buffer.String()
	w.buffer.Reset()
	return w.writeLine(line)
}

// writeLine 把单行 JSONL 交给 parser，并把解析出的 delta 写给下游。
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

// claudeJSONLDeltaParser 从 Claude stream-json 行中提取正文增量。
type claudeJSONLDeltaParser struct {
	// lastAssistantText 记录已转发文本，用于去重累计快照。
	lastAssistantText string
}

// newClaudeJSONLDeltaParser 创建 Claude stream-json delta 解析器。
func newClaudeJSONLDeltaParser() *claudeJSONLDeltaParser {
	return &claudeJSONLDeltaParser{}
}

// Deltas 从 Claude stream-json 单行事件中提取可转发正文。
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

// normalizeTextDelta 把纯增量、累计文本和重复文本归一化成未发送过的部分。
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

// extractExplicitDeltas 递归提取 JSON 结构里名为 delta 的文本字段。
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

// collectClaudeAssistantText 合并 Claude assistant message content 中的 text 字段。
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

// walkJSON 深度遍历 JSON 对象和数组，并对每个对象字段调用 visit。
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
