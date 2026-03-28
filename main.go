package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
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
	Events []ConnpassEvent `json:"events"`
}

type ConnpassEvent struct {
	EventID   int    `json:"event_id"`
	Title     string `json:"title"`
	Place     string `json:"place"`
	StartedAt string `json:"start_at"`
	EndedAt   string `json:"end_at"`
}

type Event struct {
	ID       int       `json:"id"`
	Name     string    `json:"name"`
	Location string    `json:"location"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
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

func GetConnpassEvents(c echo.Context) error {
	url := "https://connpass.com/api/v1/event/?keyword=Go&count=5"
	resp, err := http.Get(url)
	var connpassData ConnpassResponse
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to fetch events"})
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&connpassData)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to decode events"})
	}

	var frontendEvents []Event
	for _, ce := range connpassData.Events {
		startAt, _ := time.Parse(time.RFC3339, ce.StartedAt)
		endAt, _ := time.Parse(time.RFC3339, ce.EndedAt)
		frontendEvents = append(frontendEvents, Event{
			ID:       ce.EventID,
			Name:     ce.Title,
			Location: ce.Place,
			StartAt:  startAt,
			EndAt:    endAt,
		})
	}
	return c.JSON(http.StatusOK, frontendEvents)
}

func GetUserConnpassEvents(c echo.Context) error {
	username := c.Param("username")

	url := "https://connpass.com/api/v1/event/?owner_account=" + username
	resp, err := http.Get(url)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to fetch events"})
	}
	defer resp.Body.Close()

	var connpassData ConnpassResponse
	err = json.NewDecoder(resp.Body).Decode(&connpassData)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to decode events"})
	}

	var frontendEvents []Event
	for _, ce := range connpassData.Events {
		startAt, _ := time.Parse(time.RFC3339, ce.StartedAt)
		endAt, _ := time.Parse(time.RFC3339, ce.EndedAt)
		frontendEvents = append(frontendEvents, Event{
			ID:       ce.EventID,
			Name:     ce.Title,
			Location: ce.Place,
			StartAt:  startAt,
			EndAt:    endAt,
		})
	}
	return c.JSON(http.StatusOK, frontendEvents)
}

func main() {
	godotenv.Load()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "host=localhost port=5432 user=user password=pass dbname=streetpass sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	firebaseServiceAccount := os.Getenv("FIREBASE_SERVICE_ACCOUNT_PATH")
	firebaseBucket := os.Getenv("FIREBASE_STORAGE_BUCKET")
	if firebaseServiceAccount == "" {
		log.Println("FIREBASE_SERVICE_ACCOUNT_PATH not set - avatar upload disabled")
	} else if err := InitFirebase(ctx, firebaseServiceAccount, firebaseBucket); err != nil {
		log.Printf("Firebase init error: %v (continuing without upload)\n", err)
	} else {
		log.Println("Firebase Storage initialized")
	}

	initDB(db)
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
			log.Printf("resolve success token=%s user_id=%s", token, resolvedUserID)
			return c.JSON(http.StatusOK, map[string]string{
				"user_id": resolvedUserID,
			})
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("resolve db error token=%s err=%v", token, err)
			return respondError(c, http.StatusInternalServerError, "Failed to resolve token")
		}

		// 旧方式との後方互換: Base64デコードも許容
		decoded, decodeErr := base64.StdEncoding.DecodeString(token)
		if decodeErr != nil {
			log.Printf("resolve invalid token=%s", token)
			return respondError(c, http.StatusBadRequest, "Invalid token")
		}
		log.Printf("resolve fallback(base64) token=%s user_id=%s", token, string(decoded))

		return c.JSON(http.StatusOK, map[string]string{
			"user_id": string(decoded),
		})
	})

	// --- イベント取得API ---
	e.GET("/events", GetConnpassEvents)
	e.GET("/users/:username/events", GetUserConnpassEvents)

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

		if err := db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE my_id = $1 OR target_id = $1`, userID).Scan(&res.TotalEncounters); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count total encounters")
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE (my_id = $1 OR target_id = $1) AND DATE(created_at) = CURRENT_DATE`, userID).Scan(&res.TodayEncounters); err != nil {
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

	// 1. 【復旧】正しい daily API (1日分のデータを返す)
	// 理由: フロントエンドが現在このAPIを使ってカレンダーを描画しているため、まずは正しく動く状態に戻す。
	e.GET("/users/:id/calendar/daily", func(c echo.Context) error {
		userID := c.Param("id")
		dateStr := c.QueryParam("date")
		if dateStr == "" {
			dateStr = time.Now().Format("2006-01-02")
		}

		type DailyData struct {
			EncounterCount int      `json:"encounter_count"`
			EventNames     []string `json:"event_names"`
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
		enc := new(Encounter)
		if err := c.Bind(enc); err != nil {
			return respondError(c, http.StatusBadRequest, "Invalid request format")
		}
		if enc.MyID == "" || enc.TargetID == "" {
			return respondError(c, http.StatusBadRequest, "my_id and target_id are required")
		}

		log.Printf("encounter request my_id=%s target_id=%s", enc.MyID, enc.TargetID)

		// 直近クールダウン内の重複を抑制
		var exists bool
		checkQuery := `
			SELECT EXISTS (
				SELECT 1 FROM encounters 
				WHERE my_id = $1 AND target_id = $2 
				AND created_at > CURRENT_TIMESTAMP - ($3 * INTERVAL '1 second')
			)
		`
		err := db.QueryRow(checkQuery, enc.MyID, enc.TargetID, encounterCooldownSeconds).Scan(&exists)
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
			log.Printf("encounter insert error my_id=%s target_id=%s err=%v", enc.MyID, enc.TargetID, err)
			return respondError(c, http.StatusInternalServerError, "Failed to record encounter")
		}
		log.Printf("encounter recorded id=%d my_id=%s target_id=%s", enc.ID, enc.MyID, enc.TargetID)
		return c.JSON(http.StatusCreated, enc)
	})

	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "StreetPass API Server is running!")
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
		`CREATE TABLE IF NOT EXISTS ephemeral_tokens (
			token VARCHAR(64) PRIMARY KEY,
			user_id VARCHAR(50) REFERENCES users(id),
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_ephemeral_tokens_user_id ON ephemeral_tokens(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_ephemeral_tokens_expires_at ON ephemeral_tokens(expires_at);`,
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			log.Fatalf("Table creation error: %v", err)
		}
	}
}
