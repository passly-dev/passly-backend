package connpass

import (
	"database/sql"
	"errors"
	"log"
)

// 一度取得した connpass のイベントを connpass_events テーブルに保存する。
// イベント詳細の取得で connpass が失敗したとき（レート制限など）に、保存済みのものを返すために使う。

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

// loadStoredEvent は保存済みのイベントを1件返す。connpass が使えないときの予備。
func loadStoredEvent(db *sql.DB, eventID int) (EventDetail, bool) {
	var d EventDetail
	var catch, eventURL, imageURL, place sql.NullString
	var limit, accepted, waiting sql.NullInt64
	err := db.QueryRow(`
		SELECT event_id, title, catch, event_url, image_url, place, started_at, ended_at,
		       limit_count, accepted, waiting
		FROM connpass_events WHERE event_id = $1
	`, eventID).Scan(&d.ID, &d.Name, &catch, &eventURL, &imageURL, &place, &d.StartAt, &d.EndAt,
		&limit, &accepted, &waiting)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("load stored connpass event failed (event_id=%d): %v", eventID, err)
		}
		return EventDetail{}, false
	}
	d.Description = catch.String
	d.EventURL = eventURL.String
	d.ImageURL = imageURL.String
	d.Location = place.String
	d.Limit = int(limit.Int64)
	d.Accepted = int(accepted.Int64)
	d.Count = d.Accepted
	d.Waiting = int(waiting.Int64)
	return d, true
}
