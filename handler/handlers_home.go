package handler

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"passly_backend/connpass"
	"passly_backend/util"
)

func GetPlazaCount(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		var totalUsers int
		if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&totalUsers); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to count users")
		}

		var todayEncounters int
		if err := db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE DATE(created_at AT TIME ZONE 'UTC' AT TIME ZONE 'Asia/Tokyo') = (CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Tokyo')::date`).Scan(&todayEncounters); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to count today encounters")
		}

		const dailyLimit = 5
		return c.JSON(http.StatusOK, map[string]interface{}{
			"count":      totalUsers,
			"todayCount": todayEncounters,
			"dailyLimit": dailyLimit,
		})
	}
}

func GetHome(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")

		// アプリを開くたびに呼ばれるので、ここで connpass 参加イベントを保存しておく。
		// ログインは JWT が切れたとき（72時間）しか起きないため、ログイン時だけでは足りない。
		// 3時間以内に取得済みなら connpass は呼ばない。
		connpass.SyncUserEventsInBackground(db, userID)

		type RecentEncounterJSON struct {
			UserID    string    `json:"user_id"`
			Name      string    `json:"name"`
			IconURL   string    `json:"icon_url"`
			MetAt     time.Time `json:"met_at"`
			EventName string    `json:"event_name"`
		}

		type HomeData struct {
			TotalEncounters  int                      `json:"total_encounters"`
			TodayEncounters  int                      `json:"today_encounters"`
			Unconfirmed      []map[string]interface{} `json:"unconfirmed"`
			RandomThree      []map[string]interface{} `json:"random_three"`
			RecentEncounters []RecentEncounterJSON    `json:"recent_encounters"`
		}
		var res HomeData

		if err := db.QueryRow(`
			SELECT COUNT(DISTINCT CASE WHEN my_id = $1 THEN target_id ELSE my_id END)
			FROM encounters
			WHERE my_id = $1 OR target_id = $1
		`, userID).Scan(&res.TotalEncounters); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to count total encounters")
		}
		if err := db.QueryRow(`
			SELECT COUNT(DISTINCT CASE WHEN my_id = $1 THEN target_id ELSE my_id END)
			FROM encounters
			WHERE (my_id = $1 OR target_id = $1)
			  AND DATE(created_at AT TIME ZONE 'UTC' AT TIME ZONE 'Asia/Tokyo') = (CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Tokyo')::date
		`, userID).Scan(&res.TodayEncounters); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to count today encounters")
		}

		rows, err := db.Query(`
            SELECT u.id, u.name, u.icon_url, u.one_word
            FROM encounters e
            JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
            WHERE (e.my_id = $1 OR e.target_id = $1)
              AND e.is_confirmed = FALSE`, userID)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query unconfirmed encounters")
		}
		defer rows.Close()

		for rows.Next() {
			var id, name, icon, word string
			if err := rows.Scan(&id, &name, &icon, &word); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to scan unconfirmed encounter record")
			}
			res.Unconfirmed = append(res.Unconfirmed, map[string]interface{}{
				"id": id, "name": name, "icon_url": icon, "one_word": word,
			})
		}

		loadRandomThree := func(query string, args ...interface{}) ([]map[string]interface{}, error) {
			randRows, err := db.Query(query, args...)
			if err != nil {
				return nil, err
			}
			defer randRows.Close()

			result := make([]map[string]interface{}, 0, 3)
			for randRows.Next() {
				var id, name, icon, word string
				if err := randRows.Scan(&id, &name, &icon, &word); err != nil {
					return nil, err
				}
				result = append(result, map[string]interface{}{
					"id": id, "name": name, "icon_url": icon, "one_word": word,
				})
			}
			if err := randRows.Err(); err != nil {
				return nil, err
			}
			return result, nil
		}

		res.RandomThree, err = loadRandomThree(`
            SELECT u.id, u.name, u.icon_url, u.one_word
						FROM encounters e
						JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
						WHERE (e.my_id = $1 OR e.target_id = $1)
							AND DATE(e.created_at AT TIME ZONE 'UTC' AT TIME ZONE 'Asia/Tokyo') = (CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Tokyo')::date
            ORDER BY RANDOM()
            LIMIT 3`, userID)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query random encounters")
		}

		if len(res.RandomThree) == 0 {
			res.RandomThree, err = loadRandomThree(`
                SELECT id, name, icon_url, one_word
                FROM (
                    SELECT DISTINCT ON (u.id)
                        u.id,
                        u.name,
                        COALESCE(u.icon_url, '') AS icon_url,
                        COALESCE(u.one_word, '') AS one_word,
                        e.created_at
                    FROM encounters e
					JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
					WHERE e.my_id = $1 OR e.target_id = $1
                    ORDER BY u.id, e.created_at DESC
                ) latest
                ORDER BY created_at DESC
                LIMIT 3`, userID)
			if err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to query fallback random encounters")
			}
		}

		// 「今まですれ違った人」(広場画面で使用): 相手ユーザー単位で重複排除し、
		// 直近のすれ違い日時・イベント名を添えて全件返す。
		recentRows, err := db.Query(`
			SELECT
				u.id,
				u.name,
				COALESCE(u.icon_url, '') AS icon_url,
				le.met_at,
				COALESCE(ev.name, '') AS event_name
			FROM (
				SELECT DISTINCT
					CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END AS other_id
				FROM encounters e
				WHERE e.my_id = $1 OR e.target_id = $1
			) counterparts
			JOIN users u ON u.id = counterparts.other_id
			JOIN LATERAL (
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
			log.Printf("Failed to query recent encounters (user_id=%s): %v", userID, err)
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query recent encounters")
		}
		defer recentRows.Close()

		res.RecentEncounters = make([]RecentEncounterJSON, 0)
		for recentRows.Next() {
			var item RecentEncounterJSON
			if err := recentRows.Scan(&item.UserID, &item.Name, &item.IconURL, &item.MetAt, &item.EventName); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to scan recent encounter record")
			}
			res.RecentEncounters = append(res.RecentEncounters, item)
		}
		if err := recentRows.Err(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to iterate recent encounters")
		}

		return c.JSON(http.StatusOK, res)
	}
}

func GetAchievements(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")

		type dayCount struct {
			Date  time.Time
			Count int
		}

		type achievementItem struct {
			ID          string  `json:"id"`
			Emoji       string  `json:"emoji"`
			Title       string  `json:"title"`
			Description string  `json:"description"`
			IsUnlocked  bool    `json:"is_unlocked"`
			UnlockedAt  *string `json:"unlocked_at"`
		}

		type achievementResponse struct {
			UnlockedCount int               `json:"unlocked_count"`
			TotalCount    int               `json:"total_count"`
			Achievements  []achievementItem `json:"achievements"`
		}

		ptrDate := func(t time.Time) *string {
			s := t.Format("2006-01-02")
			return &s
		}

		parseISODate := func(s string) (time.Time, error) {
			return time.Parse("2006-01-02", s)
		}

		dailyRows, err := db.Query(`
			SELECT DATE(created_at) AS d,
			       COUNT(DISTINCT CASE WHEN my_id = $1 THEN target_id ELSE my_id END) AS cnt
			FROM encounters
			WHERE my_id = $1 OR target_id = $1
			GROUP BY DATE(created_at)
			ORDER BY d ASC
		`, userID)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query daily encounter stats")
		}
		defer dailyRows.Close()

		dailyCounts := make([]dayCount, 0)
		for dailyRows.Next() {
			var d time.Time
			var cnt int
			if err := dailyRows.Scan(&d, &cnt); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to read daily encounter stats")
			}
			dailyCounts = append(dailyCounts, dayCount{Date: d, Count: cnt})
		}
		if err := dailyRows.Err(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to iterate daily encounter stats")
		}

		firstDateByThreshold := func(threshold int) *string {
			if threshold <= 0 {
				return nil
			}
			cumulative := 0
			for _, day := range dailyCounts {
				cumulative += day.Count
				if cumulative >= threshold {
					return ptrDate(day.Date)
				}
			}
			return nil
		}

		firstDateByDailyThreshold := func(threshold int) *string {
			for _, day := range dailyCounts {
				if day.Count >= threshold {
					return ptrDate(day.Date)
				}
			}
			return nil
		}

		firstDateByStreak := func(targetStreak int) *string {
			if targetStreak <= 0 || len(dailyCounts) == 0 {
				return nil
			}

			streak := 0
			var prev time.Time
			for _, day := range dailyCounts {
				if streak == 0 {
					streak = 1
				} else {
					diffDays := int(day.Date.Sub(prev).Hours() / 24)
					if diffDays == 1 {
						streak++
					} else {
						streak = 1
					}
				}

				if streak >= targetStreak {
					return ptrDate(day.Date)
				}
				prev = day.Date
			}

			return nil
		}

		roleRows, err := db.Query(`
			SELECT role, MIN(DATE(e.created_at))
			FROM encounters e
			JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
			WHERE (e.my_id = $1 OR e.target_id = $1)
			  AND u.role IN ('frontend', 'backend', 'fullstack')
			GROUP BY role
		`, userID)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query role encounters")
		}
		defer roleRows.Close()

		roleFirstDate := map[string]time.Time{}
		for roleRows.Next() {
			var role string
			var dateStr string
			if err := roleRows.Scan(&role, &dateStr); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to read role encounters")
			}
			d, err := parseISODate(dateStr)
			if err != nil {
				continue
			}
			roleFirstDate[role] = d
		}
		if err := roleRows.Err(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to iterate role encounters")
		}

		allRolesDate := func() *string {
			required := []string{"frontend", "backend", "fullstack"}
			latest := time.Time{}
			for _, role := range required {
				d, ok := roleFirstDate[role]
				if !ok {
					return nil
				}
				if d.After(latest) {
					latest = d
				}
			}
			if latest.IsZero() {
				return nil
			}
			return ptrDate(latest)
		}

		var firstEventDateStr sql.NullString
		if err := db.QueryRow(`
			SELECT TO_CHAR(MIN(DATE(created_at)), 'YYYY-MM-DD')
			FROM encounters
			WHERE (my_id = $1 OR target_id = $1)
			  AND event_id IS NOT NULL
		`, userID).Scan(&firstEventDateStr); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query event encounter date")
		}

		var eventDate *string
		if firstEventDateStr.Valid && firstEventDateStr.String != "" {
			s := firstEventDateStr.String
			eventDate = &s
		}

		achievements := []achievementItem{
			{
				ID:          "first_encounter",
				Emoji:       "🎉",
				Title:       "はじめてのすれ違い",
				Description: "初めて誰かとすれ違いました！",
				UnlockedAt:  firstDateByThreshold(1),
			},
			{
				ID:          "encounter_10",
				Emoji:       "👥",
				Title:       "10人達成",
				Description: "累計10人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(10),
			},
			{
				ID:          "encounter_50",
				Emoji:       "⭐",
				Title:       "50人達成",
				Description: "累計50人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(50),
			},
			{
				ID:          "encounter_100",
				Emoji:       "💯",
				Title:       "100人達成",
				Description: "累計100人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(100),
			},
			{
				ID:          "all_roles",
				Emoji:       "🎨",
				Title:       "オールロール",
				Description: "全ロールの人とすれ違いました！",
				UnlockedAt:  allRolesDate(),
			},
			{
				ID:          "daily_5",
				Emoji:       "⚡",
				Title:       "1日5人",
				Description: "1日で5人とすれ違いました！",
				UnlockedAt:  firstDateByDailyThreshold(5),
			},
			{
				ID:          "streak_3",
				Emoji:       "🔥",
				Title:       "3日連続",
				Description: "3日連続ですれ違いました！",
				UnlockedAt:  firstDateByStreak(3),
			},
			{
				ID:          "streak_7",
				Emoji:       "🏆",
				Title:       "7日連続",
				Description: "7日連続ですれ違いました！",
				UnlockedAt:  firstDateByStreak(7),
			},
			{
				ID:          "event_encounter",
				Emoji:       "🌟",
				Title:       "イベント参加",
				Description: "イベントに参加してすれ違いました！",
				UnlockedAt:  eventDate,
			},
			{
				ID:          "encounter_200",
				Emoji:       "🚀",
				Title:       "200人達成",
				Description: "累計200人とすれ違いました！",
				UnlockedAt:  firstDateByThreshold(200),
			},
		}

		unlockedCount := 0
		for i := range achievements {
			achievements[i].IsUnlocked = achievements[i].UnlockedAt != nil
			if achievements[i].IsUnlocked {
				unlockedCount++
			}
		}

		return c.JSON(http.StatusOK, achievementResponse{
			UnlockedCount: unlockedCount,
			TotalCount:    len(achievements),
			Achievements:  achievements,
		})
	}
}

// 1. 【復旧】正しい daily API (1日分のデータを返す)
// 理由: フロントエンドが現在このAPIを使ってカレンダーを描画しているため、まずは正しく動く状態に戻す。
func GetCalendarDaily(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		dateStr := c.QueryParam("date")
		if dateStr == "" {
			dateStr = time.Now().Format("2006-01-02")
		}

		type EncounterUser struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			IconURL   string `json:"icon_url"`
			OneWord   string `json:"one_word"`
			TechStack string `json:"tech_stack"`
		}

		type DailyData struct {
			EncounterCount int             `json:"encounter_count"`
			EventNames     []string        `json:"event_names"`
			EncounterUsers []EncounterUser `json:"encounter_users"`
		}
		var res DailyData

		// ホーム(today_encounters)と定義を揃えるため、レコード数ではなく
		// 相手の人数(DISTINCT)でカウントする。
		countQuery := `
			SELECT COUNT(DISTINCT CASE WHEN my_id = $1 THEN target_id ELSE my_id END)
			FROM encounters
			WHERE (my_id = $1 OR target_id = $1)
			  AND DATE(created_at AT TIME ZONE 'UTC' AT TIME ZONE 'Asia/Tokyo') = $2::date
		`
		if err := db.QueryRow(countQuery, userID, dateStr).Scan(&res.EncounterCount); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to count encounters")
		}

		eventQuery := `SELECT name FROM events WHERE DATE(start_at) = $1`
		rows, err := db.Query(eventQuery, dateStr)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query events")
		}
		defer rows.Close()

		for rows.Next() {
			var eventName string
			if err := rows.Scan(&eventName); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to read event rows")
			}
			res.EventNames = append(res.EventNames, eventName)
		}

		usersQuery := `
			SELECT id, name, icon_url, one_word, tech_stack
			FROM (
				SELECT DISTINCT ON (u.id)
					u.id,
					u.name,
					COALESCE(u.icon_url, '') AS icon_url,
					COALESCE(u.one_word, '') AS one_word,
					COALESCE(u.tech_stack, '') AS tech_stack,
					e.created_at
				FROM encounters e
				JOIN users u ON u.id = CASE WHEN e.my_id = $1 THEN e.target_id ELSE e.my_id END
				WHERE (e.my_id = $1 OR e.target_id = $1)
					AND DATE(e.created_at AT TIME ZONE 'UTC' AT TIME ZONE 'Asia/Tokyo') = $2::date
				ORDER BY u.id, e.created_at DESC
			) latest
			ORDER BY created_at DESC
		`

		userRows, err := db.Query(usersQuery, userID, dateStr)
		if err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to query encounter users")
		}
		defer userRows.Close()

		for userRows.Next() {
			var user EncounterUser
			if err := userRows.Scan(&user.ID, &user.Name, &user.IconURL, &user.OneWord, &user.TechStack); err != nil {
				return util.RespondError(c, http.StatusInternalServerError, "Failed to read encounter users")
			}
			res.EncounterUsers = append(res.EncounterUsers, user)
		}

		if err := userRows.Err(); err != nil {
			return util.RespondError(c, http.StatusInternalServerError, "Failed to iterate encounter users")
		}

		return c.JSON(http.StatusOK, res)
	}
}

// 2. 【新規】正しい monthly API (1ヶ月分の人数を一括で返す)
// 理由: フロントエンドが「1ヶ月分のAPI」に切り替えて、カレンダーの描画を爆速化（サーバー負荷軽減）できるようにするため。タイポ(wncountters)も修正済み。
func GetCalendarMonthly(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		userID := c.Param("id")
		yearMonth := c.QueryParam("month")
		if yearMonth == "" {
			yearMonth = time.Now().Format("2006-01")
		}

		query := `
            SELECT DATE(created_at) as date, COUNT(*) as count
            FROM encounters
			WHERE (my_id = $1 OR target_id = $1) AND TO_CHAR(created_at, 'YYYY-MM') = $2
            GROUP BY DATE(created_at)
        `
		rows, err := db.Query(query, userID, yearMonth)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to fetch monthly data"})
		}
		defer rows.Close()

		monthlyData := make(map[string]int)
		for rows.Next() {
			var date time.Time
			var count int
			if err := rows.Scan(&date, &count); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to scan row"})
			}
			monthlyData[date.Format("2006-01-02")] = count
		}

		return c.JSON(http.StatusOK, monthlyData)
	}
}
