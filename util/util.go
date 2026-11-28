package util

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

func RespondError(c echo.Context, status int, message string) error {
	return c.JSON(status, map[string]string{"error": message})
}

func GetFileExt(filename string) string {
	parts := strings.Split(filename, ".")
	if len(parts) > 1 {
		return "." + parts[len(parts)-1]
	}
	return ".jpg"
}

func GenerateEphemeralToken() (string, error) {
	// 8byteを16桁hex化。BLE広告名(SP_ + token)でも20文字以内に収まる。
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// IsEncounterEventWindowRequired は、すれ違いを connpass イベントの開催時間内に限定するかを返す。
// 既定は true。ローカル検証などで外したいときだけ ENCOUNTER_REQUIRE_EVENT=false にする。
func IsEncounterEventWindowRequired() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("ENCOUNTER_REQUIRE_EVENT")))
	return !(raw == "0" || raw == "false" || raw == "no" || raw == "off")
}

func GetEncounterCooldownSeconds() int {
	// ハッカソン検証中は短めにして、実機テストで件数変化を確認しやすくする
	const defaultSeconds = 10
	raw := strings.TrimSpace(os.Getenv("ENCOUNTER_COOLDOWN_SECONDS"))
	if raw == "" {
		return defaultSeconds
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return defaultSeconds
	}
	return parsed
}
