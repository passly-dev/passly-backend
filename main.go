package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"github.com/labstack/echo/v4"
	_ "github.com/lib/pq"
)

func main() {
	// データベースの接続情報（docker-compose.ymlの設定と一致させます）
	dbUrl := os.Getenv("DATABASE_URL")
	if dbUrl == ""{
		dbUrl = "host=127.0.0.1 port=5432 user=user password=password dbname=streetpass sslmode=disable"
	}
	db, err := sql.Open("postgres", dbUrl)
	if err != nil {
		log.Fatal("DB起動エラー:", err)
	}
	defer db.Close()

	// 接続確認
	if err := db.Ping(); err != nil {
		log.Fatal("DB接続エラー:", err)
	}
	fmt.Println("データベース接続成功！")

	// テーブルの作成（すでに存在する場合はスキップ）
	createTableQuery := `
	CREATE TABLE IF NOT EXISTS encounters (
		id SERIAL PRIMARY KEY,
		my_id VARCHAR(50) NOT NULL,
		target_id VARCHAR(50) NOT NULL,
		latitude DOUBLE PRECISION,
		longitude DOUBLE PRECISION,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := db.Exec(createTableQuery); err != nil {
		log.Fatal("テーブル作成エラー:", err)
	}
	fmt.Println("テーブルの準備完了！")

	// サーバーの起動
	e := echo.New()

	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "API Server and DB are running!")
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	e.Logger.Fatal(e.Start(":" + port))
}