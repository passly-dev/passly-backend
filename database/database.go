package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

func InitDB(db *sql.DB) {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
            id VARCHAR(50) PRIMARY KEY,
            name VARCHAR(100) NOT NULL,
            email VARCHAR(255) UNIQUE NOT NULL,
            password_hash TEXT NOT NULL,
            icon_url TEXT,
            cover_url TEXT,
            one_word TEXT,
            role VARCHAR(50),
            tech_stack TEXT,
            twitter_url TEXT,
            github_url TEXT,
            portfolio_url TEXT,
            affiliation TEXT,
            about TEXT,
            connpass_username VARCHAR(255),
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        );`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS connpass_username VARCHAR(255);`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS cover_url TEXT;`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS about TEXT;`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version INTEGER NOT NULL DEFAULT 0;`,
		`CREATE TABLE IF NOT EXISTS events (
            id SERIAL PRIMARY KEY,
            name VARCHAR(255) NOT NULL,
            location VARCHAR(255),
            start_at TIMESTAMP,
            end_at TIMESTAMP,
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        );`,
		`CREATE TABLE IF NOT EXISTS connpass_mock_events (
			event_id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			catch TEXT,
			description TEXT,
			event_url TEXT,
			started_at TIMESTAMPTZ NOT NULL,
			ended_at TIMESTAMPTZ,
			limit_count INTEGER,
			hash_tag TEXT,
			event_type TEXT,
			accepted INTEGER,
			waiting INTEGER,
			source_updated_at TIMESTAMPTZ,
			owner_id INTEGER,
			owner_nickname TEXT,
			owner_display_name TEXT,
			place TEXT,
			address TEXT,
			is_active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
			updated_row_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_connpass_mock_events_owner_started_at
			ON connpass_mock_events (owner_nickname, started_at);`,
		`CREATE TABLE IF NOT EXISTS event_participants (
            id SERIAL PRIMARY KEY,
            event_id INTEGER REFERENCES events(id),
            user_id VARCHAR(50) REFERENCES users(id),
            joined_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        );`,
		`CREATE TABLE IF NOT EXISTS encounters (
            id SERIAL PRIMARY KEY,
            my_id VARCHAR(50) REFERENCES users(id),
            target_id VARCHAR(50) REFERENCES users(id),
            event_id INTEGER REFERENCES events(id),
            is_confirmed BOOLEAN DEFAULT FALSE,
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        );`,
		`DELETE FROM encounters e
		 USING encounters d
		 WHERE e.id > d.id
		   AND DATE(e.created_at) = DATE(d.created_at)
		   AND LEAST(e.my_id, e.target_id) = LEAST(d.my_id, d.target_id)
		   AND GREATEST(e.my_id, e.target_id) = GREATEST(d.my_id, d.target_id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_encounters_pair_per_day
			ON encounters (LEAST(my_id, target_id), GREATEST(my_id, target_id), (DATE(created_at)));`,
		`CREATE TABLE IF NOT EXISTS ephemeral_tokens (
			token VARCHAR(64) PRIMARY KEY,
			user_id VARCHAR(50) REFERENCES users(id),
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		// オフライン用トークン事前配布API(2026-09-18)で使用。NULLの場合は従来通り
		// 開始時刻の制約なし(オンデマンド発行分)。
		`ALTER TABLE ephemeral_tokens ADD COLUMN IF NOT EXISTS valid_from TIMESTAMP;`,
		`CREATE INDEX IF NOT EXISTS idx_ephemeral_tokens_user_id ON ephemeral_tokens(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_ephemeral_tokens_expires_at ON ephemeral_tokens(expires_at);`,
		`CREATE TABLE IF NOT EXISTS close_friends (
			user_id VARCHAR(50) NOT NULL REFERENCES users(id),
			friend_id VARCHAR(50) NOT NULL REFERENCES users(id),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, friend_id),
			CHECK (user_id <> friend_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_close_friends_user_id ON close_friends(user_id);`,
		// すれ違いをイベント開催時間内に限定する(2026-11-28)。
		// 各ユーザーの connpass 参加イベントを保存し、すれ違いのたびに connpass を呼ばずに判定する。
		`CREATE TABLE IF NOT EXISTS user_connpass_events (
			user_id VARCHAR(50) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			event_id INTEGER NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			started_at TIMESTAMPTZ NOT NULL,
			ended_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (user_id, event_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_user_connpass_events_event_id ON user_connpass_events(event_id);`,
		// 参加イベントを最後に connpass から取り直した時刻。NULL なら未取得。
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS connpass_synced_at TIMESTAMPTZ;`,
		// encounters.event_id は自前の events テーブルを参照しているので、connpass のイベントIDは別の列に入れる。
		`ALTER TABLE encounters ADD COLUMN IF NOT EXISTS connpass_event_id INTEGER;`,
		// 一度取得した connpass のイベント(2026-11-28)。
		// イベント詳細の取得で connpass が失敗したとき（レート制限など）に、保存済みのものを返す。
		`CREATE TABLE IF NOT EXISTS connpass_events (
			event_id INTEGER PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			catch TEXT,
			event_url TEXT,
			place TEXT,
			started_at TIMESTAMPTZ NOT NULL,
			ended_at TIMESTAMPTZ NOT NULL,
			limit_count INTEGER,
			accepted INTEGER,
			waiting INTEGER,
			image_url TEXT,
			updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS tech_tags (
			id SERIAL PRIMARY KEY,
			name VARCHAR(50) UNIQUE NOT NULL,
			category VARCHAR(50),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			log.Fatalf("Table creation error: %v", err)
		}
	}
}

// 期限が切れてからこれ以上たった一時トークンは消す。
//
// オフラインで貯めたすれ違いは、すれ違った時刻を基準にトークンを照合する（ResolveUserIDFromTokenAt）。
// アプリはすれ違ってから48時間まで未送信のトークンを持っているので、それより十分長く残す。
// これより短くすると、48時間以内の一括送信でも invalid_token になる。
const ephemeralTokenRetention = 7 * 24 * time.Hour

// CleanupExpiredEphemeralTokens は、期限が切れてから ephemeralTokenRetention 以上たった一時トークンを消す。
func CleanupExpiredEphemeralTokens(db *sql.DB) (int64, error) {
	res, err := db.Exec(
		`DELETE FROM ephemeral_tokens WHERE expires_at < $1`,
		time.Now().UTC().Add(-ephemeralTokenRetention),
	)
	if err != nil {
		return 0, fmt.Errorf("delete expired ephemeral tokens failed: %w", err)
	}
	return res.RowsAffected()
}

// StartEphemeralTokenCleanup は、起動時と、その後1日ごとに古い一時トークンを消す。
// Render の無料プランはスリープと起動を繰り返すので、起動時の分だけでもほぼ足りる。
func StartEphemeralTokenCleanup(db *sql.DB) {
	run := func() {
		n, err := CleanupExpiredEphemeralTokens(db)
		if err != nil {
			log.Printf("ephemeral token cleanup failed: %v", err)
			return
		}
		log.Printf("ephemeral token cleanup removed %d tokens", n)
	}
	go func() {
		run()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			run()
		}
	}()
}

// SeedTechTags は選択式UI用の技術タグマスタをJSONファイルからupsertする。
// 同名タグは重複せず、再起動しても件数は増えない。
func SeedTechTags(db *sql.DB, filePath string) error {
	body, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read tech tags file failed: %w", err)
	}

	var payload struct {
		Tags []struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("parse tech tags file failed: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx failed: %w", err)
	}
	defer tx.Rollback()

	for _, t := range payload.Tags {
		if t.Name == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO tech_tags (name, category) VALUES ($1, $2)
			 ON CONFLICT (name) DO UPDATE SET category = EXCLUDED.category`,
			t.Name, t.Category,
		); err != nil {
			return fmt.Errorf("upsert tech tag failed (name=%s): %w", t.Name, err)
		}
	}

	return tx.Commit()
}
