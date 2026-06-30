package agenthttp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteTimeFormat 是 SQLite 存储时间戳使用的格式，纳秒精度、可排序。
const sqliteTimeFormat = time.RFC3339Nano

// SQLiteSessionStore 基于纯 Go SQLite 实现的 SessionStore。
// 最大打开连接数为 1，避免并发写入时出现 busy/lock 竞态。
type SQLiteSessionStore struct {
	// db 是底层 SQLite 连接句柄。
	db *sql.DB
}

// OpenSQLiteSessionStore 打开本地 SQLite 文件并执行幂等建表。
// SQLite 是第一版实现，调用方仍通过 SessionStore 接口依赖它。
func OpenSQLiteSessionStore(path string) (*SQLiteSessionStore, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite session database path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 单文件 SQLite 在本服务里只承担轻量 session 存储；限制单连接可以
	// 避免并发写入时出现额外的 SQLite busy/lock 行为差异。
	db.SetMaxOpenConns(1)

	store := &SQLiteSessionStore{db: db}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close 关闭底层 SQLite 数据库连接。
func (s *SQLiteSessionStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// init 维护最小 schema。这里不用迁移框架，先保持启动时幂等创建；
// 后续如果 schema 演进，再引入版本表或迁移步骤。
func (s *SQLiteSessionStore) init(ctx context.Context) error {
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			agent TEXT NOT NULL,
			cwd TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_session_id_id ON session_messages(session_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_session_id_status_id ON session_messages(session_id, status, id)`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// GetSession 查询 session 元信息；不存在时返回 ok=false 而不是错误。
func (s *SQLiteSessionStore) GetSession(ctx context.Context, id string) (Session, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, agent, cwd, created_at, updated_at FROM sessions WHERE id = ?`, id)

	var (
		session   Session
		createdAt string
		updatedAt string
	)
	if err := row.Scan(&session.ID, &session.Agent, &session.Cwd, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, false, nil
		}
		return Session{}, false, err
	}

	var err error
	session.CreatedAt, err = parseSQLiteTime(createdAt)
	if err != nil {
		return Session{}, false, err
	}
	session.UpdatedAt, err = parseSQLiteTime(updatedAt)
	if err != nil {
		return Session{}, false, err
	}
	return session, true, nil
}

// CreateSession 固定 session 的 agent/cwd 绑定，后续请求必须匹配。
func (s *SQLiteSessionStore) CreateSession(ctx context.Context, create SessionCreate) (Session, error) {
	now := time.Now().UTC()
	session := Session{
		ID:        create.ID,
		Agent:     create.Agent,
		Cwd:       create.Cwd,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, agent, cwd, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		session.ID,
		session.Agent,
		session.Cwd,
		formatSQLiteTime(session.CreatedAt),
		formatSQLiteTime(session.UpdatedAt),
	)
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// ListMessages 返回完整消息历史，用于 GET /sessions/{sessionId} 观察和排查。
func (s *SQLiteSessionStore) ListMessages(ctx context.Context, sessionID string) ([]SessionMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, status, created_at FROM session_messages WHERE session_id = ? ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSessionMessages(rows)
}

// ListContextMessages 返回最近的成功消息。失败和超时记录会保留在完整历史里，
// 但不进入 prompt，避免把错误输出污染后续回答。
func (s *SQLiteSessionStore) ListContextMessages(ctx context.Context, sessionID string, maxMessages int) ([]SessionMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, status, created_at
		FROM (
			SELECT id, session_id, role, content, status, created_at
			FROM session_messages
			WHERE session_id = ? AND status = ?
			ORDER BY id DESC
			LIMIT ?
		)
		ORDER BY id ASC`,
		sessionID,
		SessionStatusOK,
		maxMessages,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSessionMessages(rows)
}

// AppendTurn 将 user/assistant 两条消息放在一个事务里写入，
// 保证 GET 历史和下一轮上下文不会看到半个 turn。
func (s *SQLiteSessionStore) AppendTurn(ctx context.Context, turn SessionTurn) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for _, message := range []struct {
		role    string
		content string
	}{
		{role: SessionRoleUser, content: turn.UserContent},
		{role: SessionRoleAssistant, content: turn.AssistantContent},
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_messages(session_id, role, content, status, created_at) VALUES (?, ?, ?, ?, ?)`,
			turn.SessionID,
			message.role,
			message.content,
			turn.Status,
			formatSQLiteTime(now),
		); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		formatSQLiteTime(now),
		turn.SessionID,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteSession 依赖外键级联删除消息；不存在时也视为成功，保持 DELETE 幂等。
func (s *SQLiteSessionStore) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// scanSessionMessages 统一处理 SQL 行到 API 模型的转换。
func scanSessionMessages(rows *sql.Rows) ([]SessionMessage, error) {
	var messages []SessionMessage
	for rows.Next() {
		var (
			message   SessionMessage
			createdAt string
		)
		if err := rows.Scan(
			&message.ID,
			&message.SessionID,
			&message.Role,
			&message.Content,
			&message.Status,
			&createdAt,
		); err != nil {
			return nil, err
		}
		parsed, err := parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		message.CreatedAt = parsed
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// formatSQLiteTime 将时间转为 UTC 并格式化为 SQLite 兼容字符串。
func formatSQLiteTime(value time.Time) string {
	return value.UTC().Format(sqliteTimeFormat)
}

// parseSQLiteTime 将 SQLite 时间字符串解析为 time.Time。
func parseSQLiteTime(value string) (time.Time, error) {
	return time.Parse(sqliteTimeFormat, value)
}
