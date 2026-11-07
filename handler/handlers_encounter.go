package handler

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"

	"passly_backend/auth"
	"passly_backend/model"
	"passly_backend/util"
)

func ListEncounters(db *sql.DB) echo.HandlerFunc {
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
				COALESCE(u.affiliation, '') AS affiliation,
				le.met_at,
				COALESCE(ev.name, '') AS event_name,
				EXISTS(
					SELECT 1 FROM close_friends cf
					WHERE cf.user_id = $1 AND cf.friend_id = u.id
				) AS is_close_friend
			FROM (
				SELECT DISTINCT
					CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END AS other_id
				FROM encounters e
				WHERE e.my_id = $1 OR e.target_id = $1
			) counterparts
			JOIN users u ON u.id = counterparts.other_id
			LEFT JOIN LATERAL (
				SELECT e.created_at AS met_at, e.event_id
				FROM encounters e
				WHERE (e.my_id = $1 AND e.target_id = u.id)
				   OR (e.my_id = u.id AND e.target_id = $1)
				ORDER BY e.created_at DESC
				LIMIT 1
			) le ON true
			LEFT JOIN events ev ON ev.id = le.event_id
			ORDER BY le.met_at DESC
		`, userID)
		if err != nil {
			log.Printf("Failed to query encounters (user_id=%s): %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query encounters")
		}
		defer rows.Close()

		type LastEncounterJSON struct {
			MetAt     time.Time `json:"met_at"`
			EventName string    `json:"event_name"`
		}
		type EncounterUser struct {
			ID            string             `json:"id"`
			Name          string             `json:"name"`
			IconURL       string             `json:"icon_url"`
			OneWord       string             `json:"one_word"`
			Role          string             `json:"role"`
			TechStack     string             `json:"tech_stack"`
			Affiliation   string             `json:"affiliation"`
			IsCloseFriend bool               `json:"is_close_friend"`
			LastEncounter *LastEncounterJSON `json:"last_encounter"`
		}

		encounters := make([]EncounterUser, 0)
		for rows.Next() {
			var item EncounterUser
			var metAt sql.NullTime
			var eventName string
			if err := rows.Scan(&item.ID, &item.Name, &item.IconURL, &item.OneWord, &item.Role, &item.TechStack, &item.Affiliation, &metAt, &eventName, &item.IsCloseFriend); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to scan encounter record")
			}
			if metAt.Valid {
				item.LastEncounter = &LastEncounterJSON{MetAt: metAt.Time, EventName: eventName}
			}
			encounters = append(encounters, item)
		}

		if err := rows.Err(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to iterate encounters")
		}

		return c.JSON(http.StatusOK, encounters)
	}
}

func ConfirmEncounters(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		// 本人のみ確認操作を許可(2026-09-19)。
		if err := auth.RequireSelf(c, userID); err != nil {
			return err
		}
		query := `UPDATE encounters SET is_confirmed = TRUE WHERE (my_id = $1 OR target_id = $1) AND is_confirmed = FALSE`
		_, err := db.Exec(query, userID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to update confirmation status"})
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Encounters marked as confirmed"})
	}
}

// --- すれ違い保存（重複排除・クールタイム付き） ---
func CreateEncounter(db *sql.DB, encounterCooldownSeconds int) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req struct {
			MyID        string `json:"my_id"`
			TargetID    string `json:"target_id"`
			TargetToken string `json:"target_token"`
			Token       string `json:"token"`
			EphemeralID string `json:"ephemeral_id"`
			EventID     *int   `json:"event_id"`
		}
		if err := c.Bind(&req); err != nil {
			return util.RespondError(c, http.StatusBadRequest, "Invalid request format")
		}

		enc := &model.Encounter{
			MyID:     req.MyID,
			TargetID: req.TargetID,
			EventID:  req.EventID,
		}

		if enc.MyID == "" {
			return util.RespondError(c, http.StatusBadRequest, "my_id is required")
		}

		// なりすまし対策: my_id はJWTの認証済みユーザーと一致必須(2026-09-19)。
		if err := auth.RequireSelf(c, enc.MyID); err != nil {
			return err
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
				return util.RespondError(c, http.StatusBadRequest, "target_id or target_token is required")
			}

			resolvedTargetID, err := auth.ResolveUserIDFromToken(db, token)
			if err != nil {
				log.Printf("encounter token resolve failed my_id=%s token=%s err=%v", enc.MyID, token, err)
				return util.RespondError(c, http.StatusBadRequest, "invalid target token")
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
			return util.RespondError(c, http.StatusInternalServerError, "Database error during daily duplicate check")
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
			return util.RespondError(c, http.StatusInternalServerError, "Database error during duplicate check")
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
			return util.RespondError(c, http.StatusInternalServerError, "Failed to record encounter")
		}
		log.Printf("encounter recorded id=%d my_id=%s target_id=%s", enc.ID, enc.MyID, enc.TargetID)
		return c.JSON(http.StatusCreated, enc)
	}
}

// --- すれ違い一括登録（オフライン時にローカルへ貯めた分をまとめて送信するためのAPI） ---
func CreateEncountersBatch(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		// なりすまし対策: my_id はボディ/パスから受け取らず、必ずJWT(auth_user_id)から抽出する(設計書Phase1)。
		myID, _ := c.Get("auth_user_id").(string)
		if myID == "" {
			return util.RespondError(c, http.StatusUnauthorized, "unauthenticated")
		}

		var req struct {
			Encounters []struct {
				TargetID      string `json:"target_id"`
				TargetToken   string `json:"target_token"`
				Token         string `json:"token"`
				EphemeralID   string `json:"ephemeral_id"`
				EventID       *int   `json:"event_id"`
				EncounteredAt string `json:"encountered_at"`
			} `json:"encounters"`
		}
		if err := c.Bind(&req); err != nil {
			return util.RespondError(c, http.StatusBadRequest, "Invalid request format")
		}

		const maxBatchSize = 1000
		if len(req.Encounters) == 0 {
			return util.RespondError(c, http.StatusBadRequest, "encounters must not be empty")
		}
		if len(req.Encounters) > maxBatchSize {
			return util.RespondError(c, http.StatusBadRequest, fmt.Sprintf("encounters exceeds max batch size of %d", maxBatchSize))
		}

		type BatchResultItem struct {
			Index    int    `json:"index"`
			Status   string `json:"status"` // created | skipped_duplicate | skipped_self | invalid_token | error
			Message  string `json:"message,omitempty"`
			TargetID string `json:"target_id,omitempty"`
			ID       *int   `json:"id,omitempty"`
		}

		results := make([]BatchResultItem, len(req.Encounters))

		for i, item := range req.Encounters {
			result := BatchResultItem{Index: i}

			// encountered_at: クライアントが実際に検知した時刻(RFC3339)。
			// オフラインでまとめて送る想定のため、サーバー受信時刻ではなく検知時刻をcreated_atに使う。
			encounteredAt := time.Now().UTC()
			if item.EncounteredAt != "" {
				parsed, perr := time.Parse(time.RFC3339, item.EncounteredAt)
				if perr != nil {
					result.Status = "error"
					result.Message = "invalid encountered_at format (expected RFC3339)"
					results[i] = result
					continue
				}
				encounteredAt = parsed.UTC()
			}

			targetID := item.TargetID
			if targetID == "" {
				token := item.TargetToken
				if token == "" {
					token = item.EphemeralID
				}
				if token == "" {
					token = item.Token
				}
				if token == "" {
					result.Status = "error"
					result.Message = "target_id or target_token is required"
					results[i] = result
					continue
				}

				// サーバーの「現在時刻」ではなく、クライアント申告の検知時刻(encounteredAt)を
				// 基準にトークンの有効性を判定する(時計ズレ対策で前後60秒の許容誤差)。
				resolved, rerr := auth.ResolveUserIDFromTokenAt(db, token, encounteredAt, 60*time.Second)
				if rerr != nil {
					log.Printf("batch encounter token resolve failed my_id=%s index=%d err=%v", myID, i, rerr)
					result.Status = "invalid_token"
					result.Message = "invalid or expired token"
					results[i] = result
					continue
				}
				targetID = resolved
			}

			if targetID == myID {
				result.Status = "skipped_self"
				results[i] = result
				continue
			}
			result.TargetID = targetID

			// 同じペアが同日(検知時刻ベース)にすでに記録済みなら重複としてスキップ。
			// リアルタイム投稿と違い、貯めておいた分をまとめて送る想定のため
			// 直近クールダウン秒数によるチェックは行わない(同日重複排除とDBのユニーク制約のみで防ぐ)。
			var existsSameDay bool
			sameDayQuery := `
				SELECT EXISTS (
					SELECT 1 FROM encounters
					WHERE ((my_id = $1 AND target_id = $2) OR (my_id = $2 AND target_id = $1))
					AND DATE(created_at) = DATE($3)
				)
			`
			if err := db.QueryRow(sameDayQuery, myID, targetID, encounteredAt).Scan(&existsSameDay); err != nil {
				log.Printf("batch encounter daily-check error my_id=%s target_id=%s err=%v", myID, targetID, err)
				result.Status = "error"
				result.Message = "database error during duplicate check"
				results[i] = result
				continue
			}
			if existsSameDay {
				result.Status = "skipped_duplicate"
				results[i] = result
				continue
			}

			var newID int
			insertQuery := `INSERT INTO encounters (my_id, target_id, event_id, created_at) VALUES ($1, $2, $3, $4) RETURNING id`
			if err := db.QueryRow(insertQuery, myID, targetID, item.EventID, encounteredAt).Scan(&newID); err != nil {
				if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
					result.Status = "skipped_duplicate"
					results[i] = result
					continue
				}
				log.Printf("batch encounter insert error my_id=%s target_id=%s err=%v", myID, targetID, err)
				result.Status = "error"
				result.Message = "failed to record encounter"
				results[i] = result
				continue
			}

			log.Printf("batch encounter recorded id=%d my_id=%s target_id=%s encountered_at=%s", newID, myID, targetID, encounteredAt.Format(time.RFC3339))
			result.Status = "created"
			result.ID = &newID
			results[i] = result
		}

		// 設計書Phase1/3: 重複・無効はスキップ済み。全体は正常(201)としてまとめて返すが、
		// 「同日重複でスキップ」と「エラーで失敗」を呼び出し側が区別できるよう
		// アイテムごとの結果(results)も併せて返す。
		return c.JSON(http.StatusCreated, map[string]interface{}{
			"message": "Batch encounters successfully recorded",
			"results": results,
		})
	}
}
