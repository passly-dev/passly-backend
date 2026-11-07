package handler

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"passly_backend/util"
)

// 親しい友達（close friends）
// フロント: lib/features/close_friend/data/close_friend_repository_api.dart が前提とするAPI。
func AddCloseFriend(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")

		var body struct {
			TargetID string `json:"target_id"`
		}
		if err := c.Bind(&body); err != nil || body.TargetID == "" {
			return util.RespondError(c, http.StatusBadRequest, "target_id is required")
		}
		if body.TargetID == userID {
			return util.RespondError(c, http.StatusBadRequest, "cannot add yourself as a close friend")
		}

		var targetExists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, body.TargetID).Scan(&targetExists); err != nil || !targetExists {
			return util.RespondError(c, http.StatusNotFound, "target user not found")
		}

		_, err := db.Exec(
			`INSERT INTO close_friends (user_id, friend_id) VALUES ($1, $2) ON CONFLICT (user_id, friend_id) DO NOTHING`,
			userID, body.TargetID,
		)
		if err != nil {
			log.Printf("Failed to add close friend: %v", err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to add close friend")
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Close friend added"})
	}
}

func RemoveCloseFriend(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		targetID := c.Param("target_id")

		_, err := db.Exec(`DELETE FROM close_friends WHERE user_id = $1 AND friend_id = $2`, userID, targetID)
		if err != nil {
			log.Printf("Failed to remove close friend: %v", err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to remove close friend")
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Close friend removed"})
	}
}

func ListCloseFriends(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
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
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query close friends")
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
				return util.RespondError(c, http.StatusInternalServerError, "Failed to scan close friend record")
			}
			item.IsCloseFriend = true
			if metAt.Valid {
				item.LastEncounter = &LastEncounterJSON{MetAt: metAt.Time, EventName: eventName}
			}
			friends = append(friends, item)
		}
		if err := rows.Err(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to iterate close friends")
		}

		return c.JSON(http.StatusOK, friends)
	}
}
