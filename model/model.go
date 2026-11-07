package model

import ()

// --- 構造体定義 (Models) ---

type User struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Email            string `json:"email"`
	Password         string `json:"password,omitempty"`
	IconURL          string `json:"icon_url"`
	CoverURL         string `json:"cover_url"`
	OneWord          string `json:"one_word"`
	Role             string `json:"role"`
	TechStack        string `json:"tech_stack"`
	TwitterURL       string `json:"twitter_url"`
	GitHubURL        string `json:"github_url"`
	PortfolioURL     string `json:"portfolio_url"`
	Affiliation      string `json:"affiliation"`
	About            string `json:"about"`
	ConnpassUsername string `json:"connpass_username"`
}

type Encounter struct {
	ID          int    `json:"id"`
	MyID        string `json:"my_id"`
	TargetID    string `json:"target_id"`
	EventID     *int   `json:"event_id"`
	IsConfirmed bool   `json:"is_confirmed"`
	CreatedAt   string `json:"created_at"`
}
