package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"passly_backend/auth"
	"passly_backend/connpass"
	"passly_backend/database"
	"passly_backend/firebase"
	"passly_backend/handler"
	"passly_backend/util"
)

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
	} else if err := firebase.InitFirebase(ctx, firebaseServiceAccount, firebaseBucket); err != nil {
		log.Printf("Firebase init error: %v (continuing without upload)\n", err)
	} else {
		log.Println("Firebase Storage initialized")
	}

	database.InitDB(db)
	if connpass.ShouldSeedMockOnStartup() {
		if err := connpass.SeedMockEventsFromFile(db, connpass.MockFilePath()); err != nil {
			log.Printf("connpass mock seed skipped: %v", err)
		} else {
			log.Printf("connpass mock seed completed from %s", connpass.MockFilePath())
		}
	}
	if err := database.SeedTechTags(db, "mock/tech_tags.json"); err != nil {
		log.Printf("tech tags seed skipped: %v", err)
	}
	encounterCooldownSeconds := util.GetEncounterCooldownSeconds()
	log.Printf("encounter cooldown seconds = %d", encounterCooldownSeconds)

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	registerRoutes(e, db, encounterCooldownSeconds)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	e.Logger.Fatal(e.Start(":" + port))
}

func registerRoutes(e *echo.Echo, db *sql.DB, encounterCooldownSeconds int) {
	// --- エフェメラルトークンAPI ---
	e.GET("/users/:id/ephemeral-token", handler.GetEphemeralToken(db))
	e.GET("/users/:id/ephemeral-tokens", auth.JWTRequired(db, handler.PrefetchEphemeralTokens(db)))
	e.POST("/encounters/resolve", handler.ResolveEncounter(db))

	// --- イベント取得API ---
	e.GET("/events", connpass.GetEvents(db))
	e.GET("/events/:id", connpass.GetEventDetail(db))
	e.GET("/users/:id/connpass-events", connpass.GetUserParticipatedEvents(db))

	// --- 技術タグ(選択式UI用マスタ) ---
	e.GET("/tech-tags", handler.GetTechTags(db))

	// --- 認証 ---
	e.POST("/signup", handler.Signup(db))
	e.POST("/login", handler.Login(db))

	// --- ホーム・カレンダー・実績 ---
	e.GET("/plaza/count", handler.GetPlazaCount(db))
	e.GET("/users/:id/home", handler.GetHome(db))
	e.GET("/users/:id/achievements", handler.GetAchievements(db))
	e.GET("/users/:id/calendar/daily", handler.GetCalendarDaily(db))
	e.GET("/users/:id/calendar/monthly", handler.GetCalendarMonthly(db))

	// --- すれ違い ---
	e.GET("/users/:id/encounters", handler.ListEncounters(db))
	e.PUT("/users/:id/encounters/confirm", auth.JWTRequired(db, handler.ConfirmEncounters(db)))
	e.POST("/encounters", auth.JWTRequired(db, handler.CreateEncounter(db, encounterCooldownSeconds)))
	e.POST("/encounters/batch", auth.JWTRequired(db, handler.CreateEncountersBatch(db)))

	// --- 親しい友達 ---
	e.POST("/users/:id/close-friends", handler.AddCloseFriend(db))
	e.DELETE("/users/:id/close-friends/:target_id", handler.RemoveCloseFriend(db))
	e.GET("/users/:id/close-friends", handler.ListCloseFriends(db))

	// --- ユーザー ---
	e.GET("/users/:id", handler.GetUser(db))
	e.PATCH("/users/:id", auth.JWTRequired(db, handler.UpdateUser(db)))
	e.PATCH("/users/:id/password", auth.JWTRequired(db, handler.ChangePassword(db)))
	e.DELETE("/users/:id", auth.JWTRequired(db, handler.DeleteUser(db)))
	e.POST("/users/:id/avatar", auth.JWTRequired(db, handler.UploadAvatar(db)))
	e.POST("/users/:id/cover", handler.UploadCover(db))

	// --- ヘルスチェック ---
	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "passly API Server is running!")
	})
	e.HEAD("/", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
}
