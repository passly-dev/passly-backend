package connpass

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const connpassV2EventsURL = "https://connpass.com/api/v2/events/"

// connpass API v2 はプランごとにレート制限がある（フリープランは 12 秒に 1 回・1 日 100 回）。
// 同じ問い合わせはこの間キャッシュを返し、期限切れ後に取得に失敗したときも前回の結果を返す。
const v2CacheTTL = 30 * time.Minute

type v2Response struct {
	Events []v2Event `json:"events"`
}

type v2Event struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Catch     string `json:"catch"`
	URL       string `json:"url"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Limit     int    `json:"limit"`
	Accepted  int    `json:"accepted"`
	Waiting   int    `json:"waiting"`
	Place     string `json:"place"`
}

// EventDetail は GET /events/:id のレスポンス。アプリの EventModel に合わせる。
type EventDetail struct {
	Event
	Description string `json:"description"`
}

type v2CacheEntry struct {
	events    []v2Event
	fetchedAt time.Time
}

var (
	v2CacheMu sync.Mutex
	v2Cache   = map[string]v2CacheEntry{}
	// 一覧で取得済みのイベント。詳細表示のたびに connpass を呼ばないよう ID で引けるようにする。
	v2EventByID = map[int]v2Event{}
)

// NormalizeUsername は connpass_username に保存された値（ユーザー名 / @ユーザー名 /
// プロフィールURL）から connpass のニックネームだけを取り出す。
func NormalizeUsername(raw string) string {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "@")
	if i := strings.Index(v, "connpass.com/user/"); i >= 0 {
		v = v[i+len("connpass.com/user/"):]
		if j := strings.IndexAny(v, "/?#"); j >= 0 {
			v = v[:j]
		}
	}
	return strings.TrimSpace(v)
}

// fetchV2 は connpass API v2 のイベント検索を呼ぶ。同じクエリは v2CacheTTL の間キャッシュを返し、
// 取得に失敗したとき（レート制限の 429 など）は期限切れでも前回の結果を返す。
func fetchV2(apiKey string, q url.Values) ([]v2Event, error) {
	key := q.Encode()

	v2CacheMu.Lock()
	cached, hasCache := v2Cache[key]
	v2CacheMu.Unlock()
	if hasCache && time.Since(cached.fetchedAt) < v2CacheTTL {
		return cached.events, nil
	}

	events, err := requestV2(apiKey, key)
	if err != nil {
		if hasCache {
			log.Printf("connpass fetch failed, serving stale cache (query=%s): %v", key, err)
			return cached.events, nil
		}
		return nil, err
	}

	v2CacheMu.Lock()
	v2Cache[key] = v2CacheEntry{events: events, fetchedAt: time.Now()}
	for _, ce := range events {
		v2EventByID[ce.ID] = ce
	}
	v2CacheMu.Unlock()

	return events, nil
}

func cachedEventByID(id int) (v2Event, bool) {
	v2CacheMu.Lock()
	defer v2CacheMu.Unlock()
	ce, ok := v2EventByID[id]
	return ce, ok
}

func requestV2(apiKey, query string) ([]v2Event, error) {
	req, err := http.NewRequest(http.MethodGet, connpassV2EventsURL+"?"+query, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("connpass status: %d", resp.StatusCode)
	}

	var payload v2Response
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Events, nil
}

func toEvent(ce v2Event) (Event, bool) {
	startAt, err := time.Parse(time.RFC3339, ce.StartedAt)
	if err != nil {
		return Event{}, false
	}
	endAt, err := time.Parse(time.RFC3339, ce.EndedAt)
	if err != nil {
		endAt = startAt.Add(2 * time.Hour)
	}
	return Event{
		ID:       ce.ID,
		Name:     ce.Title,
		Location: ce.Place,
		EventURL: ce.URL,
		StartAt:  startAt.UTC(),
		EndAt:    endAt.UTC(),
		Count:    ce.Accepted,
		Accepted: ce.Accepted,
		Waiting:  ce.Waiting,
		Limit:    ce.Limit,
	}, true
}

func connpassAPIKey() string {
	return strings.TrimSpace(os.Getenv("CONNPASS_API_KEY"))
}

// GetUserParticipatedEvents は users.connpass_username を使って、そのユーザーが
// connpass で参加した・参加予定のイベントを返す。
// ユーザー名が未設定なら空配列。モックモード時はモックを返す。
func GetUserParticipatedEvents(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		var stored sql.NullString
		err := db.QueryRow(`SELECT connpass_username FROM users WHERE id = $1`, c.Param("id")).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
		}
		if err != nil {
			log.Printf("connpass username lookup failed: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}

		nickname := NormalizeUsername(stored.String)
		if nickname == "" {
			return c.JSON(http.StatusOK, []Event{})
		}

		if isConnpassMockEnabled() {
			return c.JSON(http.StatusOK, buildMockEvents(db, nickname))
		}

		apiKey := connpassAPIKey()
		if apiKey == "" {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "CONNPASS_API_KEY is not set"})
		}

		q := url.Values{}
		q.Set("nickname", nickname)
		q.Set("order", "3") // 開催日時の新しい順
		q.Set("count", "100")

		raw, err := fetchV2(apiKey, q)
		if err != nil {
			log.Printf("connpass participant fetch failed (user=%s): %v", nickname, err)
			return c.JSON(http.StatusBadGateway, map[string]string{"error": "failed to fetch connpass events"})
		}

		events := make([]Event, 0, len(raw))
		for _, ce := range raw {
			if ev, ok := toEvent(ce); ok {
				events = append(events, ev)
			}
		}
		return c.JSON(http.StatusOK, events)
	}
}

// GetEventDetail は connpass のイベントIDで 1 件取得する。
// connpass の description は HTML なので、プレーンテキストの catch を説明として返す。
func GetEventDetail(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		eventID, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid event id"})
		}

		if isConnpassMockEnabled() {
			for _, ev := range buildMockEvents(db, "") {
				if ev.ID == eventID {
					return c.JSON(http.StatusOK, EventDetail{Event: ev})
				}
			}
			return c.JSON(http.StatusNotFound, map[string]string{"error": "event not found"})
		}

		// 参加イベント一覧で取得済みなら connpass を呼ばずに返す
		if ce, ok := cachedEventByID(eventID); ok {
			if ev, ok := toEvent(ce); ok {
				return c.JSON(http.StatusOK, EventDetail{Event: ev, Description: ce.Catch})
			}
		}

		apiKey := connpassAPIKey()
		if apiKey == "" {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "CONNPASS_API_KEY is not set"})
		}

		q := url.Values{}
		q.Set("event_id", strconv.Itoa(eventID))

		raw, err := fetchV2(apiKey, q)
		if err != nil {
			log.Printf("connpass event fetch failed (event_id=%d): %v", eventID, err)
			return c.JSON(http.StatusBadGateway, map[string]string{"error": "failed to fetch connpass event"})
		}
		for _, ce := range raw {
			if ev, ok := toEvent(ce); ok && ev.ID == eventID {
				return c.JSON(http.StatusOK, EventDetail{Event: ev, Description: ce.Catch})
			}
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": "event not found"})
	}
}
