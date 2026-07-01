package agenthttp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type memorySessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	messages map[string][]SessionMessage
	nextID   int64
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{
		sessions: make(map[string]Session),
		messages: make(map[string][]SessionMessage),
	}
}

func (s *memorySessionStore) GetSession(_ context.Context, id string) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[id]
	return session, ok, nil
}

func (s *memorySessionStore) CreateSession(_ context.Context, create SessionCreate) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	session := Session{
		ID:        create.ID,
		Agent:     create.Agent,
		Cwd:       create.Cwd,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.sessions[session.ID] = session
	return session, nil
}

func (s *memorySessionStore) ListMessages(_ context.Context, sessionID string, limit int) ([]SessionMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	messages := s.messages[sessionID]
	if limit > 0 && len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return append([]SessionMessage(nil), messages...), nil
}

func (s *memorySessionStore) ListContextMessages(_ context.Context, sessionID string, maxMessages int) ([]SessionMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var okMessages []SessionMessage
	for _, message := range s.messages[sessionID] {
		if message.Status == SessionStatusOK {
			okMessages = append(okMessages, message)
		}
	}
	if len(okMessages) > maxMessages {
		okMessages = okMessages[len(okMessages)-maxMessages:]
	}
	return append([]SessionMessage(nil), okMessages...), nil
}

func (s *memorySessionStore) AppendTurn(_ context.Context, turn SessionTurn) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	for _, message := range []struct {
		role    string
		content string
	}{
		{role: SessionRoleUser, content: turn.UserContent},
		{role: SessionRoleAssistant, content: turn.AssistantContent},
	} {
		s.nextID++
		s.messages[turn.SessionID] = append(s.messages[turn.SessionID], SessionMessage{
			ID:        s.nextID,
			SessionID: turn.SessionID,
			Role:      message.role,
			Content:   message.content,
			Status:    turn.Status,
			CreatedAt: now,
		})
	}

	session := s.sessions[turn.SessionID]
	session.UpdatedAt = now
	s.sessions[turn.SessionID] = session
	return nil
}

func (s *memorySessionStore) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sessions, id)
	delete(s.messages, id)
	return nil
}

func TestBuildSessionPromptKeepsRecentCompleteTurnsWithinByteLimit(t *testing.T) {
	messages := []SessionMessage{
		{Role: SessionRoleUser, Content: "old question"},
		{Role: SessionRoleAssistant, Content: "old answer"},
		{Role: SessionRoleUser, Content: "recent question"},
		{Role: SessionRoleAssistant, Content: "recent answer"},
	}

	prompt := buildSessionPrompt(messages, "latest", 64)
	if strings.Contains(prompt, "old question") {
		t.Fatalf("prompt contains old turn despite byte limit:\n%s", prompt)
	}
	for _, expected := range []string{"recent question", "recent answer", "Latest user message:\nlatest"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q in:\n%s", expected, prompt)
		}
	}
}
