package connpass

import "testing"

func TestExtractOGImage(t *testing.T) {
	cases := []struct {
		name string
		page string
		want string
	}{
		{
			"property が先",
			`<head><meta property="og:image" content="https://media.connpass.com/thumbs/a.png" /></head>`,
			"https://media.connpass.com/thumbs/a.png",
		},
		{
			"content が先・単引用符",
			`<meta content='https://media.connpass.com/thumbs/b.png' property='og:image'>`,
			"https://media.connpass.com/thumbs/b.png",
		},
		{
			"HTMLエスケープを戻す",
			`<meta property="og:image" content="https://example.com/a.png?x=1&amp;y=2">`,
			"https://example.com/a.png?x=1&y=2",
		},
		{
			"og:image:width などは無視する",
			`<meta property="og:image:width" content="1200"><meta property="og:image" content="https://example.com/c.png">`,
			"https://example.com/c.png",
		},
		{
			"https でなければ使わない",
			`<meta property="og:image" content="http://example.com/d.png">`,
			"",
		},
		{
			"og:image がない",
			`<meta property="og:title" content="タイトル">`,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractOGImage(tc.page); got != tc.want {
				t.Errorf("extractOGImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsConnpassHost(t *testing.T) {
	for host, want := range map[string]bool{
		"connpass.com":           true,
		"gdg-tokyo.connpass.com": true,
		"evilconnpass.com":       false,
		"connpass.com.evil.jp":   false,
	} {
		if got := isConnpassHost(host); got != want {
			t.Errorf("isConnpassHost(%q) = %v, want %v", host, got, want)
		}
	}
}
