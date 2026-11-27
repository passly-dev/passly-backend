package connpass

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

type ConnpassResponse struct {
	ResultsReturned  int             `json:"results_returned"`
	ResultsAvailable int             `json:"results_available"`
	ResultsStart     int             `json:"results_start"`
	Events           []ConnpassEvent `json:"events"`
}

type ConnpassEvent struct {
	EventID          int    `json:"event_id"`
	Title            string `json:"title"`
	Catch            string `json:"catch,omitempty"`
	Description      string `json:"description,omitempty"`
	EventURL         string `json:"event_url,omitempty"`
	StartedAt        string `json:"started_at"`
	EndedAt          string `json:"ended_at"`
	Limit            int    `json:"limit,omitempty"`
	HashTag          string `json:"hash_tag,omitempty"`
	EventType        string `json:"event_type,omitempty"`
	Accepted         int    `json:"accepted,omitempty"`
	Waiting          int    `json:"waiting,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	OwnerID          int    `json:"owner_id,omitempty"`
	OwnerNickname    string `json:"owner_nickname,omitempty"`
	OwnerDisplayName string `json:"owner_display_name,omitempty"`
	Place            string `json:"place"`
	Address          string `json:"address,omitempty"`
}

type Event struct {
	ID       int       `json:"id"`
	Name     string    `json:"name"`
	Location string    `json:"location"`
	EventURL string    `json:"event_url"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
	Count    int       `json:"count"`
	Accepted int       `json:"accepted"`
	Waiting  int       `json:"waiting"`
	Limit    int       `json:"limit"`
	// connpass のイベント画像（API v2 の image_url）。無いイベントは空文字。
	ImageURL string `json:"image_url"`
}

func isConnpassMockEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("CONNPASS_MOCK_MODE")))
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func MockFilePath() string {
	path := strings.TrimSpace(os.Getenv("CONNPASS_MOCK_FILE"))
	if path == "" {
		return "mock/connpass_events.json"
	}
	return path
}

func ShouldSeedMockOnStartup() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("CONNPASS_MOCK_SEED_ON_STARTUP")))
	if raw == "" {
		return true
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func SeedMockEventsFromFile(db *sql.DB, filePath string) error {
	body, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read mock file failed: %w", err)
	}

	var payload ConnpassResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("parse mock file failed: %w", err)
	}

	if len(payload.Events) == 0 {
		return errors.New("mock file has no events")
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx failed: %w", err)
	}
	defer tx.Rollback()

	upsertQuery := `
		INSERT INTO connpass_mock_events (
			event_id, title, catch, description, event_url,
			started_at, ended_at, limit_count, hash_tag, event_type,
			accepted, waiting, source_updated_at,
			owner_id, owner_nickname, owner_display_name,
			place, address, is_active, updated_row_at
		)
		VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13,
			$14, $15, $16,
			$17, $18, TRUE, CURRENT_TIMESTAMP
		)
		ON CONFLICT (event_id)
		DO UPDATE SET
			title = EXCLUDED.title,
			catch = EXCLUDED.catch,
			description = EXCLUDED.description,
			event_url = EXCLUDED.event_url,
			started_at = EXCLUDED.started_at,
			ended_at = EXCLUDED.ended_at,
			limit_count = EXCLUDED.limit_count,
			hash_tag = EXCLUDED.hash_tag,
			event_type = EXCLUDED.event_type,
			accepted = EXCLUDED.accepted,
			waiting = EXCLUDED.waiting,
			source_updated_at = EXCLUDED.source_updated_at,
			owner_id = EXCLUDED.owner_id,
			owner_nickname = EXCLUDED.owner_nickname,
			owner_display_name = EXCLUDED.owner_display_name,
			place = EXCLUDED.place,
			address = EXCLUDED.address,
			is_active = TRUE,
			updated_row_at = CURRENT_TIMESTAMP
	`

	seededCount := 0
	for _, ce := range payload.Events {
		startAt, err := time.Parse(time.RFC3339, ce.StartedAt)
		if err != nil {
			continue
		}

		var endedAt interface{}
		if ce.EndedAt != "" {
			parsedEndAt, err := time.Parse(time.RFC3339, ce.EndedAt)
			if err == nil {
				endedAt = parsedEndAt
			}
		}

		var sourceUpdatedAt interface{}
		if ce.UpdatedAt != "" {
			parsedUpdatedAt, err := time.Parse(time.RFC3339, ce.UpdatedAt)
			if err == nil {
				sourceUpdatedAt = parsedUpdatedAt
			}
		}

		if _, err := tx.Exec(
			upsertQuery,
			ce.EventID,
			ce.Title,
			ce.Catch,
			ce.Description,
			ce.EventURL,
			startAt,
			endedAt,
			ce.Limit,
			ce.HashTag,
			ce.EventType,
			ce.Accepted,
			ce.Waiting,
			sourceUpdatedAt,
			ce.OwnerID,
			ce.OwnerNickname,
			ce.OwnerDisplayName,
			ce.Place,
			ce.Address,
		); err != nil {
			return fmt.Errorf("upsert mock event failed (event_id=%d): %w", ce.EventID, err)
		}
		seededCount++
	}

	if seededCount == 0 {
		return errors.New("no valid events to seed")
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx failed: %w", err)
	}

	return nil
}

func loadMockEventsFromDB(db *sql.DB, owner string) ([]Event, error) {
	if db == nil {
		return nil, errors.New("db is nil")
	}

	normalizedOwner := strings.ToLower(strings.TrimSpace(owner))
	query := `
		SELECT event_id, title, place, event_url, started_at, ended_at, accepted, waiting, limit_count
		FROM connpass_mock_events
		WHERE is_active = TRUE
		  AND ($1 = '' OR LOWER(owner_nickname) = $1)
		ORDER BY started_at ASC
		LIMIT 100
	`

	rows, err := db.Query(query, normalizedOwner)
	if err != nil {
		return nil, fmt.Errorf("query mock events db failed: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var eventURL sql.NullString
		var endedAt sql.NullTime
		var accepted sql.NullInt64
		var waiting sql.NullInt64
		var limit sql.NullInt64
		if err := rows.Scan(&event.ID, &event.Name, &event.Location, &eventURL, &event.StartAt, &endedAt, &accepted, &waiting, &limit); err != nil {
			return nil, fmt.Errorf("scan mock event db failed: %w", err)
		}
		if eventURL.Valid {
			event.EventURL = eventURL.String
		}
		if endedAt.Valid {
			event.EndAt = endedAt.Time
		} else {
			event.EndAt = event.StartAt.Add(2 * time.Hour)
		}
		if accepted.Valid {
			event.Count = int(accepted.Int64)
			event.Accepted = int(accepted.Int64)
		}
		if waiting.Valid {
			event.Waiting = int(waiting.Int64)
		}
		if limit.Valid {
			event.Limit = int(limit.Int64)
		}
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mock event db failed: %w", err)
	}

	if len(events) == 0 {
		return nil, errors.New("no db mock events")
	}

	return events, nil
}

func convertConnpassEvents(connpassEvents []ConnpassEvent, owner string) ([]Event, error) {
	normalizedOwner := strings.ToLower(strings.TrimSpace(owner))
	events := make([]Event, 0, len(connpassEvents))

	for _, ce := range connpassEvents {
		if normalizedOwner != "" {
			if strings.ToLower(strings.TrimSpace(ce.OwnerNickname)) != normalizedOwner {
				continue
			}
		}

		startAt, err := time.Parse(time.RFC3339, ce.StartedAt)
		if err != nil {
			continue
		}

		endAt, err := time.Parse(time.RFC3339, ce.EndedAt)
		if err != nil {
			endAt = startAt.Add(2 * time.Hour)
		}

		events = append(events, Event{
			ID:       ce.EventID,
			Name:     ce.Title,
			Location: ce.Place,
			EventURL: ce.EventURL,
			StartAt:  startAt,
			EndAt:    endAt,
			Count:    ce.Accepted,
			Accepted: ce.Accepted,
			Waiting:  ce.Waiting,
			Limit:    ce.Limit,
		})
	}

	if len(events) == 0 {
		return nil, errors.New("no connpass events parsed")
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].StartAt.Before(events[j].StartAt)
	})

	return events, nil
}

func loadMockEventsFromFile(owner string) ([]Event, error) {
	filePath := MockFilePath()
	body, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read mock file failed: %w", err)
	}

	var payload ConnpassResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse mock file failed: %w", err)
	}

	return convertConnpassEvents(payload.Events, owner)
}

func buildMockEvents(db *sql.DB, owner string) []Event {
	events, err := loadMockEventsFromDB(db, owner)
	if err == nil {
		return events
	}
	log.Printf("failed to load connpass mock events from db, fallback to file: %v", err)

	events, err = loadMockEventsFromFile(owner)
	if err == nil {
		return events
	}
	log.Printf("failed to load connpass mock file, fallback to generated mock events: %v", err)

	now := time.Now()
	label := "Community"
	if strings.TrimSpace(owner) != "" {
		label = owner
	}

	return []Event{
		{
			ID:       90001,
			Name:     fmt.Sprintf("%s Go Meetup (Mock)", label),
			Location: "東京都渋谷区 渋谷ヒカリエ",
			EventURL: "",
			StartAt:  now.AddDate(0, 0, 7),
			EndAt:    now.AddDate(0, 0, 7).Add(2 * time.Hour),
			Count:    12,
			Accepted: 12,
			Waiting:  1,
			Limit:    20,
		},
		{
			ID:       90002,
			Name:     fmt.Sprintf("%s Flutter LT Night (Mock)", label),
			Location: "大阪府大阪市 グランフロント大阪",
			EventURL: "",
			StartAt:  now.AddDate(0, 0, 14),
			EndAt:    now.AddDate(0, 0, 14).Add(2 * time.Hour),
			Count:    18,
			Accepted: 18,
			Waiting:  2,
			Limit:    30,
		},
		{
			ID:       90003,
			Name:     fmt.Sprintf("%s Hack Session (Mock)", label),
			Location: "オンライン",
			EventURL: "",
			StartAt:  now.AddDate(0, 0, 21),
			EndAt:    now.AddDate(0, 0, 21).Add(3 * time.Hour),
			Count:    24,
			Accepted: 24,
			Waiting:  0,
			Limit:    40,
		},
	}
}

func fetchConnpassEvents(url string) ([]Event, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("connpass status: %d", resp.StatusCode)
	}

	var connpassData ConnpassResponse
	if err := json.NewDecoder(resp.Body).Decode(&connpassData); err != nil {
		return nil, err
	}

	return convertConnpassEvents(connpassData.Events, "")
}

func GetEvents(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		// connpass API申請待ち/障害時でも開発を止めないため、モック返却を可能にする。
		if isConnpassMockEnabled() {
			return c.JSON(http.StatusOK, buildMockEvents(db, ""))
		}

		url := "https://connpass.com/api/v1/event/?keyword=Go&count=5"
		events, err := fetchConnpassEvents(url)
		if err != nil {
			log.Printf("connpass fetch failed, fallback to mock events: %v", err)
			return c.JSON(http.StatusOK, buildMockEvents(db, ""))
		}

		return c.JSON(http.StatusOK, events)
	}
}

func GetUserEvents(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		username := c.Param("username")
		// owner別APIも同様に、外部依存失敗時はユーザー名付きモックへフォールバックする。
		if isConnpassMockEnabled() {
			return c.JSON(http.StatusOK, buildMockEvents(db, username))
		}

		url := "https://connpass.com/api/v1/event/?owner_account=" + username
		events, err := fetchConnpassEvents(url)
		if err != nil {
			log.Printf("connpass user fetch failed (user=%s), fallback to mock events: %v", username, err)
			return c.JSON(http.StatusOK, buildMockEvents(db, username))
		}

		return c.JSON(http.StatusOK, events)
	}
}
