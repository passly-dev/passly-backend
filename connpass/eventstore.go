package connpass

import (
	"database/sql"
	"errors"
	"log"
	"time"
)

// 一度取得した connpass のイベントを connpass_events テーブルに保存する。
// イベント詳細やイベント一覧を、connpass を呼ばずに返すために使う。

// 保存したイベントを、connpass から取り直さずにそのまま返す期間
const storedEventTTL = 3 * time.Hour

// upsertEvents は connpass API で取得したイベントを保存する。すでにあれば上書きする。
func upsertEvents(db *sql.DB, raw []v2Event) {
	for _, ce := range raw {
		ev, ok := toEvent(ce)
		if !ok {
			continue
		}
		_, err := db.Exec(`
			INSERT INTO connpass_events (
				event_id, title, catch, event_url, image_url, place,
				started_at, ended_at, limit_count, accepted, waiting, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, CURRENT_TIMESTAMP)
			ON CONFLICT (event_id) DO UPDATE SET
				title = EXCLUDED.title,
				catch = EXCLUDED.catch,
				event_url = EXCLUDED.event_url,
				image_url = EXCLUDED.image_url,
				place = EXCLUDED.place,
				started_at = EXCLUDED.started_at,
				ended_at = EXCLUDED.ended_at,
				limit_count = EXCLUDED.limit_count,
				accepted = EXCLUDED.accepted,
				waiting = EXCLUDED.waiting,
				updated_at = CURRENT_TIMESTAMP
		`, ev.ID, ev.Name, ce.Catch, ev.EventURL, ev.ImageURL, ev.Location,
			ev.StartAt, ev.EndAt, ev.Limit, ev.Accepted, ev.Waiting)
		if err != nil {
			log.Printf("upsert connpass event failed (event_id=%d): %v", ev.ID, err)
		}
	}
}

// loadStoredEvent は保存済みのイベントを1件返す。
// fresh は storedEventTTL 以内に connpass から取得したものかどうか。
func loadStoredEvent(db *sql.DB, eventID int) (detail EventDetail, fresh bool, ok bool) {
	var d EventDetail
	var catch, eventURL, imageURL, place sql.NullString
	var limit, accepted, waiting sql.NullInt64
	var updatedAt sql.NullTime
	err := db.QueryRow(`
		SELECT event_id, title, catch, event_url, image_url, place, started_at, ended_at,
		       limit_count, accepted, waiting, updated_at
		FROM connpass_events WHERE event_id = $1
	`, eventID).Scan(&d.ID, &d.Name, &catch, &eventURL, &imageURL, &place, &d.StartAt, &d.EndAt,
		&limit, &accepted, &waiting, &updatedAt)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("load stored connpass event failed (event_id=%d): %v", eventID, err)
		}
		return EventDetail{}, false, false
	}
	d.StartAt = d.StartAt.UTC()
	d.EndAt = d.EndAt.UTC()
	d.Description = catch.String
	d.EventURL = eventURL.String
	d.ImageURL = imageURL.String
	d.Location = place.String
	d.Limit = int(limit.Int64)
	d.Accepted = int(accepted.Int64)
	d.Count = d.Accepted
	d.Waiting = int(waiting.Int64)
	fresh = updatedAt.Valid && time.Since(updatedAt.Time) < storedEventTTL
	return d, fresh, true
}

// 一覧 GET /events で返す範囲。カレンダーで過去の月も表示するので、過去分も少し含める。
const listedEventsPastRange = 180 * 24 * time.Hour

// loadListedEvents は保存済みのイベント（Passly のユーザーが参加したもの）を、開催日時の古い順に返す。
func loadListedEvents(db *sql.DB) ([]Event, error) {
	rows, err := db.Query(`
		SELECT event_id, title, COALESCE(place, ''), COALESCE(event_url, ''), COALESCE(image_url, ''),
		       started_at, ended_at, COALESCE(accepted, 0), COALESCE(waiting, 0), COALESCE(limit_count, 0)
		FROM connpass_events
		WHERE ended_at >= $1
		ORDER BY started_at ASC
		LIMIT 200
	`, time.Now().Add(-listedEventsPastRange))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]Event, 0)
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.Name, &ev.Location, &ev.EventURL, &ev.ImageURL,
			&ev.StartAt, &ev.EndAt, &ev.Accepted, &ev.Waiting, &ev.Limit); err != nil {
			return nil, err
		}
		ev.StartAt = ev.StartAt.UTC()
		ev.EndAt = ev.EndAt.UTC()
		ev.Count = ev.Accepted
		events = append(events, ev)
	}
	return events, rows.Err()
}
