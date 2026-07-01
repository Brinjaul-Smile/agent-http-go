package agenthttp

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteSessionStorePersistsAndDeletesSession(t *testing.T) {
	store, err := OpenSQLiteSessionStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	session, err := store.CreateSession(ctx, SessionCreate{
		ID:    "chat-1",
		Agent: "codex",
		Cwd:   "/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "chat-1" || session.Agent != "codex" || session.Cwd != "/workspace" {
		t.Fatalf("session = %#v", session)
	}

	if err := store.AppendTurn(ctx, SessionTurn{
		SessionID:        "chat-1",
		UserContent:      "hello",
		AssistantContent: "answer",
		Status:           SessionStatusOK,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurn(ctx, SessionTurn{
		SessionID:        "chat-1",
		UserContent:      "break",
		AssistantContent: "failed",
		Status:           SessionStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}

	messages, err := store.ListMessages(ctx, "chat-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(messages))
	}
	limitedMessages, err := store.ListMessages(ctx, "chat-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(limitedMessages) != 3 {
		t.Fatalf("limited messages len = %d, want 3", len(limitedMessages))
	}
	if limitedMessages[0].Content != "answer" || limitedMessages[2].Content != "failed" {
		t.Fatalf("limited messages = %#v", limitedMessages)
	}

	contextMessages, err := store.ListContextMessages(ctx, "chat-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(contextMessages) != 2 {
		t.Fatalf("context messages len = %d, want 2", len(contextMessages))
	}
	if contextMessages[0].Content != "hello" || contextMessages[1].Content != "answer" {
		t.Fatalf("context messages = %#v", contextMessages)
	}

	if err := store.DeleteSession(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	_, ok, err := store.GetSession(ctx, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("session still exists after delete")
	}
	messages, err = store.ListMessages(ctx, "chat-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages len after delete = %d, want 0", len(messages))
	}
}

func TestSQLiteSessionStoreConfiguresConnectionPragmas(t *testing.T) {
	store, err := OpenSQLiteSessionStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var busyTimeout int
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var journalMode string
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
}
