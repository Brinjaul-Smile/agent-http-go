package agenthttp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// GormSessionStore 基于 GORM 实现 SessionStore，当前支持 SQLite 和 MySQL。
type GormSessionStore struct {
	// db 承载 ORM 查询、事务和 AutoMigrate；HTTP 层只通过 SessionStore 接口依赖它。
	db *gorm.DB
	// sqlDB 用于连接池参数、SQLite PRAGMA 检查和关闭底层连接。
	sqlDB *sql.DB
}

// gormSession 是 sessions 表的持久化模型。
// 这里不直接复用 API 层 Session，避免 GORM tag 泄漏到 HTTP 响应模型。
type gormSession struct {
	ID        string `gorm:"column:id;primaryKey;size:128"`
	Agent     string `gorm:"column:agent;size:64;not null"`
	Cwd       string `gorm:"column:cwd;type:text;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  []gormSessionMessage `gorm:"foreignKey:SessionID;constraint:OnDelete:CASCADE"`
}

func (gormSession) TableName() string {
	return "sessions"
}

// gormSessionMessage 是 session_messages 表的持久化模型。
// 两个复合索引分别服务于完整历史查询和“只取成功消息”的上下文查询。
type gormSessionMessage struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement;index:idx_session_messages_session_id_id,priority:2;index:idx_session_messages_session_id_status_id,priority:3"`
	SessionID string    `gorm:"column:session_id;size:128;not null;index:idx_session_messages_session_id_id,priority:1;index:idx_session_messages_session_id_status_id,priority:1"`
	Role      string    `gorm:"column:role;size:32;not null"`
	Content   string    `gorm:"column:content;type:longtext;not null"`
	Status    string    `gorm:"column:status;size:32;not null;index:idx_session_messages_session_id_status_id,priority:2"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

func (gormSessionMessage) TableName() string {
	return "session_messages"
}

// OpenSQLiteSessionStore 打开本地 SQLite 文件并执行 GORM AutoMigrate。
func OpenSQLiteSessionStore(path string) (*GormSessionStore, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite session database path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := gorm.Open(gormsqlite.Open(path), gormConfig())
	if err != nil {
		return nil, err
	}
	store, err := newGormSessionStore(db)
	if err != nil {
		return nil, err
	}
	store.sqlDB.SetMaxOpenConns(1)

	// SQLite 的外键和 WAL 是连接级行为，需要在 AutoMigrate 前显式设置。
	if err := store.configureSQLite(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.migrate(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// OpenMySQLSessionStore 打开 MySQL 连接并执行 GORM AutoMigrate。
func OpenMySQLSessionStore(dsn string) (*GormSessionStore, error) {
	normalizedDSN, err := normalizeMySQLDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(gormmysql.Open(normalizedDSN), gormConfig())
	if err != nil {
		return nil, err
	}
	store, err := newGormSessionStore(db)
	if err != nil {
		return nil, err
	}
	store.sqlDB.SetConnMaxLifetime(5 * time.Minute)
	store.sqlDB.SetMaxOpenConns(10)
	store.sqlDB.SetMaxIdleConns(5)

	if err := store.migrate(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func gormConfig() *gorm.Config {
	return &gorm.Config{
		// 服务层已有统一日志；默认关闭 GORM SQL 日志，避免正常请求输出过多噪音。
		Logger: logger.Default.LogMode(logger.Silent),
	}
}

func newGormSessionStore(db *gorm.DB) (*GormSessionStore, error) {
	// 保留 sql.DB 是为了管理连接池和关闭连接；业务读写只走 GORM。
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	return &GormSessionStore{db: db, sqlDB: sqlDB}, nil
}

func (s *GormSessionStore) configureSQLite(ctx context.Context) error {
	// foreign_keys 保证删除 session 时级联删除消息；busy_timeout 和 WAL 降低本地并发写入冲突。
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
	}
	for _, statement := range statements {
		if err := s.db.WithContext(ctx).Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *GormSessionStore) migrate() error {
	// AutoMigrate 只负责最小 schema 的幂等创建；后续破坏性迁移应单独加版本化步骤。
	return s.db.AutoMigrate(&gormSession{}, &gormSessionMessage{})
}

// Close 关闭底层数据库连接。
func (s *GormSessionStore) Close() error {
	if s == nil || s.sqlDB == nil {
		return nil
	}
	return s.sqlDB.Close()
}

// GetSession 查询 session 元信息；不存在时返回 ok=false 而不是错误。
func (s *GormSessionStore) GetSession(ctx context.Context, id string) (Session, bool, error) {
	var row gormSession
	err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Session{}, false, nil
		}
		return Session{}, false, err
	}
	return sessionFromGorm(row), true, nil
}

// CreateSession 固定 session 的 agent/cwd 绑定，后续请求必须匹配。
func (s *GormSessionStore) CreateSession(ctx context.Context, create SessionCreate) (Session, error) {
	now := time.Now().UTC()
	row := gormSession{
		ID:        create.ID,
		Agent:     create.Agent,
		Cwd:       create.Cwd,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Session{}, err
	}
	return sessionFromGorm(row), nil
}

// ListMessages 返回消息历史，用于 GET /sessions/{sessionId} 观察和排查。
// limit 大于 0 时返回最近 limit 条消息，并保持时间升序。
func (s *GormSessionStore) ListMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error) {
	query := s.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("id ASC")
	if limit > 0 {
		// 先按倒序取最近 N 条，再在外层升序排列，保证 API 返回仍符合时间顺序。
		query = s.db.WithContext(ctx).
			Table("(?) AS recent_messages",
				s.db.Model(&gormSessionMessage{}).
					Where("session_id = ?", sessionID).
					Order("id DESC").
					Limit(limit),
			).
			Order("id ASC")
	}

	var rows []gormSessionMessage
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	return messagesFromGorm(rows), nil
}

// ListContextMessages 返回最近的成功消息。失败和超时记录会保留在完整历史里，
// 但不进入 prompt，避免把错误输出污染后续回答。
func (s *GormSessionStore) ListContextMessages(ctx context.Context, sessionID string, maxMessages int) ([]SessionMessage, error) {
	var rows []gormSessionMessage
	err := s.db.WithContext(ctx).
		// 只取成功消息参与上下文拼接；失败和超时仍保留在完整历史里用于审计。
		Table("(?) AS context_messages",
			s.db.Model(&gormSessionMessage{}).
				Where("session_id = ? AND status = ?", sessionID, SessionStatusOK).
				Order("id DESC").
				Limit(maxMessages),
		).
		Order("id ASC").
		Find(&rows).
		Error
	if err != nil {
		return nil, err
	}
	return messagesFromGorm(rows), nil
}

// AppendTurn 将 user/assistant 两条消息放在一个事务里写入，
// 保证 GET 历史和下一轮上下文不会看到半个 turn。
func (s *GormSessionStore) AppendTurn(ctx context.Context, turn SessionTurn) error {
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// user 和 assistant 必须作为同一个 turn 原子写入，避免历史里出现半轮对话。
		rows := []gormSessionMessage{
			{
				SessionID: turn.SessionID,
				Role:      SessionRoleUser,
				Content:   turn.UserContent,
				Status:    turn.Status,
				CreatedAt: now,
			},
			{
				SessionID: turn.SessionID,
				Role:      SessionRoleAssistant,
				Content:   turn.AssistantContent,
				Status:    turn.Status,
				CreatedAt: now,
			},
		}
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		return tx.Model(&gormSession{}).Where("id = ?", turn.SessionID).Update("updated_at", now).Error
	})
}

// DeleteSession 依赖外键级联删除消息；不存在时也视为成功，保持 DELETE 幂等。
func (s *GormSessionStore) DeleteSession(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Delete(&gormSession{ID: id}).Error
}

func sessionFromGorm(row gormSession) Session {
	// 对外统一返回 UTC 时间，避免 SQLite/MySQL 驱动的本地时区差异泄漏到 API。
	return Session{
		ID:        row.ID,
		Agent:     row.Agent,
		Cwd:       row.Cwd,
		CreatedAt: row.CreatedAt.UTC(),
		UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func messagesFromGorm(rows []gormSessionMessage) []SessionMessage {
	messages := make([]SessionMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, SessionMessage{
			ID:        row.ID,
			SessionID: row.SessionID,
			Role:      row.Role,
			Content:   row.Content,
			Status:    row.Status,
			CreatedAt: row.CreatedAt.UTC(),
		})
	}
	return messages
}

func normalizeMySQLDSN(dsn string) (string, error) {
	if strings.TrimSpace(dsn) == "" {
		return "", fmt.Errorf("mysql session dsn must not be empty")
	}
	config, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	// MySQL DATETIME 需要 parseTime 才能被扫描进 time.Time；调用方不用在 DSN 里手写。
	config.ParseTime = true
	if config.Loc == nil {
		config.Loc = time.UTC
	}
	return config.FormatDSN(), nil
}
