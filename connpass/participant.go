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
	ImageURL  string `json:"image_url"`
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

// connpass API v2 を今日（日本時間）何回呼んだか。フリープランは1日100回まで。
// サーバーのメモリにあるので、再起動やスリープで0に戻る（実際の回数はこれ以上のことがある）。
var (
	callCountMu  sync.Mutex
	callCountDay string
	callCount    int
)

func countV2Call(query string) {
	today := time.Now().In(jst).Format("2006-01-02")
	callCountMu.Lock()
	if callCountDay != today {
		callCountDay = today
		callCount = 0
	}
	callCount++
	n := callCount
	callCountMu.Unlock()
	log.Printf("connpass api v2 call #%d today (%s JST) query=%s", n, today, query)
}

func requestV2(apiKey, query string) ([]v2Event, error) {
	countV2Call(query)
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
		ImageURL: ce.ImageURL,
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
		userID := c.Param("id")

		// userEventsTTL 以内に取得済みなら、connpass を呼ばずに保存したものを返す。
		// （モックモードはモックの変更をすぐ見られるよう、毎回モックを作る）
		if !isConnpassMockEnabled() {
			if events, fresh, err := loadUserEvents(db, userID); err != nil {
				log.Printf("load stored connpass events failed (user_id=%s): %v", userID, err)
			} else if fresh {
				return c.JSON(http.StatusOK, events)
			}
		}

		events, err := fetchParticipatedEvents(db, nickname)
		if err != nil {
			log.Printf("connpass participant fetch failed (user=%s): %v", nickname, err)
			// 取れなかったときは、古くても保存したものを返す
			if stored, _, lerr := loadUserEvents(db, userID); lerr == nil && len(stored) > 0 {
				return c.JSON(http.StatusOK, stored)
			}
			if errors.Is(err, errNoAPIKey) {
				return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "CONNPASS_API_KEY is not set"})
			}
			return c.JSON(http.StatusBadGateway, map[string]string{"error": "failed to fetch connpass events"})
		}

		// 取得できたら保存しておく（次の表示と、すれ違いの判定で使う）
		if err := saveUserEvents(db, userID, events); err != nil {
			log.Printf("save connpass events failed (user_id=%s): %v", userID, err)
		}
		return c.JSON(http.StatusOK, events)
	}
}

// loadUserEvents はユーザーの保存済みの参加イベントを、開催日時の新しい順に返す。
// fresh は userEventsTTL 以内に connpass から取得したものかどうか。
func loadUserEvents(db *sql.DB, userID string) (events []Event, fresh bool, err error) {
	var syncedAt sql.NullTime
	if err := db.QueryRow(`SELECT connpass_synced_at FROM users WHERE id = $1`, userID).Scan(&syncedAt); err != nil {
		return nil, false, err
	}
	fresh = syncedAt.Valid && time.Since(syncedAt.Time) < userEventsTTL

	rows, err := db.Query(`
		SELECT u.event_id, u.title, u.started_at, u.ended_at,
		       COALESCE(c.place, ''), COALESCE(c.event_url, ''), COALESCE(c.image_url, ''),
		       COALESCE(c.accepted, 0), COALESCE(c.waiting, 0), COALESCE(c.limit_count, 0)
		FROM user_connpass_events u
		LEFT JOIN connpass_events c ON c.event_id = u.event_id
		WHERE u.user_id = $1
		ORDER BY u.started_at DESC
	`, userID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	events = make([]Event, 0)
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.Name, &ev.StartAt, &ev.EndAt,
			&ev.Location, &ev.EventURL, &ev.ImageURL, &ev.Accepted, &ev.Waiting, &ev.Limit); err != nil {
			return nil, false, err
		}
		ev.StartAt = ev.StartAt.UTC()
		ev.EndAt = ev.EndAt.UTC()
		ev.Count = ev.Accepted
		events = append(events, ev)
	}
	return events, fresh, rows.Err()
}

var errNoAPIKey = errors.New("CONNPASS_API_KEY is not set")

// fetchParticipatedEvents は connpass のニックネームで、参加した・参加予定のイベントを取得する。
// モックモード時はモックを返す。
func fetchParticipatedEvents(db *sql.DB, nickname string) ([]Event, error) {
	if isConnpassMockEnabled() {
		return buildMockEvents(db, nickname), nil
	}

	apiKey := connpassAPIKey()
	if apiKey == "" {
		return nil, errNoAPIKey
	}

	q := url.Values{}
	q.Set("nickname", nickname)
	q.Set("order", "3") // 開催日時の新しい順
	q.Set("count", "100")

	raw, err := fetchV2(apiKey, q)
	if err != nil {
		return nil, err
	}
	upsertEvents(db, raw)

	events := make([]Event, 0, len(raw))
	for _, ce := range raw {
		if ev, ok := toEvent(ce); ok {
			events = append(events, ev)
		}
	}
	return events, nil
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

		// storedEventTTL 以内に保存したものがあれば、connpass を呼ばずに返す
		stored, fresh, hasStored := loadStoredEvent(db, eventID)
		if hasStored && fresh {
			return c.JSON(http.StatusOK, stored)
		}

		apiKey := connpassAPIKey()
		if apiKey == "" {
			if hasStored {
				return c.JSON(http.StatusOK, stored)
			}
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "CONNPASS_API_KEY is not set"})
		}

		q := url.Values{}
		q.Set("event_id", strconv.Itoa(eventID))

		raw, err := fetchV2(apiKey, q)
		if err != nil {
			log.Printf("connpass event fetch failed (event_id=%d): %v", eventID, err)
			// レート制限などで取れないときは、古くても保存したイベントを返す
			if hasStored {
				return c.JSON(http.StatusOK, stored)
			}
			return c.JSON(http.StatusBadGateway, map[string]string{"error": "failed to fetch connpass event"})
		}
		upsertEvents(db, raw)
		for _, ce := range raw {
			if ev, ok := toEvent(ce); ok && ev.ID == eventID {
				return c.JSON(http.StatusOK, EventDetail{Event: ev, Description: ce.Catch})
			}
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": "event not found"})
	}
}
