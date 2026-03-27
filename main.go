package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Password     string `json:"password,omitempty"`
	IconURL      string `json:"icon_url"`
	OneWord      string `json:"one_word"`
	Role         string `json:"role"`
	TechStack    string `json:"tech_stack"`
	TwitterURL   string `json:"twitter_url"`
	GitHubURL    string `json:"github_url"`
	PortfolioURL string `json:"portfolio_url"`
	Affiliation  string `json:"affiliation"`
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

func GetConnpassEvents(c echo.Context) error {
	url := "https://connpass.com/api/v1/event/?keyword=Go&count=5"
	resp, err := http.Get(url)
	var connpassData ConnpassResponse
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to fetch events"})
	}
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

	url:= "https://connpass.com/api/v1/event/?owner_account=" + username
	resp, err := http.Get(url)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to fetch events"})
	}
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
	// .env ファイルを読み込む
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

	// Firebase 初期化
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	firebaseServiceAccount := os.Getenv("FIREBASE_SERVICE_ACCOUNT_PATH")
	firebaseBucket := os.Getenv("FIREBASE_STORAGE_BUCKET")
	if firebaseServiceAccount == "" {
		log.Println("⚠️  FIREBASE_SERVICE_ACCOUNT_PATH not set - avatar upload disabled")
	} else if err := InitFirebase(ctx, firebaseServiceAccount, firebaseBucket); err != nil {
		log.Printf("Firebase init error: %v (continuing without upload)\n", err)
	} else {
		log.Println("✓ Firebase Storage initialized")
	}

	initDB(db)

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	// --- エンドポイント（窓口）の定義 ---

	// 1. ユーザー登録（アカウント作成）
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
        INSERT INTO users (id, name, email, password_hash, icon_url, one_word, role, tech_stack, twitter_url, github_url, portfolio_url, affiliation)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

		_, err = db.Exec(query, u.ID, u.Name, u.Email, string(hashedPassword), u.IconURL, u.OneWord, u.Role, u.TechStack, u.TwitterURL, u.GitHubURL, u.PortfolioURL, u.Affiliation)
		if err != nil {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				return respondError(c, http.StatusConflict, "User ID or Email already exists")
			}
			return respondError(c, http.StatusInternalServerError, "Failed to insert user")
		}

		return c.JSON(http.StatusCreated, map[string]string{"message": "User registered successfully"})
	})

	// 2. ログイン（永続ログイン用）
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

	// 3. 【広場】総ユーザー数 / 本日すれ違い数 を返す API
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

	// 4. 【ホーム画面】データ一括取得（ランダム3人追加版）
	e.GET("/users/:id/home", func(c echo.Context) error {
		userID := c.Param("id")

		type HomeData struct {
			TotalEncounters int                      `json:"total_encounters"`
			TodayEncounters int                      `json:"today_encounters"`
			Unconfirmed     []map[string]interface{} `json:"unconfirmed"`
			RandomThree     []map[string]interface{} `json:"random_three"`
		}
		var res HomeData

		if err := db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE my_id = $1`, userID).Scan(&res.TotalEncounters); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count total encounters")
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE my_id = $1 AND DATE(created_at) = CURRENT_DATE`, userID).Scan(&res.TodayEncounters); err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to count today encounters")
		}

		rows, err := db.Query(`
            SELECT u.id, u.name, u.icon_url, u.one_word 
            FROM encounters e JOIN users u ON e.target_id = u.id 
            WHERE e.my_id = $1 AND e.is_confirmed = FALSE`, userID)
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
		if err := rows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Row iteration error for unconfirmed encounters")
		}

		randRows, err := db.Query(`
            SELECT u.id, u.name, u.icon_url, u.one_word 
            FROM encounters e JOIN users u ON e.target_id = u.id 
            WHERE e.my_id = $1 AND DATE(e.created_at) = CURRENT_DATE
            ORDER BY RANDOM() LIMIT 3`, userID)
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "Failed to query random encounters")
		}
		defer randRows.Close()

		for randRows.Next() {
			var id, name, icon, word string
			if err := randRows.Scan(&id, &name, &icon, &word); err != nil {
				return respondError(c, http.StatusInternalServerError, "Failed to scan random encounter record")
			}
			res.RandomThree = append(res.RandomThree, map[string]interface{}{
				"id": id, "name": name, "icon_url": icon, "one_word": word,
			})
		}
		if err := randRows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Row iteration error for random encounters")
		}

		return c.JSON(http.StatusOK, res)
	})

	// 4. 【ホーム画面】ポップアップの既読化（新規追加）
	e.PUT("/users/:id/encounters/confirm", func(c echo.Context) error {
		userID := c.Param("id")
		query := `UPDATE encounters SET is_confirmed = TRUE WHERE my_id = $1 AND is_confirmed = FALSE`
		_, err := db.Exec(query, userID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to update confirmation status"})
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Encounters marked as confirmed"})
	})

	// 5. 【カレンダー画面】日別データ取得（追加漏れ対応）
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

		countQuery := `SELECT COUNT(*) FROM encounters WHERE my_id = $1 AND DATE(created_at) = $2`
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
		if err := rows.Err(); err != nil {
			return respondError(c, http.StatusInternalServerError, "Row iteration error for events")
		}

		return c.JSON(http.StatusOK, res)
	})

	// 6. 【広場画面】ランダム10人取得（修正反映）
	e.GET("/users/:id/encounters", func(c echo.Context) error {
		myID := c.Param("id")
		query := `
            SELECT u.id, u.name, u.icon_url, u.one_word, e.created_at, e.is_confirmed
            FROM encounters e
            JOIN users u ON e.target_id = u.id
            WHERE e.my_id = $1
            ORDER BY RANDOM() LIMIT 10`

		rows, err := db.Query(query, myID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database error"})
		}
		defer rows.Close()

		var results []map[string]interface{}
		for rows.Next() {
			var id, name, icon, word, createdAt string
			var confirmed bool
			rows.Scan(&id, &name, &icon, &word, &createdAt, &confirmed)
			results = append(results, map[string]interface{}{
				"id": id, "name": name, "icon_url": icon, "one_word": word, "created_at": createdAt, "is_confirmed": confirmed,
			})
		}
		return c.JSON(http.StatusOK, results)
	})

	// 7. 【プロフィール画面】ユーザー詳細取得
	e.GET("/users/:id", func(c echo.Context) error {
		id := c.Param("id")
		var u User
		query := `SELECT id, name, icon_url, one_word, role, tech_stack, twitter_url, github_url, portfolio_url, affiliation, connpass_username FROM users WHERE id = $1`
		err := db.QueryRow(query, id).Scan(&u.ID, &u.Name, &u.IconURL, &u.OneWord, &u.Role, &u.TechStack, &u.TwitterURL, &u.GitHubURL, &u.PortfolioURL, &u.Affiliation, &u.ConnpassUsername)
		if err != nil {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "User not found"})
		}
		return c.JSON(http.StatusOK, u)
	})

	// 7.2. 【プロフィール画面】ユーザーデータ更新
	e.PATCH("/users/:id", func(c echo.Context) error {
		userID := c.Param("id")

		// ユーザー存在確認
		var exists bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if err != nil || !exists {
			return respondError(c, http.StatusNotFound, "User not found")
		}

		// リクエストボディをパース（パーシャル更新対応）
		var updateData map[string]interface{}
		if err := c.Bind(&updateData); err != nil {
			return respondError(c, http.StatusBadRequest, "Invalid request format")
		}

		// 動的なUPDATEクエリを構築
		var updateFields []string
		var args []interface{}
		argIndex := 1

		// name の処理（空でも許容）
		if name, ok := updateData["name"].(string); ok {
			updateFields = append(updateFields, fmt.Sprintf("name = $%d", argIndex))
			args = append(args, name)
			argIndex++
		}

		// オプショナルフィールド
		optionalFields := map[string]string{
			"one_word":      "one_word",
			"tech_stack":    "tech_stack",
			"twitter_url":   "twitter_url",
			"github_url":    "github_url",
			"portfolio_url": "portfolio_url",
			"affiliation":   "affiliation",
			"connpass_username": "connpass_username",
		}

		for jsonKey, dbCol := range optionalFields {
			if value, ok := updateData[jsonKey].(string); ok {
				updateFields = append(updateFields, fmt.Sprintf("%s = $%d", dbCol, argIndex))
				args = append(args, value)
				argIndex++
			}
		}

		// 更新フィールドがない場合
		if len(updateFields) == 0 {
			return respondError(c, http.StatusBadRequest, "No fields to update")
		}

		// updated_at を追加
		updateFields = append(updateFields, "updated_at = CURRENT_TIMESTAMP")

		// クエリを構築して実行
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

	// 7.3. 【ユーザーアバター】画像アップロード（Firebase Storage）
	e.POST("/users/:id/avatar", func(c echo.Context) error {
		userID := c.Param("id")

		// ユーザー存在確認
		var exists bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if err != nil || !exists {
			return respondError(c, http.StatusNotFound, "User not found")
		}

		// ファイル受け取り
		file, err := c.FormFile("avatar")
		if err != nil {
			return respondError(c, http.StatusBadRequest, "avatar file is required")
		}

		// ファイルサイズチェック（5MB上限）
		const maxSize = 5 * 1024 * 1024
		if file.Size > maxSize {
			return respondError(c, http.StatusBadRequest, fmt.Sprintf("file size exceeds %d bytes", maxSize))
		}

		// ファイル形式チェック
		allowedMimes := map[string]bool{
			"image/jpeg": true,
			"image/png":  true,
			"image/gif":  true,
			"image/webp": true,
		}
		if !allowedMimes[file.Header.Get("Content-Type")] {
			return respondError(c, http.StatusBadRequest, "only JPEG, PNG, GIF, WebP allowed")
		}

		// Firebase にアップロード
		src, err := file.Open()
		if err != nil {
			return respondError(c, http.StatusInternalServerError, "failed to read file")
		}

		// ファイル名を生成（user_id + uuid）
		fileName := fmt.Sprintf("%s_%s%s", userID, uuid.New().String(), getFileExt(file.Filename))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		avatarURL, err := UploadUserAvatar(ctx, userID, src, fileName)
		if err != nil {
			log.Printf("Firebase upload error: %v", err)
			return respondError(c, http.StatusInternalServerError, "failed to upload avatar")
		}

		// DB 更新
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

	// 【すれ違い保存】BLE通信用
	e.GET("/events", GetConnpassEvents)
	e.GET("/users/:username/events", GetUserConnpassEvents)
	e.POST("/encounters", func(c echo.Context) error {
		enc := new(Encounter)
		if err := c.Bind(enc); err != nil {
			return err
		}
		query := `INSERT INTO encounters (my_id, target_id, event_id) VALUES ($1, $2, $3) RETURNING id`
		err := db.QueryRow(query, enc.MyID, enc.TargetID, enc.EventID).Scan(&enc.ID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to record encounter"})
		}
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
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			log.Fatalf("Table creation error: %v", err)
		}
	}
}
