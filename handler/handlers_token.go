package handler

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"passly_backend/auth"
	"passly_backend/util"
)

// --- エフェメラルトークンAPI ---
// BLE広告名20文字制限に合わせた短いトークンを返す。
func GetEphemeralToken(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		token, err := util.GenerateEphemeralToken()
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "failed to generate token")
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
			return util.RespondError(c, http.StatusInternalServerError, "failed to store token")
		}

		return c.JSON(http.StatusOK, map[string]string{
			"token":      token,
			"expires_in": "3600",
			"expires_at": expiresAt,
		})
	}
}

// オフライン用トークン事前配布API(2026-09-18追加)。
// オフライン中はサーバーに5分おきの再取得ができないため、これから先の時間帯に
// 使うトークンをまとめて事前発行し、それぞれの有効開始時刻(valid_from)・
// 有効期限(expires_at)を添えて返す。クライアント側はオフラインになる前に
// この一覧を取得しておき、時刻に応じて広告するトークンを順番に切り替える。
// このAPIは自分自身のトークンしか取得できないようJWT必須にしている。
// 未認証のままだと、他人のuser_idさえ知っていれば誰でもそのユーザーになりすませる
// 有効なトークンをまとめて入手できてしまうため(2026-09-18、設計レビューで発覚)。
func PrefetchEphemeralTokens(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		if userID == "" {
			return util.RespondError(c, http.StatusBadRequest, "id is required")
		}
		if err := auth.RequireSelf(c, userID); err != nil {
			return err
		}

		const slotCount = 24               // 事前発行する個数(24時間分)
		const slotDuration = 1 * time.Hour // 1個あたりの有効時間(設計書: 1時間ごとに切替)

		type PrefetchToken struct {
			Token     string `json:"token"`
			ValidFrom string `json:"valid_from"`
			ExpiresAt string `json:"expires_at"`
		}

		now := time.Now().UTC()
		tokens := make([]PrefetchToken, 0, slotCount)

		tx, err := db.Begin()
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "failed to start transaction")
		}
		defer tx.Rollback()

		upsertQuery := `
			INSERT INTO ephemeral_tokens (token, user_id, valid_from, expires_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (token)
			DO UPDATE SET user_id = EXCLUDED.user_id, valid_from = EXCLUDED.valid_from, expires_at = EXCLUDED.expires_at, created_at = CURRENT_TIMESTAMP
		`

		for i := 0; i < slotCount; i++ {
			validFrom := now.Add(time.Duration(i) * slotDuration)
			expiresAt := validFrom.Add(slotDuration)

			token, terr := util.GenerateEphemeralToken()
			if terr != nil {
				return util.RespondError(c, http.StatusInternalServerError, "failed to generate token")
			}

			if _, err := tx.Exec(upsertQuery, token, userID, validFrom, expiresAt); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "failed to store token")
			}

			tokens = append(tokens, PrefetchToken{
				Token:     token,
				ValidFrom: validFrom.Format(time.RFC3339),
				ExpiresAt: expiresAt.Format(time.RFC3339),
			})
		}

		if err := tx.Commit(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "failed to commit prefetched tokens")
		}

		return c.JSON(http.StatusOK, map[string]interface{}{
			"tokens": tokens,
		})
	}
}

func ResolveEncounter(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req struct {
			Token       string `json:"token"`
			EphemeralID string `json:"ephemeral_id"`
		}
		if err := c.Bind(&req); err != nil {
			return util.RespondError(c, http.StatusBadRequest, "Invalid request")
		}
		token := req.Token
		if token == "" {
			token = req.EphemeralID
		}
		if token == "" {
			return util.RespondError(c, http.StatusBadRequest, "token or ephemeral_id is required")
		}

		resolvedUserID, err := auth.ResolveUserIDFromToken(db, token)
		if err != nil {
			log.Printf("resolve failed token=%s err=%v", token, err)
			return util.RespondError(c, http.StatusBadRequest, "Invalid token")
		}
		log.Printf("resolve success token=%s user_id=%s", token, resolvedUserID)

		return c.JSON(http.StatusOK, map[string]string{
			"user_id": resolvedUserID,
		})
	}
}
