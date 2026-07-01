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
//
// Claude stream-json 会交错发出两类事件：
//  1. content_block_delta：携带 delta.text，是逐字流出的纯增量（或偶尔累计快照）；
//  2. type=assistant 消息快照：携带完整的 message.content[].text。
//
// 两路事件都需要去重。核心原则：维护同一个 lastEmittedText，记录已向客户端
// 发送的累计文本总量；无论哪路事件发出的文本都更新它，确保两路互不重放。
type claudeJSONLDeltaParser struct {
	// lastEmittedText 记录已向下游发出的正文累计值。
	// delta 路和快照路都读写同一个字段，保证彼此不重发已经推送过的内容。
	lastEmittedText string
	// sawExplicitDelta 标记 Claude 已经开始发送 content_block_delta 正文增量。
	// 之后若最终 assistant 快照与已发文本不是前缀关系，只能说明快照发生了修订，
	// SSE 无法回改已发送内容，因此不能把整段快照当成新的 delta 重发。
	sawExplicitDelta bool
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

	// content_block_delta 路：提取 delta 字段，与 lastEmittedText 做归一化去重。
	if deltas := extractExplicitDeltas(payload); len(deltas) > 0 {
		p.sawExplicitDelta = true
		normalized := make([]string, 0, len(deltas))
		for _, delta := range deltas {
			nextDelta, nextText := normalizeTextDelta(p.lastEmittedText, delta)
			p.lastEmittedText = nextText
			if nextDelta != "" {
				normalized = append(normalized, nextDelta)
			}
		}
		return normalized
	}

	// type=assistant 快照路：提取完整消息文本，与 lastEmittedText 做前缀去重。
	// 此时 lastEmittedText 已包含 delta 路已发送的内容，因此快照里已发送的部分会被正确跳过。
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	if eventType, _ := root["type"].(string); eventType != "assistant" {
		return nil
	}

	text := collectClaudeAssistantText(root["message"])
	if text == "" || text == p.lastEmittedText {
		return nil
	}
	if strings.HasPrefix(text, p.lastEmittedText) {
		delta := strings.TrimPrefix(text, p.lastEmittedText)
		p.lastEmittedText = text
		return []string{delta}
	}
	// 快照内容与已发送文本无前缀关系，分两种情况处理：
	if len(text) <= len(p.lastEmittedText) {
		// 快照落后于 delta（旧快照姗姗来迟），静默丢弃，保持状态不变。
		// 若此处更新 lastEmittedText 会导致状态倒退，破坏后续去重逻辑。
		return nil
	}
	if p.sawExplicitDelta {
		// Claude 的最终 assistant 快照可能会补词或改写标点，导致它不是已发送
		// delta 的简单前缀扩展。SSE 只能追加不能回改；这里丢弃快照，避免把
		// 最终全量文本作为最后一个 delta 重复推给客户端。
		return nil
	}
	// 快照内容与已发送文本完全不同（极端异常：内容被截断重置）。
	// 将整个快照作为新增量发出，并重置已发送状态。
	p.lastEmittedText = text
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
