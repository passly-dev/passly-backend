package connpass

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// connpass のイベント情報と、イベント画像のURLを connpass_events テーブルに保存する。
//
// connpass API v2 はイベント画像を返さないので、イベントページの
// <meta property="og:image">（SNSでシェアしたときに出る画像）を読んで、URLだけを保存する。
// 画像そのものはコピーせず、アプリは connpass の画像URLを直接表示する。

const (
	// 画像URLを読み直す間隔（主催者が画像を差し替えたときに追従する）
	imageRefreshInterval = 7 * 24 * time.Hour
	// 画像が取れなかったイベントを読み直す間隔
	imageRetryInterval = 24 * time.Hour
	// 一覧表示のときに裏でページを読む間隔（connpass に負荷をかけないよう1件ずつ）
	imageFetchGap = 2 * time.Second
	// 読み込むページの最大サイズ（og:image は <head> にあるので先頭だけで足りる）
	maxEventPageBytes = 1 << 20
)

// upsertEvents は connpass API で取得したイベントを保存する。画像URLはここでは変えない。
func upsertEvents(db *sql.DB, raw []v2Event) {
	for _, ce := range raw {
		ev, ok := toEvent(ce)
		if !ok {
			continue
		}
		_, err := db.Exec(`
			INSERT INTO connpass_events (
				event_id, title, catch, event_url, place,
				started_at, ended_at, limit_count, accepted, waiting, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, CURRENT_TIMESTAMP)
			ON CONFLICT (event_id) DO UPDATE SET
				title = EXCLUDED.title,
				catch = EXCLUDED.catch,
				event_url = EXCLUDED.event_url,
				place = EXCLUDED.place,
				started_at = EXCLUDED.started_at,
				ended_at = EXCLUDED.ended_at,
				limit_count = EXCLUDED.limit_count,
				accepted = EXCLUDED.accepted,
				waiting = EXCLUDED.waiting,
				updated_at = CURRENT_TIMESTAMP
		`, ev.ID, ev.Name, ce.Catch, ev.EventURL, ev.Location,
			ev.StartAt, ev.EndAt, ev.Limit, ev.Accepted, ev.Waiting)
		if err != nil {
			log.Printf("upsert connpass event failed (event_id=%d): %v", ev.ID, err)
		}
	}
}

// loadStoredEvent は保存済みのイベントを1件返す。connpass が使えないときの予備。
func loadStoredEvent(db *sql.DB, eventID int) (EventDetail, bool) {
	var d EventDetail
	var catch, eventURL, place, imageURL sql.NullString
	var limit, accepted, waiting sql.NullInt64
	err := db.QueryRow(`
		SELECT event_id, title, catch, event_url, place, started_at, ended_at,
		       limit_count, accepted, waiting, image_url
		FROM connpass_events WHERE event_id = $1
	`, eventID).Scan(&d.ID, &d.Name, &catch, &eventURL, &place, &d.StartAt, &d.EndAt,
		&limit, &accepted, &waiting, &imageURL)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("load stored connpass event failed (event_id=%d): %v", eventID, err)
		}
		return EventDetail{}, false
	}
	d.Description = catch.String
	d.EventURL = eventURL.String
	d.Location = place.String
	d.Limit = int(limit.Int64)
	d.Accepted = int(accepted.Int64)
	d.Count = d.Accepted
	d.Waiting = int(waiting.Int64)
	d.ImageURL = imageURL.String
	return d, true
}

// attachImages は保存済みの画像URLをイベントに付ける。
// まだ読んでいない・古いイベントは、裏でページを読みに行く（今回のレスポンスには間に合わない）。
func attachImages(db *sql.DB, events []Event) {
	if len(events) == 0 {
		return
	}
	ids := make([]int64, len(events))
	for i, ev := range events {
		ids[i] = int64(ev.ID)
	}

	rows, err := db.Query(`
		SELECT event_id, COALESCE(image_url, ''), image_checked_at
		FROM connpass_events WHERE event_id = ANY($1)
	`, pq.Array(ids))
	if err != nil {
		log.Printf("query connpass event images failed: %v", err)
		return
	}
	defer rows.Close()

	type stored struct {
		imageURL  string
		checkedAt sql.NullTime
	}
	byID := map[int]stored{}
	for rows.Next() {
		var id int
		var s stored
		if err := rows.Scan(&id, &s.imageURL, &s.checkedAt); err != nil {
			log.Printf("scan connpass event image failed: %v", err)
			return
		}
		byID[id] = s
	}

	for i := range events {
		s, ok := byID[events[i].ID]
		if !ok {
			continue // 保存されていないイベント（モックなど）
		}
		events[i].ImageURL = s.imageURL
		if needsImageCheck(s.imageURL, s.checkedAt) {
			enqueueImageFetch(db, events[i].ID, events[i].EventURL)
		}
	}
}

// attachImageNow は1件分の画像URLを付ける。まだ読んでいなければ、その場でページを読む（詳細画面用）。
func attachImageNow(db *sql.DB, ev *Event) {
	var imageURL string
	var checkedAt sql.NullTime
	err := db.QueryRow(`
		SELECT COALESCE(image_url, ''), image_checked_at FROM connpass_events WHERE event_id = $1
	`, ev.ID).Scan(&imageURL, &checkedAt)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("query connpass event image failed (event_id=%d): %v", ev.ID, err)
		}
		return
	}
	if needsImageCheck(imageURL, checkedAt) {
		imageURL = refreshEventImage(db, ev.ID, ev.EventURL, imageURL)
	}
	ev.ImageURL = imageURL
}

func needsImageCheck(imageURL string, checkedAt sql.NullTime) bool {
	if !checkedAt.Valid {
		return true
	}
	since := time.Since(checkedAt.Time)
	if imageURL == "" {
		return since > imageRetryInterval
	}
	return since > imageRefreshInterval
}

// refreshEventImage はイベントページを読んで画像URLを保存し、使う画像URLを返す。
// 読めなかったときは、前に保存したURL（current）をそのまま使う。
func refreshEventImage(db *sql.DB, eventID int, pageURL, current string) string {
	imageURL, err := fetchOGImage(pageURL)
	if err != nil {
		log.Printf("connpass event image fetch failed (event_id=%d): %v", eventID, err)
		// 失敗しても読んだ時刻は残し、次に読み直すまで間を空ける
		if _, uerr := db.Exec(
			`UPDATE connpass_events SET image_checked_at = CURRENT_TIMESTAMP WHERE event_id = $1`, eventID,
		); uerr != nil {
			log.Printf("update image_checked_at failed (event_id=%d): %v", eventID, uerr)
		}
		return current
	}
	if _, uerr := db.Exec(
		`UPDATE connpass_events SET image_url = $2, image_checked_at = CURRENT_TIMESTAMP WHERE event_id = $1`,
		eventID, imageURL,
	); uerr != nil {
		log.Printf("update image_url failed (event_id=%d): %v", eventID, uerr)
	}
	return imageURL
}

// --- 裏でページを読む処理（1件ずつ、間を空けて） ---

type imageJob struct {
	db      *sql.DB
	eventID int
	pageURL string
}

var (
	imageQueue     chan imageJob
	imageQueueOnce sync.Once
	imagePendingMu sync.Mutex
	imagePending   = map[int]bool{}
)

func enqueueImageFetch(db *sql.DB, eventID int, pageURL string) {
	if pageURL == "" {
		return
	}
	imageQueueOnce.Do(func() {
		imageQueue = make(chan imageJob, 500)
		go imageWorker()
	})

	imagePendingMu.Lock()
	if imagePending[eventID] {
		imagePendingMu.Unlock()
		return
	}
	imagePending[eventID] = true
	imagePendingMu.Unlock()

	select {
	case imageQueue <- imageJob{db: db, eventID: eventID, pageURL: pageURL}:
	default:
		// 待ちがいっぱいなら今回は諦める（次に一覧を表示したときにまた入る）
		imagePendingMu.Lock()
		delete(imagePending, eventID)
		imagePendingMu.Unlock()
	}
}

func imageWorker() {
	for job := range imageQueue {
		refreshEventImage(job.db, job.eventID, job.pageURL, "")
		imagePendingMu.Lock()
		delete(imagePending, job.eventID)
		imagePendingMu.Unlock()
		time.Sleep(imageFetchGap)
	}
}

// --- イベントページから og:image を読む ---

var (
	metaTagRe  = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	propertyRe = regexp.MustCompile(`(?is)\b(?:property|name)\s*=\s*["']([^"']*)["']`)
	contentRe  = regexp.MustCompile(`(?is)\bcontent\s*=\s*["']([^"']*)["']`)
)

// fetchOGImage はイベントページを読み、og:image のURLを返す。og:image がなければ空文字。
func fetchOGImage(pageURL string) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "", fmt.Errorf("invalid event url: %w", err)
	}
	// connpass 以外のURLには行かない（保存されたURLを使って任意のサーバーに通信させないため）
	if u.Scheme != "https" || !isConnpassHost(u.Hostname()) {
		return "", fmt.Errorf("not a connpass url: %s", pageURL)
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Passly/1.0 (event image fetch)")
	req.Header.Set("Accept", "text/html")

	client := &http.Client{
		Timeout: 8 * time.Second,
		// connpass.com/event/ID/ はグループのサブドメインへ 302 で転送される。
		// 転送先も connpass のページだけに限る。
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if r.URL.Scheme != "https" || !isConnpassHost(r.URL.Hostname()) {
				return fmt.Errorf("redirect to non-connpass url: %s", r.URL)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("event page status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEventPageBytes))
	if err != nil {
		return "", err
	}
	return extractOGImage(string(body)), nil
}

func isConnpassHost(host string) bool {
	host = strings.ToLower(host)
	return host == "connpass.com" || strings.HasSuffix(host, ".connpass.com")
}

// extractOGImage は HTML から og:image の https URL を取り出す。なければ空文字。
func extractOGImage(page string) string {
	for _, tag := range metaTagRe.FindAllString(page, -1) {
		p := propertyRe.FindStringSubmatch(tag)
		if p == nil || !strings.EqualFold(strings.TrimSpace(p[1]), "og:image") {
			continue
		}
		c := contentRe.FindStringSubmatch(tag)
		if c == nil {
			continue
		}
		v := strings.TrimSpace(html.UnescapeString(c[1]))
		if strings.HasPrefix(v, "https://") {
			return v
		}
	}
	return ""
}
