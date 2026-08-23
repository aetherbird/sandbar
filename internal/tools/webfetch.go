package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	tagStrip   = regexp.MustCompile(`<[^>]*>`)
	spaceStrip = regexp.MustCompile(`\s+`)
)

// WebFetch retrieves and extracts text content from a URL.
func WebFetch(ctx context.Context, args map[string]interface{}) (string, error) {
	rawURL, _ := args["url"].(string)
	if rawURL == "" {
		return "", fmt.Errorf("url is required")
	}

	maxChars := 50000
	if v, ok := args["max_chars"]; ok {
		if n, ok := v.(float64); ok {
			maxChars = int(n)
		}
	}
	if maxChars < 1000 {
		maxChars = 1000
	}
	if maxChars > 200000 {
		maxChars = 200000
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sandbar/1.0)")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxChars*2)))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	text := extractText(string(body))
	if len(text) > maxChars {
		text = text[:maxChars]
		// Truncate at last complete UTF-8 boundary.
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}

	title := resp.Header.Get("X-Title")
	if title == "" {
		title = rawURL
	}

	return fmt.Sprintf("Title: %s\nURL: %s\n\n%s", title, rawURL, text), nil
}

func extractText(html string) string {
	// Remove scripts and styles.
	html = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(html, "")
	html = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(html, "")
	html = regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`).ReplaceAllString(html, "")

	// Try to extract main content first.
	main := ""
	for _, tag := range []string{"article", "main", `div[role="main"]`} {
		re := regexp.MustCompile(`(?is)<` + tag + `[^>]*>(.*?)</` + regexp.QuoteMeta(strings.SplitN(tag, "[", 2)[0]) + `>`)
		if m := re.FindStringSubmatch(html); len(m) > 1 {
			main = m[1]
			break
		}
	}
	if main == "" {
		main = html
	}

	// Strip tags and collapse whitespace.
	text := tagStrip.ReplaceAllString(main, " ")
	text = spaceStrip.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}
