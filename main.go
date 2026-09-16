package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// --- 構造体定義 (Models) ---

type User struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Email            string `json:"email"`
	Password         string `json:"password,omitempty"`
	IconURL          string `json:"icon_url"`
	OneWord          string `json:"one_word"`
	Role             string `json:"role"`
	TechStack        string `json:"tech_stack"`
	TwitterURL       string `json:"twitter_url"`
	GitHubURL        string `json:"github_url"`
	PortfolioURL     string `json:"portfolio_url"`
	Affiliation      string `json:"affiliation"`
	ConnpassUsername string `json:"connpass_username"`
}

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
}

type Encounter struct {
	ID          int    `json:"id"`
	MyID        string `json:"my_id"`
	TargetID    string `json:"target_id"`
	EventID     *int   `json:"event_id"`
	IsConfirmed bool   `json:"is_confirmed"`
	CreatedAt   string `json:"created_at"`
}

func respondError(c echo.Context, status int, message string) error {
	return c.JSON(status, map[string]string{"error": message})
}

func getFileExt(filename string) string {
	parts := strings.Split(filename, ".")
	if len(parts) > 1 {
		return "." + parts[len(parts)-1]
	}
	return ".jpg"
}

func generateEphemeralToken() (string, error) {
	// 8byteを16桁hex化。BLE広告名(SP_ + token)でも20文字以内に収まる。
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func getEncounterCooldownSeconds() int {
	// ハッカソン検証中は短めにして、実機テストで件数変化を確認しやすくする
	const defaultSeconds = 10
	raw := strings.TrimSpace(os.Getenv("ENCOUNTER_COOLDOWN_SECONDS"))
	if raw == "" {
		return defaultSeconds
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return defaultSeconds
	}
	return parsed
}

func resolveUserIDFromToken(db *sql.DB, token string) (string, error) {
	if token == "" {
		return "", errors.New("token is required")
	}

	// 新方式: DB保存済みの短いトークンを優先解決
	var resolvedUserID string
	query := `
		SELECT user_id
		FROM ephemeral_tokens
		WHERE token = $1 AND expires_at > CURRENT_TIMESTAMP
		ORDER BY created_at DESC
		LIMIT 1
	`
	err := db.QueryRow(query, token).Scan(&resolvedUserID)
	if err == nil {
		return resolvedUserID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("failed to resolve token from db: %w", err)
	}

	return "", errors.New("invalid or expired token")
}

func isConnpassMockEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("CONNPASS_MOCK_MODE")))
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func getConnpassMockFilePath() string {
	path := strings.TrimSpace(os.Getenv("CONNPASS_MOCK_FILE"))
	if path == "" {
		return "mock/connpass_events.json"
	}
	return path
}

func shouldSeedConnpassMockOnStartup() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("CONNPASS_MOCK_SEED_ON_STARTUP")))
	if raw == "" {
		return true
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func seedConnpassMockEventsFromFile(db *sql.DB, filePath string) error {
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
	filePath := getConnpassMockFilePath()
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

func GetConnpassEvents(db *sql.DB) echo.HandlerFunc {
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

func GetUserConnpassEvents(db *sql.DB) echo.HandlerFunc {
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

func main() {
	godotenv.Load()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "host=localhost port=5432 user=user password=pass dbname=passly sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	firebaseServiceAccount := os.Getenv("FIREBASE_SERVICE_ACCOUNT_PATH")
	firebaseServiceAccountJSON := os.Getenv("FIREBASE_SERVICE_ACCOUNT_JSON")
	firebaseBucket := os.Getenv("FIREBASE_STORAGE_BUCKET")
	if firebaseServiceAccount == "" && firebaseServiceAccountJSON == "" {
		log.Println("FIREBASE_SERVICE_ACCOUNT_PATH/FIREBASE_SERVICE_ACCOUNT_JSON not set - avatar upload disabled")
	} else if err := InitFirebase(ctx, firebaseServiceAccount, firebaseBucket); err != nil {
		log.Printf("Firebase init error: %v (continuing without upload)\n", err)
	} else {
		log.Println("Firebase Storage initialized")
	}

	initDB(db)
	if shouldSeedConnpassMockOnStartup() {
		if err := seedConnpassMockEventsFromFile(db, getConnpassMockFilePath()); err != nil {
			log.Printf("connpass mock seed skipped: %v", err)
		} else {
			log.Printf("connpass mock seed completed from %s", getConnpassMockFilePath())
		}
	}
	encounterCooldownSeconds := getEncounterCooldownSeconds()
	log.Printf("encounter cooldown seconds = %d", encounterCooldownSeconds)

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	// --- エフェメラルトークンAPI ---
	// BLE広告名20文字制限に合わせた短いトークンを返す。
	e.GET("/users/:id/ephemeral-token", func(c echo.Context) error {
		userID := c.Param("id")
		token, err := generateEphemeralToken()
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "failed to generate token")
		}

		expiresAtTime := time.Now().Add(1 * time.Hour).UTC()
		expiresAt := expiresAtTime.Format(time.RFC3339)

		upsertQuery := `
			INSERT INTO ephemeral_tokens (token, user_id, expires_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (token)
			DO UPDATE SET user_id = EXCLUDED.user_id, expires_at = EXCLUDED.expires_at, created_at = CURRENT_TIMESTAMP
		`
		if _, err := db.Exec(upsertQuery, token, userID, expiresAtTime); err != nil {
			return respondError(c, http.StatusInternalServerError, "failed to store token")
		}

		return c.JSON(http.StatusOK, map[string]string{
			"token":      token,
			"expires_in": "3600",
			"expires_at": expiresAt,
		})
	})

	e.POST("/encounters/resolve", func(c echo.Context) error {
		var req struct {
			Token       string `json:"token"`
			EphemeralID string `json:"ephemeral_id"`
		}
		if err := c.Bind(&req); err != nil {
			return respondError(c, http.StatusBadRequest, "Invalid request")
		}
		token := req.Token
		if token == "" {
			token = req.EphemeralID
		}
		if token == "" {
			return respondError(c, http.StatusBadRequest, "token or ephemeral_id is required")
		}

		resolvedUserID, err := resolveUserIDFromToken(db, token)
		if err != nil {
			log.Printf("resolve failed token=%s err=%v", token, err)
			return respondError(c, http.StatusBadRequest, "Invalid token")
		}
		log.Printf("resolve success token=%s user_id=%s", token, resolvedUserID)

		return c.JSON(http.StatusOK, map[string]string{
			"user_id": resolvedUserID,
		})
	})

	// --- イベント取得API ---
	e.GET("/events", GetConnpassEvents(db))
	e.GET("/users/:username/events", GetUserConnpassEvents(db))

	e.POST("/signup", func(c echo.Context) error {
		var u User
		if err := c.Bind(&u); err != nil {
			return respondError(c, http.StatusBadRequest, "Invalid request format")
		}
		if u.ID == "" || u.Name == "" || u.Email == "" || u.Password == "" {
			return respondError(c, http.StatusBadRequest, "id, name, email, and password are required")
		}

		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to hash password")
		}

		query := `
		INSERT INTO users (id, name, email, password_hash, icon_url, one_word, role, tech_stack, twitter_url, github_url, portfolio_url, affiliation, connpass_username)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

		_, err = db.Exec(query, u.ID, u.Name, u.Email, string(hashedPassword), u.IconURL, u.OneWord, u.Role, u.TechStack, u.TwitterURL, u.GitHubURL, u.PortfolioURL, u.Affiliation, u.ConnpassUsername)
		if err != nil {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				return respondError(c, http.StatusConflict, "User ID or Email already exists")
			}
			return respondError(c, http.StatusInternalServerError, "Failed to insert user")
		}

		return c.JSON(http.StatusCreated, map[string]string{"message": "User registered successfully"})
	})

	e.POST("/login", func(c echo.Context) error {
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := c.Bind(&req); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
		}

		var storedHash, userID string
		query := `SELECT password_hash, id FROM users WHERE email = $1`
		err := db.QueryRow(query, req.Email).Scan(&storedHash, &userID)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "User not found"})
		}

		err = bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.Password))
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Invalid password"})
		}

		return c.JSON(http.StatusOK, map[string]string{
			"message": "Login successful",
			"user_id": userID,
		})
	})

	e.GET("/plaza/count", func(c echo.Context) error {
		var totalUsers int
		if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&totalUsers); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count users")
		}

		var todayEncounters int
		if err := db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE DATE(created_at) = CURRENT_DATE`).Scan(&todayEncounters); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count today encounters")
		}

		const dailyLimit = 5
		return c.JSON(http.StatusOK, map[string]interface{}{
			"count":      totalUsers,
			"todayCount": todayEncounters,
			"dailyLimit": dailyLimit,
		})
	})

	e.GET("/users/:id/home", func(c echo.Context) error {
		userID := c.Param("id")

		type HomeData struct {
			TotalEncounters int                      `json:"total_encounters"`
			TodayEncounters int                      `json:"today_encounters"`
			Unconfirmed     []map[string]interface{} `json:"unconfirmed"`
			RandomThree     []map[string]interface{} `json:"random_three"`
		}
		var res HomeData

		if err := db.QueryRow(`
			SELECT COUNT(DISTINCT CASE WHEN my_id = $1 THEN target_id ELSE my_id END)
			FROM encounters
			WHERE my_id = $1 OR target_id = $1
		`, userID).Scan(&res.TotalEncounters); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count total encounters")
		}
		if err := db.QueryRow(`
			SELECT COUNT(DISTINCT CASE WHEN my_id = $1 THEN target_id ELSE my_id END)
			FROM encounters
			WHERE (my_id = $1 OR target_id = $1)
			  AND DATE(created_at) = CURRENT_DATE
		`, userID).Scan(&res.TodayEncounters); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count today encounters")
		}

		rows, err := db.Query(`
            SELECT u.id, u.name, u.icon_url, u.one_word
            FROM encounters e
            JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
            WHERE (e.my_id = $1 OR e.target_id = $1)
              AND e.is_confirmed = FALSE`, userID)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query unconfirmed encounters")
		}
		defer rows.Close()

		for rows.Next() {
			var id, name, icon, word string
			if err := rows.Scan(&id, &name, &icon, &word); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to scan unconfirmed encounter record")
			}
			res.Unconfirmed = append(res.Unconfirmed, map[string]interface{}{
				"id": id, "name": name, "icon_url": icon, "one_word": word,
			})
		}

		loadRandomThree := func(query string, args ...interface{}) ([]map[string]interface{}, error) {
			randRows, err := db.Query(query, args...)
			if err != nil {
				return nil, err
			}
			defer randRows.Close()

			result := make([]map[string]interface{}, 0, 3)
			for randRows.Next() {
				var id, name, icon, word string
				if err := randRows.Scan(&id, &name, &icon, &word); err != nil {
					return nil, err
				}
				result = append(result, map[string]interface{}{
					"id": id, "name": name, "icon_url": icon, "one_word": word,
				})
			}
			if err := randRows.Err(); err != nil {
				return nil, err
			}
			return result, nil
		}

		res.RandomThree, err = loadRandomThree(`
            SELECT u.id, u.name, u.icon_url, u.one_word
						FROM encounters e
						JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
						WHERE (e.my_id = $1 OR e.target_id = $1)
							AND DATE(e.created_at) = CURRENT_DATE
            ORDER BY RANDOM()
            LIMIT 3`, userID)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query random encounters")
		}

		if len(res.RandomThree) == 0 {
			res.RandomThree, err = loadRandomThree(`
                SELECT id, name, icon_url, one_word
                FROM (
                    SELECT DISTINCT ON (u.id)
                        u.id,
                        u.name,
                        COALESCE(u.icon_url, '') AS icon_url,
                        COALESCE(u.one_word, '') AS one_word,
                        e.created_at
                    FROM encounters e
					JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
					WHERE e.my_id = $1 OR e.target_id = $1
                    ORDER BY u.id, e.created_at DESC
                ) latest
                ORDER BY created_at DESC
                LIMIT 3`, userID)
			if err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to query fallback random encounters")
			}
		}

		return c.JSON(http.StatusOK, res)
	})

	e.GET("/users/:id/achievements", func(c echo.Context) error {
		userID := c.Param("id")

		type dayCount struct {
			Date  time.Time
			Count int
		}

		type achievementItem struct {
			ID          string  `json:"id"`
			Emoji       string  `json:"emoji"`
			Title       string  `json:"title"`
			Description string  `json:"description"`
			IsUnlocked  bool    `json:"is_unlocked"`
			UnlockedAt  *string `json:"unlocked_at"`
		}

		type achievementResponse struct {
			UnlockedCount int               `json:"unlocked_count"`
			TotalCount    int               `json:"total_count"`
			Achievements  []achievementItem `json:"achievements"`
		}

		ptrDate := func(t time.Time) *string {
			s := t.Format("2006-01-02")
			return &s
		}

		parseISODate := func(s string) (time.Time, error) {
			return time.Parse("2006-01-02", s)
		}

		dailyRows, err := db.Query(`
			SELECT DATE(created_at) AS d,
			       COUNT(DISTINCT CASE WHEN my_id = $1 THEN target_id ELSE my_id END) AS cnt
			FROM encounters
			WHERE my_id = $1 OR target_id = $1
			GROUP BY DATE(created_at)
			ORDER BY d ASC
		`, userID)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query daily encounter stats")
		}
		defer dailyRows.Close()

		dailyCounts := make([]dayCount, 0)
		for dailyRows.Next() {
			var d time.Time
			var cnt int
			if err := dailyRows.Scan(&d, &cnt); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to read daily encounter stats")
			}
			dailyCounts = append(dailyCounts, dayCount{Date: d, Count: cnt})
		}
		if err := dailyRows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to iterate daily encounter stats")
		}

		firstDateByThreshold := func(threshold int) *string {
			if threshold <= 0 {
				return nil
			}
			cumulative := 0
			for _, day := range dailyCounts {
				cumulative += day.Count
				if cumulative >= threshold {
					return ptrDate(day.Date)
				}
			}
			return nil
		}

		firstDateByDailyThreshold := func(threshold int) *string {
			for _, day := range dailyCounts {
				if day.Count >= threshold {
					return ptrDate(day.Date)
				}
			}
			return nil
		}

		firstDateByStreak := func(targetStreak int) *string {
			if targetStreak <= 0 || len(dailyCounts) == 0 {
				return nil
			}

			streak := 0
			var prev time.Time
			for _, day := range dailyCounts {
				if streak == 0 {
					streak = 1
				} else {
					diffDays := int(day.Date.Sub(prev).Hours() / 24)
					if diffDays == 1 {
						streak++
					} else {
						streak = 1
					}
				}

				if streak >= targetStreak {
					return ptrDate(day.Date)
				}
				prev = day.Date
			}

			return nil
		}

		roleRows, err := db.Query(`
			SELECT role, MIN(DATE(e.created_at))
			FROM encounters e
			JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
			WHERE (e.my_id = $1 OR e.target_id = $1)
			  AND u.role IN ('frontend', 'backend', 'fullstack')
			GROUP BY role
		`, userID)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query role encounters")
		}
		defer roleRows.Close()

		roleFirstDate := map[string]time.Time{}
		for roleRows.Next() {
			var role string
			var dateStr string
			if err := roleRows.Scan(&role, &dateStr); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to read role encounters")
			}
			d, err := parseISODate(dateStr)
			if err != nil {
				continue
			}
			roleFirstDate[role] = d
		}
		if err := roleRows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to iterate role encounters")
		}

		allRolesDate := func() *string {
			required := []string{"frontend", "backend", "fullstack"}
			latest := time.Time{}
			for _, role := range required {
				d, ok := roleFirstDate[role]
				if !ok {
					return nil
				}
				if d.After(latest) {
					latest = d
				}
			}
			if latest.IsZero() {
				return nil
			}
			return ptrDate(latest)
		}

		var firstEventDateStr sql.NullString
		if err := db.QueryRow(`
			SELECT TO_CHAR(MIN(DATE(created_at)), 'YYYY-MM-DD')
			FROM encounters
			WHERE (my_id = $1 OR target_id = $1)
			  AND event_id IS NOT NULL
		`, userID).Scan(&firstEventDateStr); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query event encounter date")
		}

		var eventDate *string
		if firstEventDateStr.Valid && firstEventDateStr.String != "" {
			s := firstEventDateStr.String
			eventDate = &s
		}

		achievements := []achievementItem{
			{
				ID:          "first_encounter",
				Emoji:       "🎉",
				Title:       "はじめてのすれ違い",
				Description: "初めて誰かとすれ違いました！",
				UnlockedAt:  firstDateByThreshold(1),
			},
			{
				ID:          "encounter_10",
				Emoji:       "👥",
				Title:       "10人達成",
				Description: "累計10人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(10),
			},
			{
				ID:          "encounter_50",
				Emoji:       "⭐",
				Title:       "50人達成",
				Description: "累計50人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(50),
			},
			{
				ID:          "encounter_100",
				Emoji:       "💯",
				Title:       "100人達成",
				Description: "累計100人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(100),
			},
			{
				ID:          "all_roles",
				Emoji:       "🎨",
				Title:       "オールロール",
				Description: "全ロールの人とすれ違いました！",
				UnlockedAt:  allRolesDate(),
			},
			{
				ID:          "daily_5",
				Emoji:       "⚡",
				Title:       "1日5人",
				Description: "1日で5人とすれ違いました！",
				UnlockedAt:  firstDateByDailyThreshold(5),
			},
			{
				ID:          "streak_3",
				Emoji:       "🔥",
				Title:       "3日連続",
				Description: "3日連続ですれ違いました！",
				UnlockedAt:  firstDateByStreak(3),
			},
			{
				ID:          "streak_7",
				Emoji:       "🏆",
				Title:       "7日連続",
				Description: "7日連続ですれ違いました！",
				UnlockedAt:  firstDateByStreak(7),
			},
			{
				ID:          "event_encounter",
				Emoji:       "🌟",
				Title:       "イベント参加",
				Description: "イベントに参加してすれ違いました！",
				UnlockedAt:  eventDate,
			},
			{
				ID:          "encounter_200",
				Emoji:       "🚀",
				Title:       "200人達成",
				Description: "累計200人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(200),
			},
		}

		unlockedCount := 0
		for i := range achievements {
			achievements[i].IsUnlocked = achievements[i].UnlockedAt != nil
			if achievements[i].IsUnlocked {
				unlockedCount++
			}
		}

		return c.JSON(http.StatusOK, achievementResponse{
			UnlockedCount: unlockedCount,
			TotalCount:    len(achievements),
			Achievements:  achievements,
		})
	})

	e.GET("/users/:id/encounters", func(c echo.Context) error {
		userID := c.Param("id")

		rows, err := db.Query(`
			SELECT id, name, icon_url, one_word, created_at, is_confirmed
			FROM (
				SELECT DISTINCT ON (u.id)
					u.id,
					u.name,
					COALESCE(u.icon_url, '') AS icon_url,
					COALESCE(u.one_word, '') AS one_word,
					e.created_at,
					e.is_confirmed
				FROM encounters e
				JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
				WHERE e.my_id = $1 OR e.target_id = $1
				ORDER BY u.id, e.created_at DESC
			) latest
			ORDER BY RANDOM()
			LIMIT 10
		`, userID)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query encounters")
		}
		defer rows.Close()

		type PlazaEncounter struct {
			ID          string    `json:"id"`
			Name        string    `json:"name"`
			IconURL     string    `json:"icon_url"`
			OneWord     string    `json:"one_word"`
			CreatedAt   time.Time `json:"created_at"`
			IsConfirmed bool      `json:"is_confirmed"`
		}

		encounters := make([]PlazaEncounter, 0)
		for rows.Next() {
			var item PlazaEncounter
			if err := rows.Scan(&item.ID, &item.Name, &item.IconURL, &item.OneWord, &item.CreatedAt, &item.IsConfirmed); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to scan encounter record")
			}
			encounters = append(encounters, item)
		}

		if err := rows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to iterate encounters")
		}

		return c.JSON(http.StatusOK, encounters)
	})

	e.PUT("/users/:id/encounters/confirm", func(c echo.Context) error {
		userID := c.Param("id")
		query := `UPDATE encounters SET is_confirmed = TRUE WHERE (my_id = $1 OR target_id = $1) AND is_confirmed = FALSE`
		_, err := db.Exec(query, userID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to update confirmation status"})
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Encounters marked as confirmed"})
	})

	// 親しい友達（close friends）
	// フロント: lib/features/close_friend/data/close_friend_repository_api.dart が前提とするAPI。
	e.POST("/users/:id/close-friends", func(c echo.Context) error {
		userID := c.Param("id")

		var body struct {
			TargetID string `json:"target_id"`
		}
		if err := c.Bind(&body); err != nil || body.TargetID == "" {
			return respondError(c, http.StatusBadRequest, "target_id is required")
		}
		if body.TargetID == userID {
			return respondError(c, http.StatusBadRequest, "cannot add yourself as a close friend")
		}

		var targetExists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, body.TargetID).Scan(&targetExists); err != nil || !targetExists {
			return respondError(c, http.StatusNotFound, "target user not found")
		}

		_, err := db.Exec(
			`INSERT INTO close_friends (user_id, friend_id) VALUES ($1, $2) ON CONFLICT (user_id, friend_id) DO NOTHING`,
			userID, body.TargetID,
		)
		if err != nil {
			log.Printf("Failed to add close friend: %v", err)
			return respondError(c, http.StatusInternalServerError, "Failed to add close friend")
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Close friend added"})
	})

	e.DELETE("/users/:id/close-friends/:target_id", func(c echo.Context) error {
		userID := c.Param("id")
		targetID := c.Param("target_id")

		_, err := db.Exec(`DELETE FROM close_friends WHERE user_id = $1 AND friend_id = $2`, userID, targetID)
		if err != nil {
			log.Printf("Failed to remove close friend: %v", err)
			return respondError(c, http.StatusInternalServerError, "Failed to remove close friend")
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Close friend removed"})
	})

	e.GET("/users/:id/close-friends", func(c echo.Context) error {
		userID := c.Param("id")

		rows, err := db.Query(`
			SELECT
				u.id,
				u.name,
				COALESCE(u.icon_url, '') AS icon_url,
				COALESCE(u.one_word, '') AS one_word,
				COALESCE(u.role, 'other') AS role,
				COALESCE(u.tech_stack, '') AS tech_stack,
				le.met_at,
				COALESCE(ev.name, '') AS event_name
			FROM close_friends cf
			JOIN users u ON u.id = cf.friend_id
			LEFT JOIN LATERAL (
				SELECT e.created_at AS met_at, e.event_id
				FROM encounters e
				WHERE (e.my_id = cf.user_id AND e.target_id = cf.friend_id)
				   OR (e.my_id = cf.friend_id AND e.target_id = cf.user_id)
				ORDER BY e.created_at DESC
				LIMIT 1
			) le ON true
			LEFT JOIN events ev ON ev.id = le.event_id
			WHERE cf.user_id = $1
			ORDER BY cf.created_at DESC
		`, userID)
		if err != nil {
			log.Printf("Failed to query close friends: %v", err)
			return respondError(c, http.StatusInternalServerError, "Failed to query close friends")
		}
		defer rows.Close()

		type LastEncounterJSON struct {
			MetAt     time.Time `json:"met_at"`
			EventName string    `json:"event_name"`
		}
		type CloseFriendUser struct {
			ID            string             `json:"id"`
			Name          string             `json:"name"`
			IconURL       string             `json:"icon_url"`
			OneWord       string             `json:"one_word"`
			Role          string             `json:"role"`
			TechStack     string             `json:"tech_stack"`
			IsCloseFriend bool               `json:"is_close_friend"`
			LastEncounter *LastEncounterJSON `json:"last_encounter"`
		}

		friends := make([]CloseFriendUser, 0)
		for rows.Next() {
			var item CloseFriendUser
			var metAt sql.NullTime
			var eventName string
			if err := rows.Scan(&item.ID, &item.Name, &item.IconURL, &item.OneWord, &item.Role, &item.TechStack, &metAt, &eventName); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to scan close friend record")
			}
			item.IsCloseFriend = true
			if metAt.Valid {
				item.LastEncounter = &LastEncounterJSON{MetAt: metAt.Time, EventName: eventName}
			}
			friends = append(friends, item)
		}
		if err := rows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to iterate close friends")
		}

		return c.JSON(http.StatusOK, friends)
	})

	// 1. 【復旧】正しい daily API (1日分のデータを返す)
	// 理由: フロントエンドが現在このAPIを使ってカレンダーを描画しているため、まずは正しく動く状態に戻す。
	e.GET("/users/:id/calendar/daily", func(c echo.Context) error {
		userID := c.Param("id")
		dateStr := c.QueryParam("date")
		if dateStr == "" {
			dateStr = time.Now().Format("2006-01-02")
		}

		type EncounterUser struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			IconURL string `json:"icon_url"`
			OneWord string `json:"one_word"`
		}

		type DailyData struct {
			EncounterCount int             `json:"encounter_count"`
			EventNames     []string        `json:"event_names"`
			EncounterUsers []EncounterUser `json:"encounter_users"`
		}
		var res DailyData

		countQuery := `SELECT COUNT(*) FROM encounters WHERE (my_id = $1 OR target_id = $1) AND DATE(created_at) = $2`
		if err := db.QueryRow(countQuery, userID, dateStr).Scan(&res.EncounterCount); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count encounters")
		}

		eventQuery := `SELECT name FROM events WHERE DATE(start_at) = $1`
		rows, err := db.Query(eventQuery, dateStr)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query events")
		}
		defer rows.Close()

		for rows.Next() {
			var eventName string
			if err := rows.Scan(&eventName); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to read event rows")
			}
			res.EventNames = append(res.EventNames, eventName)
		}

		usersQuery := `
			SELECT id, name, icon_url, one_word
			FROM (
				SELECT DISTINCT ON (u.id)
					u.id,
					u.name,
					COALESCE(u.icon_url, '') AS icon_url,
					COALESCE(u.one_word, '') AS one_word,
					e.created_at
				FROM encounters e
				JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
				WHERE (e.my_id = $1 OR e.target_id = $1)
					AND DATE(e.created_at) = $2
				ORDER BY u.id, e.created_at DESC
			) latest
			ORDER BY created_at DESC
		`

		userRows, err := db.Query(usersQuery, userID, dateStr)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query encounter users")
		}
		defer userRows.Close()

		for userRows.Next() {
			var user EncounterUser
			if err := userRows.Scan(&user.ID, &user.Name, &user.IconURL, &user.OneWord); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to read encounter users")
			}
			res.EncounterUsers = append(res.EncounterUsers, user)
		}

		if err := userRows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to iterate encounter users")
		}

		return c.JSON(http.StatusOK, res)
	})

	// 2. 【新規】正しい monthly API (1ヶ月分の人数を一括で返す)
	// 理由: フロントエンドが「1ヶ月分のAPI」に切り替えて、カレンダーの描画を爆速化（サーバー負荷軽減）できるようにするため。タイポ(wncountters)も修正済み。
	e.GET("/users/:id/calendar/monthly", func(c echo.Context) error {
		userID := c.Param("id")
		yearMonth := c.QueryParam("month")
		if yearMonth == "" {
			yearMonth = time.Now().Format("2006-01")
		}

		query := `
            SELECT DATE(created_at) as date, COUNT(*) as count
            FROM encounters
			WHERE (my_id = $1 OR target_id = $1) AND TO_CHAR(created_at, 'YYYY-MM') = $2
            GROUP BY DATE(created_at)
        `
		rows, err := db.Query(query, userID, yearMonth)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to fetch monthly data"})
		}
		defer rows.Close()

		monthlyData := make(map[string]int)
		for rows.Next() {
			var date time.Time
			var count int
			if err := rows.Scan(&date, &count); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to scan row"})
			}
			monthlyData[date.Format("2006-01-02")] = count
		}

		return c.JSON(http.StatusOK, monthlyData)
	})

	e.GET("/users/:id", func(c echo.Context) error {
		id := c.Param("id")
		var u User
		query := `SELECT
			id,
			name,
			COALESCE(icon_url, ''),
			COALESCE(one_word, ''),
			COALESCE(role, 'other'),
			COALESCE(tech_stack, ''),
			COALESCE(twitter_url, ''),
			COALESCE(github_url, ''),
			COALESCE(portfolio_url, ''),
			COALESCE(affiliation, ''),
			COALESCE(connpass_username, '')
		FROM users
		WHERE id = $1`
		err := db.QueryRow(query, id).Scan(&u.ID, &u.Name, &u.IconURL, &u.OneWord, &u.Role, &u.TechStack, &u.TwitterURL, &u.GitHubURL, &u.PortfolioURL, &u.Affiliation, &u.ConnpassUsername)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return c.JSON(http.StatusNotFound, map[string]string{"error": "User not found"})
			}
			log.Printf("GET /users/:id query error (id=%s): %v", id, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database error"})
		}
		return c.JSON(http.StatusOK, u)
	})

	e.PATCH("/users/:id", func(c echo.Context) error {
		userID := c.Param("id")

		var exists bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if err != nil || !exists {
			return respondError(c, http.StatusNotFound, "User not found")
		}

		var updateData map[string]interface{}
		if err := c.Bind(&updateData); err != nil {
			return respondError(c, http.StatusBadRequest, "Invalid request format")
		}

		var updateFields []string
		var args []interface{}
		argIndex := 1

		if name, ok := updateData["name"].(string); ok {
			updateFields = append(updateFields, fmt.Sprintf("name = $%d", argIndex))
			args = append(args, name)
			argIndex++
		}

		optionalFields := map[string]string{
			"one_word":          "one_word",
			"tech_stack":        "tech_stack",
			"twitter_url":       "twitter_url",
			"github_url":        "github_url",
			"portfolio_url":     "portfolio_url",
			"affiliation":       "affiliation",
			"connpass_username": "connpass_username",
		}

		for jsonKey, dbCol := range optionalFields {
			if value, ok := updateData[jsonKey].(string); ok {
				updateFields = append(updateFields, fmt.Sprintf("%s = $%d", dbCol, argIndex))
				args = append(args, value)
				argIndex++
			}
		}

		if len(updateFields) == 0 {
			return respondError(c, http.StatusBadRequest, "No fields to update")
		}

		updateFields = append(updateFields, "updated_at = CURRENT_TIMESTAMP")

		updateQuery := fmt.Sprintf(
			"UPDATE users SET %s WHERE id = $%d",
			strings.Join(updateFields, ", "),
			argIndex,
		)
		args = append(args, userID)

		_, err = db.Exec(updateQuery, args...)
		if err != nil {
			log.Printf("Failed to update user: %v", err)
			return respondError(c, http.StatusInternalServerError, "failed to update user profile")
		}

		return c.JSON(http.StatusOK, map[string]string{
			"message": "User profile updated successfully",
		})
	})

	e.DELETE("/users/:id", func(c echo.Context) error {
		userID := c.Param("id")

		tx, err := db.Begin()
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to start transaction")
		}
		defer tx.Rollback()

		// users への外部キーを持つ関連テーブルを先に削除する（ON DELETE CASCADE 未設定のため）。
		if _, err := tx.Exec(`DELETE FROM ephemeral_tokens WHERE user_id = $1`, userID); err != nil {
			log.Printf("Failed to delete ephemeral_tokens for user %s: %v", userID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		if _, err := tx.Exec(`DELETE FROM event_participants WHERE user_id = $1`, userID); err != nil {
			log.Printf("Failed to delete event_participants for user %s: %v", userID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		// 退会ユーザーが関わるすれ違い記録は相手側の履歴からも完全に削除する（決定済み仕様）。
		if _, err := tx.Exec(`DELETE FROM encounters WHERE my_id = $1 OR target_id = $1`, userID); err != nil {
			log.Printf("Failed to delete encounters for user %s: %v", userID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		result, err := tx.Exec(`DELETE FROM users WHERE id = $1`, userID)
		if err != nil {
			log.Printf("Failed to delete user %s: %v", userID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to delete user")
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			log.Printf("Failed to read rows affected for user %s deletion: %v", userID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to delete user")
		}
		if rowsAffected == 0 {
			return respondError(c, http.StatusNotFound, "User not found")
		}

		if err := tx.Commit(); err != nil {
			log.Printf("Failed to commit user deletion for %s: %v", userID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		// アバター画像の削除はベストエフォート。失敗してもアカウント削除自体は成功として扱う。
		if err := cleanupOldAvatars(c.Request().Context(), userID); err != nil {
			log.Printf("Failed to cleanup avatar files for user %s: %v", userID, err)
		}

		return c.JSON(http.StatusOK, map[string]string{"message": "User deleted"})
	})

	e.POST("/users/:id/avatar", func(c echo.Context) error {
		userID := c.Param("id")

		var exists bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if err != nil || !exists {
			return respondError(c, http.StatusNotFound, "User not found")
		}

		file, err := c.FormFile("avatar")
		if err != nil {
			return respondError(c, http.StatusBadRequest, "avatar file is required")
		}

		const maxSize = 5 * 1024 * 1024
		if file.Size > maxSize {
			return respondError(c, http.StatusBadRequest, fmt.Sprintf("file size exceeds %d bytes", maxSize))
		}

		allowedMimes := map[string]bool{
			"image/jpeg": true,
			"image/png":  true,
			"image/gif":  true,
			"image/webp": true,
		}
		if !allowedMimes[file.Header.Get("Content-Type")] {
			return respondError(c, http.StatusBadRequest, "only JPEG, PNG, GIF, WebP allowed")
		}

		src, err := file.Open()
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "failed to read file")
		}

		fileName := fmt.Sprintf("%s_%s%s", userID, uuid.New().String(), getFileExt(file.Filename))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		avatarURL, err := UploadUserAvatar(ctx, userID, src, fileName)
		if err != nil {
			log.Printf("Firebase upload error: %v", err)
			return respondError(c, http.StatusInternalServerError, "failed to upload avatar")
		}

		updateQuery := `UPDATE users SET icon_url = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		_, err = db.Exec(updateQuery, avatarURL, userID)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "failed to update user profile")
		}

		return c.JSON(http.StatusOK, map[string]string{
			"message":  "Avatar uploaded successfully",
			"icon_url": avatarURL,
		})
	})

	// --- すれ違い保存（重複排除・クールタイム付き） ---
	e.POST("/encounters", func(c echo.Context) error {
		var req struct {
			MyID        string `json:"my_id"`
			TargetID    string `json:"target_id"`
			TargetToken string `json:"target_token"`
			Token       string `json:"token"`
			EphemeralID string `json:"ephemeral_id"`
			EventID     *int   `json:"event_id"`
		}
		if err := c.Bind(&req); err != nil {
			return respondError(c, http.StatusBadRequest, "Invalid request format")
		}

		enc := &Encounter{
			MyID:     req.MyID,
			TargetID: req.TargetID,
			EventID:  req.EventID,
		}

		if enc.MyID == "" {
			return respondError(c, http.StatusBadRequest, "my_id is required")
		}

		enc.TargetID = req.TargetID
		if enc.TargetID == "" {
			token := req.TargetToken
			if token == "" {
				token = req.EphemeralID
			}
			if token == "" {
				token = req.Token
			}
			if token == "" {
				return respondError(c, http.StatusBadRequest, "target_id or target_token is required")
			}

			resolvedTargetID, err := resolveUserIDFromToken(db, token)
			if err != nil {
				log.Printf("encounter token resolve failed my_id=%s token=%s err=%v", enc.MyID, token, err)
				return respondError(c, http.StatusBadRequest, "invalid target token")
			}
			enc.TargetID = resolvedTargetID
		}

		if enc.MyID == enc.TargetID {
			return c.JSON(http.StatusOK, map[string]string{
				"message": "self encounter ignored",
			})
		}

		log.Printf("encounter request my_id=%s target_id=%s", enc.MyID, enc.TargetID)

		// 同じ相手とのすれ違いは1日1回まで
		var existsToday bool
		todayCheckQuery := `
			SELECT EXISTS (
				SELECT 1 FROM encounters
				WHERE ((my_id = $1 AND target_id = $2) OR (my_id = $2 AND target_id = $1))
				AND DATE(created_at) = CURRENT_DATE
			)
		`
		err := db.QueryRow(todayCheckQuery, enc.MyID, enc.TargetID).Scan(&existsToday)
		if err != nil {
			log.Printf("encounter daily-check error my_id=%s target_id=%s err=%v", enc.MyID, enc.TargetID, err)
			return respondError(c, http.StatusInternalServerError, "Database error during daily duplicate check")
		}

		if existsToday {
			log.Printf("encounter skipped same-day my_id=%s target_id=%s", enc.MyID, enc.TargetID)
			return c.JSON(http.StatusOK, map[string]string{
				"message": "Encounter skipped because this pair already met today",
			})
		}

		// 直近クールダウン内の重複を抑制
		var exists bool
		checkQuery := `
			SELECT EXISTS (
				SELECT 1 FROM encounters 
				WHERE ( (my_id = $1 AND target_id = $2) OR (my_id = $2 AND target_id = $1) )
				AND created_at > CURRENT_TIMESTAMP - ($3 * INTERVAL '1 second')
			)
		`
		err = db.QueryRow(checkQuery, enc.MyID, enc.TargetID, encounterCooldownSeconds).Scan(&exists)
		if err != nil {
			log.Printf("encounter duplicate-check error my_id=%s target_id=%s err=%v", enc.MyID, enc.TargetID, err)
			return respondError(c, http.StatusInternalServerError, "Database error during duplicate check")
		}

		// 直近ですれ違っている場合は保存しない
		if exists {
			log.Printf("encounter skipped cooldown=%ds my_id=%s target_id=%s", encounterCooldownSeconds, enc.MyID, enc.TargetID)
			return c.JSON(http.StatusOK, map[string]string{
				"message": "Encounter skipped due to cooldown",
			})
		}

		query := `INSERT INTO encounters (my_id, target_id, event_id) VALUES ($1, $2, $3) RETURNING id`
		err = db.QueryRow(query, enc.MyID, enc.TargetID, enc.EventID).Scan(&enc.ID)
		if err != nil {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				log.Printf("encounter skipped by unique index my_id=%s target_id=%s", enc.MyID, enc.TargetID)
				return c.JSON(http.StatusOK, map[string]string{
					"message": "Encounter skipped because this pair already met today",
				})
			}
			log.Printf("encounter insert error my_id=%s target_id=%s err=%v", enc.MyID, enc.TargetID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to record encounter")
		}
		log.Printf("encounter recorded id=%d my_id=%s target_id=%s", enc.ID, enc.MyID, enc.TargetID)
		return c.JSON(http.StatusCreated, enc)
	})

	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "passly API Server is running!")
	})
	e.HEAD("/", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	e.Logger.Fatal(e.Start(":" + port))
}

func initDB(db *sql.DB) {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
            id VARCHAR(50) PRIMARY KEY,
            name VARCHAR(100) NOT NULL,
            email VARCHAR(255) UNIQUE NOT NULL,
            password_hash TEXT NOT NULL,
            icon_url TEXT,
            one_word TEXT,
            role VARCHAR(50),
            tech_stack TEXT,
            twitter_url TEXT,
            github_url TEXT,
            portfolio_url TEXT,
            affiliation TEXT,
            connpass_username VARCHAR(255),
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        );`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS connpass_username VARCHAR(255);`,
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
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			log.Fatalf("Table creation error: %v", err)
		}
	}
}
