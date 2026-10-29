package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/labstack/echo/v4"

	"passly_backend/util"
)

// --- 認証(JWT) ---
// POST /login 成功時にuser_idをclaimに含めて署名発行し、なりすまし被害の大きい
// エンドポイント(プロフィール編集・削除・アバター変更)に限定して検証を必須にする。
// すれ違い記録系のうち POST /encounters は、実機で検証済みのBLE検知〜記録フローを
// フロント側のトークン送信対応が終わるまで壊さないため、今回はあえて対象外としている
// (段階的に広げる想定。8章のTODO参照)。
// POST /users/:id/encounters/batch(オフライン一括送信)はフロント未実装でまだ
// 実際のトラフィックが無いため、リスクなく先行してJWT必須化している(my_idの
// なりすまし防止、2026-09-18)。

func getJWTSecret() []byte {
	secret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if secret == "" {
		// 未設定時の開発用フォールバック。本番(Render)では必ずJWT_SECRETを設定すること。
		secret = "insecure-dev-secret-change-me"
	}
	return []byte(secret)
}

func getJWTExpiry() time.Duration {
	// 展示当日+準備日を見込んで長めの単一トークン・リフレッシュトークン無しの方式。
	const defaultHours = 72
	raw := strings.TrimSpace(os.Getenv("JWT_EXPIRY_HOURS"))
	if raw == "" {
		return defaultHours * time.Hour
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return defaultHours * time.Hour
	}
	return time.Duration(parsed) * time.Hour
}

type jwtClaims struct {
	UserID       string `json:"user_id"`
	TokenVersion int    `json:"tv"`
	jwt.RegisteredClaims
}

func GenerateJWT(userID string, tokenVersion int) (string, error) {
	now := time.Now()
	claims := jwtClaims{
		UserID:       userID,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(getJWTExpiry())),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(getJWTSecret())
}

// JWTRequired は Authorization: Bearer <token> を検証し、成功時はリクエスト
// コンテキストに認証済みuser_idを "auth_user_id" として保存してから本来の
// ハンドラを呼び出す。
func JWTRequired(db *sql.DB, next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		authHeader := c.Request().Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) {
			return util.RespondError(c, http.StatusUnauthorized, "missing or invalid Authorization header")
		}
		tokenString := strings.TrimSpace(strings.TrimPrefix(authHeader, prefix))

		claims := &jwtClaims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return getJWTSecret(), nil
		})
		if err != nil || !token.Valid || claims.UserID == "" {
			return util.RespondError(c, http.StatusUnauthorized, "invalid or expired token")
		}

		// 単一セッション: 発行時の token_version が DB の現在値と一致するか確認する。
		// 新しいログインで DB 側が +1 されるため、古い端末のトークンはここで弾かれる
		// (401 → フロントの既存処理で自動ログアウト)。既存の tv 無しトークンは 0 として
		// 扱われ、DBの初期値 0 と一致するので再ログインするまでは従来どおり動作する。
		// DB参照に失敗した場合は可用性優先で通す(署名は検証済み)。
		var currentVersion int
		if verr := db.QueryRow(`SELECT token_version FROM users WHERE id = $1`, claims.UserID).Scan(&currentVersion); verr == nil {
			if currentVersion != claims.TokenVersion {
				return util.RespondError(c, http.StatusUnauthorized, "session superseded by a newer login")
			}
		}

		c.Set("auth_user_id", claims.UserID)
		return next(c)
	}
}

// RequireSelf は、パス/ボディで指定されたuser_idが認証済み本人と一致するかを
// 確認する。JWTRequired と組み合わせて使う(auth_user_idが未設定なら弾く)。
func RequireSelf(c echo.Context, targetUserID string) error {
	authUserID, _ := c.Get("auth_user_id").(string)
	if authUserID == "" || authUserID != targetUserID {
		return util.RespondError(c, http.StatusForbidden, "forbidden: token does not match requested user")
	}
	return nil
}

func ResolveUserIDFromToken(db *sql.DB, token string) (string, error) {
	if token == "" {
		return "", errors.New("token is required")
	}

	// 新方式: DB保存済みの短いトークンを優先解決
	var resolvedUserID string
	query := `
		SELECT user_id
		FROM ephemeral_tokens
		WHERE token = $1 AND expires_at > CURRENT_TIMESTAMP
		ORDER BY created_at DESC
		LIMIT 1
	`
	err := db.QueryRow(query, token).Scan(&resolvedUserID)
	if err == nil {
		return resolvedUserID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("failed to resolve token from db: %w", err)
	}

	return "", errors.New("invalid or expired token")
}

// ResolveUserIDFromTokenAt は ResolveUserIDFromToken と異なり、「現在時刻」ではなく
// 呼び出し側が指定した基準時刻(オフライン一括送信の場合はクライアント申告の検知時刻
// encountered_at)を基準にトークンの有効性を判定する。スマホ側の時計ズレを考慮し、
// 有効期間の前後に slop 分の許容誤差を持たせる。valid_from が設定されているトークン
// (事前配布APIで発行された分)は、その時刻より前は無効。未設定(従来のオンデマンド
// 発行分)なら開始時刻側の制約はかからない。
func ResolveUserIDFromTokenAt(db *sql.DB, token string, referenceTime time.Time, slop time.Duration) (string, error) {
	if token == "" {
		return "", errors.New("token is required")
	}

	var resolvedUserID string
	var expiresAt time.Time
	var validFrom sql.NullTime
	query := `
		SELECT user_id, expires_at, valid_from
		FROM ephemeral_tokens
		WHERE token = $1
		ORDER BY created_at DESC
		LIMIT 1
	`
	err := db.QueryRow(query, token).Scan(&resolvedUserID, &expiresAt, &validFrom)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("invalid or expired token")
		}
		return "", fmt.Errorf("failed to resolve token from db: %w", err)
	}

	if referenceTime.After(expiresAt.Add(slop)) {
		return "", errors.New("token had already expired at the reported time")
	}
	if validFrom.Valid && referenceTime.Before(validFrom.Time.Add(-slop)) {
		return "", errors.New("token was not yet valid at the reported time")
	}

	return resolvedUserID, nil
}
