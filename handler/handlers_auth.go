package handler

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"

	"passly_backend/auth"
	"passly_backend/model"
	"passly_backend/util"
)

func Signup(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		var u model.User
		if err := c.Bind(&u); err != nil {
			return util.RespondError(c, http.StatusBadRequest, "Invalid request format")
		}
		if u.ID == "" || u.Name == "" || u.Email == "" || u.Password == "" {
			return util.RespondError(c, http.StatusBadRequest, "id, name, email, and password are required")
		}

		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to hash password")
		}

		query := `
		INSERT INTO users (id, name, email, password_hash, icon_url, one_word, role, tech_stack, twitter_url, github_url, portfolio_url, affiliation, connpass_username)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

		_, err = db.Exec(query, u.ID, u.Name, u.Email, string(hashedPassword), u.IconURL, u.OneWord, u.Role, u.TechStack, u.TwitterURL, u.GitHubURL, u.PortfolioURL, u.Affiliation, u.ConnpassUsername)
		if err != nil {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				return util.RespondError(c, http.StatusConflict, "User ID or Email already exists")
			}
			return util.RespondError(c, http.StatusInternalServerError, "Failed to insert user")
		}

		return c.JSON(http.StatusCreated, map[string]string{"message": "User registered successfully"})
	}
}

func Login(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
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

		// 単一セッション: ログインのたびに token_version を+1し、以前のトークンを無効化する。
		var tokenVersion int
		if verr := db.QueryRow(
			`UPDATE users SET token_version = token_version + 1 WHERE id = $1 RETURNING token_version`,
			userID,
		).Scan(&tokenVersion); verr != nil {
			log.Printf("token_version bump error user_id=%s err=%v", userID, verr)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to update session"})
		}

		token, err := auth.GenerateJWT(userID, tokenVersion)
		if err != nil {
			log.Printf("jwt generate error user_id=%s err=%v", userID, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to issue token"})
		}

		return c.JSON(http.StatusOK, map[string]string{
			"message": "Login successful",
			"user_id": userID,
			"token":   token,
		})
	}
}
