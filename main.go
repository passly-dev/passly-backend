package main

import (
    "database/sql"
    "log"
    "net/http"
    "os"

    "github.com/labstack/echo/v4"
    "github.com/labstack/echo/v4/middleware"
    _ "github.com/lib/pq"
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
}

type Event struct {
    ID       int    `json:"id"`
    Name     string `json:"name"`
    Location string `json:"location"`
    StartAt  string `json:"start_at"`
    EndAt    string `json:"end_at"`
}

type Encounter struct {
    ID          int    `json:"id"`
    MyID        string `json:"my_id"`
    TargetID    string `json:"target_id"`
    EventID     *int   `json:"event_id"`
    IsConfirmed bool   `json:"is_confirmed"`
    CreatedAt   string `json:"created_at"`
}

func main() {
    dbURL := os.Getenv("DATABASE_URL")
    if dbURL == "" {
        dbURL = "host=localhost port=5432 user=user password=pass dbname=streetpass sslmode=disable"
    }

    db, err := sql.Open("postgres", dbURL)
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    initDB(db)

    e := echo.New()
    e.Use(middleware.Logger())
    e.Use(middleware.Recover())

    // --- エンドポイント（窓口）の定義 ---

    // 1. ユーザー登録（アカウント作成）
    e.POST("/signup", func(c echo.Context) error {
        var u User
        if err := c.Bind(&u); err != nil {
            return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
        }

        hashedPassword, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
        if err != nil {
            return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to hash password"})
        }

        query := `
        INSERT INTO users (id, name, email, password_hash, icon_url, one_word, role, tech_stack, twitter_url, github_url, portfolio_url, affiliation)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
        
        _, err = db.Exec(query, u.ID, u.Name, u.Email, string(hashedPassword), u.IconURL, u.OneWord, u.Role, u.TechStack, u.TwitterURL, u.GitHubURL, u.PortfolioURL, u.Affiliation)
        if err != nil {
            return c.JSON(http.StatusConflict, map[string]string{"error": "User ID or Email already exists"})
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

    // 3. 【ホーム画面】データ一括取得（ランダム3人追加版）
    e.GET("/users/:id/home", func(c echo.Context) error {
        userID := c.Param("id")

        type HomeData struct {
            TotalEncounters int                      `json:"total_encounters"`
            TodayEncounters int                      `json:"today_encounters"`
            Unconfirmed     []map[string]interface{} `json:"unconfirmed"`
            RandomThree     []map[string]interface{} `json:"random_three"`
        }
        var res HomeData

        db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE my_id = $1`, userID).Scan(&res.TotalEncounters)
        db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE my_id = $1 AND DATE(created_at) = CURRENT_DATE`, userID).Scan(&res.TodayEncounters)

        rows, _ := db.Query(`
            SELECT u.id, u.name, u.icon_url, u.one_word 
            FROM encounters e JOIN users u ON e.target_id = u.id 
            WHERE e.my_id = $1 AND e.is_confirmed = FALSE`, userID)
        defer rows.Close()
        for rows.Next() {
            var id, name, icon, word string
            rows.Scan(&id, &name, &icon, &word)
            res.Unconfirmed = append(res.Unconfirmed, map[string]interface{}{
                "id": id, "name": name, "icon_url": icon, "one_word": word,
            })
        }

        randRows, _ := db.Query(`
            SELECT u.id, u.name, u.icon_url, u.one_word 
            FROM encounters e JOIN users u ON e.target_id = u.id 
            WHERE e.my_id = $1 AND DATE(e.created_at) = CURRENT_DATE
            ORDER BY RANDOM() LIMIT 3`, userID)
        defer randRows.Close()
        for randRows.Next() {
            var id, name, icon, word string
            randRows.Scan(&id, &name, &icon, &word)
            res.RandomThree = append(res.RandomThree, map[string]interface{}{
                "id": id, "name": name, "icon_url": icon, "one_word": word,
            })
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

        type DailyData struct {
            EncounterCount int      `json:"encounter_count"`
            EventNames     []string `json:"event_names"`
        }
        var res DailyData

        countQuery := `SELECT COUNT(*) FROM encounters WHERE my_id = $1 AND DATE(created_at) = $2`
        if err := db.QueryRow(countQuery, userID, dateStr).Scan(&res.EncounterCount); err != nil {
            return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to count encounters"})
        }

        eventQuery := `SELECT name FROM events WHERE DATE(start_at) = $1`
        rows, err := db.Query(eventQuery, dateStr)
        if err == nil {
            defer rows.Close()
            for rows.Next() {
                var eventName string
                rows.Scan(&eventName)
                res.EventNames = append(res.EventNames, eventName)
            }
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
        query := `SELECT id, name, icon_url, one_word, role, tech_stack, twitter_url, github_url, portfolio_url, affiliation FROM users WHERE id = $1`
        err := db.QueryRow(query, id).Scan(&u.ID, &u.Name, &u.IconURL, &u.OneWord, &u.Role, &u.TechStack, &u.TwitterURL, &u.GitHubURL, &u.PortfolioURL, &u.Affiliation)
        if err != nil {
            return c.JSON(http.StatusNotFound, map[string]string{"error": "User not found"})
        }
        return c.JSON(http.StatusOK, u)
    })

    // 【すれ違い保存】BLE通信用
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