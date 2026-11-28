package connpass

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

// すれ違いを、2人とも参加している connpass イベントの開催時間内（開始30分前〜終了30分後）に限定するための処理。
//
// すれ違いのたびに connpass を呼ぶとレート制限（フリープランは12秒に1回・1日100回）に当たるので、
// 参加イベントは user_connpass_events テーブルに保存し、userEventsTTL より古いときだけ取り直す。

const (
	// 開始前・終了後に、すれ違いを認める余裕
	eventWindowMargin = 30 * time.Minute
	// 参加イベントを connpass から取り直す間隔
	userEventsTTL = 3 * time.Hour
	// これより長いイベントは複数日のイベントとして、毎日の開催時間だけを認める
	multiDayThreshold = 24 * time.Hour
)

// connpass の開催時間は日本時間で決まるので、複数日のイベントの「毎日」は日本時間で数える。
// Render のコンテナに tzdata がなくても動くよう、固定の +9 時間を使う。
var jst = time.FixedZone("Asia/Tokyo", 9*60*60)

// saveUserEvents はユーザーの参加イベントを丸ごと入れ替えて保存し、取得時刻を更新する。
func saveUserEvents(db *sql.DB, userID string, events []Event) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx failed: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM user_connpass_events WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete old events failed: %w", err)
	}
	for _, ev := range events {
		if _, err := tx.Exec(
			`INSERT INTO user_connpass_events (user_id, event_id, title, started_at, ended_at)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (user_id, event_id) DO NOTHING`,
			userID, ev.ID, ev.Name, ev.StartAt, ev.EndAt,
		); err != nil {
			return fmt.Errorf("insert event failed (event_id=%d): %w", ev.ID, err)
		}
	}
	if _, err := tx.Exec(`UPDATE users SET connpass_synced_at = CURRENT_TIMESTAMP WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("update synced_at failed: %w", err)
	}
	return tx.Commit()
}

// syncUserEvents は、保存してある参加イベントが古ければ connpass から取り直す。
// 取得に失敗したときは、保存済みのもの（古くても）をそのまま使う。
func syncUserEvents(db *sql.DB, userID string) error {
	var stored sql.NullString
	var syncedAt sql.NullTime
	err := db.QueryRow(
		`SELECT connpass_username, connpass_synced_at FROM users WHERE id = $1`, userID,
	).Scan(&stored, &syncedAt)
	if err != nil {
		return fmt.Errorf("user lookup failed: %w", err)
	}
	if syncedAt.Valid && time.Since(syncedAt.Time) < userEventsTTL {
		return nil
	}
	if recentlyFailed(userID) {
		// 失敗した直後は取り直さない（一括送信で1件ごとに connpass を呼ばないように）
		return nil
	}

	nickname := NormalizeUsername(stored.String)
	if nickname == "" {
		// connpass 未設定なら参加イベントは無し
		return saveUserEvents(db, userID, nil)
	}

	events, err := fetchParticipatedEvents(db, nickname)
	if err != nil {
		markFailed(userID)
		return fmt.Errorf("fetch connpass events failed (user=%s): %w", nickname, err)
	}
	return saveUserEvents(db, userID, events)
}

// SyncUserEventsInBackground は、参加イベントが古ければ裏で connpass から取り直して保存する。
// ログインやホーム表示のときに呼び、すれ違いの判定の時点で保存済みにしておく
// （判定のときに2人分を続けて取得すると、connpass の「12秒に1回」の制限に当たるため）。
// 呼び出し元の応答は待たせない。
func SyncUserEventsInBackground(db *sql.DB, userID string) {
	go func() {
		if err := syncUserEvents(db, userID); err != nil {
			log.Printf("background connpass events sync failed (user_id=%s): %v", userID, err)
		}
	}()
}

// 取得に失敗したユーザーと時刻。サーバーのメモリにあるので、再起動で消えてよい。
var (
	syncFailMu sync.Mutex
	syncFailAt = map[string]time.Time{}
)

const syncRetryInterval = 10 * time.Minute

func recentlyFailed(userID string) bool {
	syncFailMu.Lock()
	defer syncFailMu.Unlock()
	at, ok := syncFailAt[userID]
	return ok && time.Since(at) < syncRetryInterval
}

func markFailed(userID string) {
	syncFailMu.Lock()
	defer syncFailMu.Unlock()
	syncFailAt[userID] = time.Now()
}

// FindSharedEventAt は、2人とも参加していて、時刻 at が開催時間内（前後30分を含む）のイベントIDを返す。
// 見つからなければ ok = false。
func FindSharedEventAt(db *sql.DB, userA, userB string, at time.Time) (eventID int, ok bool, err error) {
	for _, id := range []string{userA, userB} {
		if serr := syncUserEvents(db, id); serr != nil {
			// 取り直せなくても、保存済みのイベントで判定を続ける
			log.Printf("connpass events sync failed, using stored events (user_id=%s): %v", id, serr)
		}
	}

	rows, err := db.Query(`
		SELECT a.event_id, a.started_at, a.ended_at
		FROM user_connpass_events a
		JOIN user_connpass_events b ON b.event_id = a.event_id AND b.user_id = $2
		WHERE a.user_id = $1
		  AND a.started_at <= $3
		  AND a.ended_at >= $4
		ORDER BY a.started_at DESC
	`, userA, userB, at.Add(eventWindowMargin), at.Add(-eventWindowMargin))
	if err != nil {
		return 0, false, fmt.Errorf("query shared events failed: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var start, end time.Time
		if err := rows.Scan(&id, &start, &end); err != nil {
			return 0, false, fmt.Errorf("scan shared event failed: %w", err)
		}
		if InEventWindow(start, end, at) {
			return id, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("iterate shared events failed: %w", err)
	}
	return 0, false, nil
}

// InEventWindow は、時刻 at がイベントの「開始30分前〜終了30分後」に入っているかを返す。
//
// 24時間より長いイベント（例: 9/19 10:00〜9/27 18:00）は、期間中の毎日について
// 「初日の開始時刻の30分前〜最終日の終了時刻の30分後」（例: 毎日 9:30〜18:30）だけを認める。
// connpass は日ごとの開催時間を持たないので、初日と最終日の時刻から推定する。
func InEventWindow(start, end, at time.Time) bool {
	if at.Before(start.Add(-eventWindowMargin)) || at.After(end.Add(eventWindowMargin)) {
		return false
	}
	if end.Sub(start) <= multiDayThreshold {
		return true
	}

	s, e, t := start.In(jst), end.In(jst), at.In(jst)
	openAt := timeOfDay(s)
	closeAt := timeOfDay(e)
	if closeAt <= openAt {
		// 夜から翌朝まで（例: 毎日 22:00〜翌2:00）
		closeAt += 24 * time.Hour
	}

	// 前日の開催が日付をまたいでいる場合と、翌日の開始30分前が今日にかかる場合も見る
	today := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, jst)
	for _, offset := range []int{-1, 0, 1} {
		day := today.AddDate(0, 0, offset)
		from := day.Add(openAt - eventWindowMargin)
		to := day.Add(closeAt + eventWindowMargin)
		if !t.Before(from) && !t.After(to) {
			return true
		}
	}
	return false
}

func timeOfDay(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second
}
