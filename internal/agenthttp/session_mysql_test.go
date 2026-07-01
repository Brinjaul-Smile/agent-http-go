package agenthttp

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

func TestOpenMySQLSessionStoreRejectsEmptyDSN(t *testing.T) {
	_, err := OpenMySQLSessionStore("")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "mysql session dsn must not be empty") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeMySQLDSNEnablesParseTime(t *testing.T) {
	dsn, err := normalizeMySQLDSN("user:pass@tcp(127.0.0.1:3306)/agent_http?charset=utf8mb4")
	if err != nil {
		t.Fatal(err)
	}

	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !config.ParseTime {
		t.Fatal("parseTime = false, want true")
	}
	if config.Loc != time.UTC {
		t.Fatalf("loc = %v, want UTC", config.Loc)
	}
}

func TestMySQLSessionStoreIntegration(t *testing.T) {
	dsn := os.Getenv("AGENT_HTTP_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("AGENT_HTTP_MYSQL_TEST_DSN is not set")
	}

	store, err := OpenMySQLSessionStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	sessionID := "mysql-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	defer func() {
		if err := store.DeleteSession(ctx, sessionID); err != nil {
			t.Logf("failed to clean mysql test session %q: %v", sessionID, err)
		}
	}()

	session, err := store.CreateSession(ctx, SessionCreate{
		ID:    sessionID,
		Agent: "claude",
		Cwd:   "/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != sessionID || session.Agent != "claude" || session.Cwd != "/workspace" {
		t.Fatalf("session = %#v", session)
	}

	if err := store.AppendTurn(ctx, SessionTurn{
		SessionID:        sessionID,
		UserContent:      "hello",
		AssistantContent: "answer",
		Status:           SessionStatusOK,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurn(ctx, SessionTurn{
		SessionID:        sessionID,
		UserContent:      "break",
		AssistantContent: "failed",
		Status:           SessionStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}

	messages, err := store.ListMessages(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(messages))
	}
	limitedMessages, err := store.ListMessages(ctx, sessionID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(limitedMessages) != 3 {
		t.Fatalf("limited messages len = %d, want 3", len(limitedMessages))
	}
	if limitedMessages[0].Content != "answer" || limitedMessages[2].Content != "failed" {
		t.Fatalf("limited messages = %#v", limitedMessages)
	}

	contextMessages, err := store.ListContextMessages(ctx, sessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(contextMessages) != 2 {
		t.Fatalf("context messages len = %d, want 2", len(contextMessages))
	}
	if contextMessages[0].Content != "hello" || contextMessages[1].Content != "answer" {
		t.Fatalf("context messages = %#v", contextMessages)
	}

	if err := store.DeleteSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	_, ok, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("session still exists after delete")
	}
	messages, err = store.ListMessages(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages len after delete = %d, want 0", len(messages))
	}
}
