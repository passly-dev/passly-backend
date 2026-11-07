package handler

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/labstack/echo/v4"

	"passly_backend/util"
)

// GetTechTags は選択式UI用の技術タグ一覧を返す。
func GetTechTags(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		rows, err := db.Query(`SELECT id, name, COALESCE(category, '') FROM tech_tags ORDER BY category, id`)
		if err != nil {
			log.Printf("Failed to query tech tags: %v", err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query tech tags")
		}
		defer rows.Close()

		type techTag struct {
			ID       int    `json:"id"`
			Name     string `json:"name"`
			Category string `json:"category"`
		}

		tags := make([]techTag, 0)
		for rows.Next() {
			var t techTag
			if err := rows.Scan(&t.ID, &t.Name, &t.Category); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to scan tech tag")
			}
			tags = append(tags, t)
		}
		if err := rows.Err(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to iterate tech tags")
		}

		return c.JSON(http.StatusOK, tags)
	}
}
