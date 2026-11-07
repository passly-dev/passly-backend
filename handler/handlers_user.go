package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	"passly_backend/auth"
	"passly_backend/firebase"
	"passly_backend/model"
	"passly_backend/util"
)

func GetUser(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := c.Param("id")
		var u model.User
		query := `SELECT
			id,
			name,
			COALESCE(icon_url, ''),
			COALESCE(cover_url, ''),
			COALESCE(one_word, ''),
			COALESCE(role, 'other'),
			COALESCE(tech_stack, ''),
			COALESCE(twitter_url, ''),
			COALESCE(github_url, ''),
			COALESCE(portfolio_url, ''),
			COALESCE(affiliation, ''),
			COALESCE(about, ''),
			COALESCE(connpass_username, '')
		FROM users
		WHERE id = $1`
		err := db.QueryRow(query, id).Scan(&u.ID, &u.Name, &u.IconURL, &u.CoverURL, &u.OneWord, &u.Role, &u.TechStack, &u.TwitterURL, &u.GitHubURL, &u.PortfolioURL, &u.Affiliation, &u.About, &u.ConnpassUsername)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return c.JSON(http.StatusNotFound, map[string]string{"error": "User not found"})
			}
			log.Printf("GET /users/:id query error (id=%s): %v", id, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database error"})
		}
		return c.JSON(http.StatusOK, u)
	}
}

func UpdateUser(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		if err := auth.RequireSelf(c, userID); err != nil {
			return err
		}

		var exists bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if err != nil || !exists {
			return util.RespondError(c, http.StatusNotFound, "User not found")
		}

		var updateData map[string]interface{}
		if err := c.Bind(&updateData); err != nil {
			return util.RespondError(c, http.StatusBadRequest, "Invalid request format")
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
			"about":             "about",
		}

		for jsonKey, dbCol := range optionalFields {
			if value, ok := updateData[jsonKey].(string); ok {
				updateFields = append(updateFields, fmt.Sprintf("%s = $%d", dbCol, argIndex))
				args = append(args, value)
				argIndex++
			}
		}

		if len(updateFields) == 0 {
			return util.RespondError(c, http.StatusBadRequest, "No fields to update")
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
			return util.RespondError(c, http.StatusInternalServerError, "failed to update user profile")
		}

		return c.JSON(http.StatusOK, map[string]string{
			"message": "User profile updated successfully",
		})
	}
}

func ChangePassword(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		if err := auth.RequireSelf(c, userID); err != nil {
			return err
		}

		var req struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := c.Bind(&req); err != nil {
			return util.RespondError(c, http.StatusBadRequest, "Invalid request body")
		}
		if req.CurrentPassword == "" || req.NewPassword == "" {
			return util.RespondError(c, http.StatusBadRequest, "current_password and new_password are required")
		}
		if len(req.NewPassword) < 6 {
			return util.RespondError(c, http.StatusBadRequest, "new_password must be at least 6 characters")
		}

		var storedHash string
		if err := db.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&storedHash); err != nil {
			if err == sql.ErrNoRows {
				return util.RespondError(c, http.StatusNotFound, "User not found")
			}
			log.Printf("Failed to fetch password hash: %v", err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to fetch user")
		}

		if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.CurrentPassword)); err != nil {
			return util.RespondError(c, http.StatusUnauthorized, "current password is incorrect")
		}

		newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to hash password")
		}

		if _, err := db.Exec(`UPDATE users SET password_hash = $1 WHERE id = $2`, string(newHash), userID); err != nil {
			log.Printf("Failed to update password: %v", err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to update password")
		}

		return c.JSON(http.StatusOK, map[string]string{"message": "Password updated successfully"})
	}
}

func DeleteUser(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		if err := auth.RequireSelf(c, userID); err != nil {
			return err
		}

		tx, err := db.Begin()
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to start transaction")
		}
		defer tx.Rollback()

		// users への外部キーを持つ関連テーブルを先に削除する（ON DELETE CASCADE 未設定のため）。
		if _, err := tx.Exec(`DELETE FROM ephemeral_tokens WHERE user_id = $1`, userID); err != nil {
			log.Printf("Failed to delete ephemeral_tokens for user %s: %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		if _, err := tx.Exec(`DELETE FROM event_participants WHERE user_id = $1`, userID); err != nil {
			log.Printf("Failed to delete event_participants for user %s: %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		// 退会ユーザーが関わるすれ違い記録は相手側の履歴からも完全に削除する（決定済み仕様）。
		if _, err := tx.Exec(`DELETE FROM encounters WHERE my_id = $1 OR target_id = $1`, userID); err != nil {
			log.Printf("Failed to delete encounters for user %s: %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		result, err := tx.Exec(`DELETE FROM users WHERE id = $1`, userID)
		if err != nil {
			log.Printf("Failed to delete user %s: %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to delete user")
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			log.Printf("Failed to read rows affected for user %s deletion: %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to delete user")
		}
		if rowsAffected == 0 {
			return util.RespondError(c, http.StatusNotFound, "User not found")
		}

		if err := tx.Commit(); err != nil {
			log.Printf("Failed to commit user deletion for %s: %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to delete user")
		}

		// アバター・カバー画像の削除はベストエフォート。失敗してもアカウント削除自体は成功として扱う。
		if err := firebase.CleanupOldAvatars(c.Request().Context(), userID); err != nil {
			log.Printf("Failed to cleanup avatar files for user %s: %v", userID, err)
		}
		if err := firebase.CleanupOldCovers(c.Request().Context(), userID); err != nil {
			log.Printf("Failed to cleanup cover files for user %s: %v", userID, err)
		}

		return c.JSON(http.StatusOK, map[string]string{"message": "User deleted"})
	}
}

func UploadAvatar(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		if err := auth.RequireSelf(c, userID); err != nil {
			return err
		}

		var exists bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if err != nil || !exists {
			return util.RespondError(c, http.StatusNotFound, "User not found")
		}

		file, err := c.FormFile("avatar")
		if err != nil {
			return util.RespondError(c, http.StatusBadRequest, "avatar file is required")
		}

		const maxSize = 5 * 1024 * 1024
		if file.Size > maxSize {
			return util.RespondError(c, http.StatusBadRequest, fmt.Sprintf("file size exceeds %d bytes", maxSize))
		}

		allowedMimes := map[string]bool{
			"image/jpeg": true,
			"image/png":  true,
			"image/gif":  true,
			"image/webp": true,
		}
		if !allowedMimes[file.Header.Get("Content-Type")] {
			return util.RespondError(c, http.StatusBadRequest, "only JPEG, PNG, GIF, WebP allowed")
		}

		src, err := file.Open()
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "failed to read file")
		}

		fileName := fmt.Sprintf("%s_%s%s", userID, uuid.New().String(), util.GetFileExt(file.Filename))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		avatarURL, err := firebase.UploadUserAvatar(ctx, userID, src, fileName)
		if err != nil {
			log.Printf("Firebase upload error: %v", err)
			return util.RespondError(c, http.StatusInternalServerError, "failed to upload avatar")
		}

		updateQuery := `UPDATE users SET icon_url = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		result, err := db.Exec(updateQuery, avatarURL, userID)
		if err != nil {
			log.Printf("avatar db update error user_id=%q avatar_url=%q err=%v", userID, avatarURL, err)
			return util.RespondError(c, http.StatusInternalServerError, "failed to update user profile")
		}
		// UPDATEはWHERE条件に一致する行が0件でもGoのerrorにはならないため、
		// 「200を返したのにDBには反映されていない」という事故を防ぐために
		// 実際の更新件数を必ず確認する。
		rowsAffected, raErr := result.RowsAffected()
		log.Printf("avatar db update user_id=%q avatar_url=%q rows_affected=%d rows_affected_err=%v", userID, avatarURL, rowsAffected, raErr)
		if raErr == nil && rowsAffected == 0 {
			log.Printf("avatar update matched 0 rows for user_id=%q — icon_url was NOT persisted", userID)
			return util.RespondError(c, http.StatusInternalServerError, "avatar upload succeeded but user record was not updated (0 rows matched)")
		}

		return c.JSON(http.StatusOK, map[string]string{
			"message":  "Avatar uploaded successfully",
			"icon_url": avatarURL,
		})
	}
}

func UploadCover(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")

		var exists bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if err != nil || !exists {
			return util.RespondError(c, http.StatusNotFound, "User not found")
		}

		file, err := c.FormFile("cover")
		if err != nil {
			return util.RespondError(c, http.StatusBadRequest, "cover file is required")
		}

		const maxSize = 5 * 1024 * 1024
		if file.Size > maxSize {
			return util.RespondError(c, http.StatusBadRequest, fmt.Sprintf("file size exceeds %d bytes", maxSize))
		}

		allowedMimes := map[string]bool{
			"image/jpeg": true,
			"image/png":  true,
			"image/gif":  true,
			"image/webp": true,
		}
		if !allowedMimes[file.Header.Get("Content-Type")] {
			return util.RespondError(c, http.StatusBadRequest, "only JPEG, PNG, GIF, WebP allowed")
		}

		src, err := file.Open()
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "failed to read file")
		}

		fileName := fmt.Sprintf("%s_%s%s", userID, uuid.New().String(), util.GetFileExt(file.Filename))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		coverURL, err := firebase.UploadUserCover(ctx, userID, src, fileName)
		if err != nil {
			log.Printf("Firebase upload error: %v", err)
			return util.RespondError(c, http.StatusInternalServerError, "failed to upload cover")
		}

		updateQuery := `UPDATE users SET cover_url = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		_, err = db.Exec(updateQuery, coverURL, userID)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "failed to update user profile")
		}

		return c.JSON(http.StatusOK, map[string]string{
			"message":   "Cover image uploaded successfully",
			"cover_url": coverURL,
		})
	}
}
