package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebSearch performs a web search using Brave API or DuckDuckGo fallback.
func WebSearch(ctx context.Context, args map[string]interface{}) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}

	apiKey, _ := args["brave_api_key"].(string)
	if apiKey != "" {
		return braveSearch(ctx, query, apiKey)
	}
	return duckDuckGoSearch(ctx, query)
}

type braveResult struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func braveSearch(ctx context.Context, query, apiKey string) (string, error) {
	u := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query) + "&count=10"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Subscription-Token", apiKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		return "", fmt.Errorf("brave search error %d: %s", res.StatusCode, string(body))
	}

	var result braveResult
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return "", err
	}
	return formatResults(result.Web.Results), nil
}

type ddgoResult struct {
	Title   string
	URL     string
	Snippet string
}

func duckDuckGoSearch(ctx context.Context, query string) (string, error) {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sandbar/1.0)")

	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	results := parseDDGoHTML(string(body))
	return formatDDGoResults(results), nil
}

func parseDDGoHTML(html string) []ddgoResult {
	var results []ddgoResult
	// Very basic HTML parsing: look for result links and snippets.
	// DuckDuckGo HTML structure uses .result__a for links and .result__snippet for snippets.
	lines := strings.Split(html, "\n")
	var current ddgoResult
	inResult := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "result__a") && strings.Contains(line, "href=\"") {
			inResult = true
			current = ddgoResult{}
			start := strings.Index(line, "href=\"")
			if start != -1 {
				start += 6
				end := strings.Index(line[start:], "\"")
				if end != -1 {
					current.URL = line[start : start+end]
					// DuckDuckGo uses redirect URLs
					if strings.HasPrefix(current.URL, "//") {
						current.URL = "https:" + current.URL
					}
				}
			}
			// Extract title text
			if start := strings.Index(line, ">"); start != -1 {
				if end := strings.Index(line[start:], "<"); end != -1 {
					current.Title = strings.TrimSpace(line[start+1 : start+end])
					current.Title = strings.ReplaceAll(current.Title, "<b>", "")
					current.Title = strings.ReplaceAll(current.Title, "</b>", "")
				}
			}
		} else if strings.Contains(line, "result__snippet") && inResult {
			if start := strings.Index(line, ">"); start != -1 {
				if end := strings.Index(line[start:], "<"); end != -1 {
					current.Snippet = strings.TrimSpace(line[start+1 : start+end])
					current.Snippet = strings.ReplaceAll(current.Snippet, "<b>", "")
					current.Snippet = strings.ReplaceAll(current.Snippet, "</b>", "")
				}
			}
			results = append(results, current)
			inResult = false
			if len(results) >= 10 {
				break
			}
		}
	}
	return results
}

func formatResults(results interface{}) string {
	switch r := results.(type) {
	case []struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
	}:
		var sb strings.Builder
		for i, res := range r {
			if i >= 10 {
				break
			}
			sb.WriteString(fmt.Sprintf("%d. %s\n%s\n%s\n\n", i+1, res.Title, res.URL, res.Description))
		}
		return sb.String()
	case []ddgoResult:
		return formatDDGoResults(r)
	}
	return ""
}

func formatDDGoResults(results []ddgoResult) string {
	var sb strings.Builder
	for i, res := range results {
		if i >= 10 {
			break
		}
		sb.WriteString(fmt.Sprintf("%d. %s\n%s\n%s\n\n", i+1, res.Title, res.URL, res.Snippet))
	}
	return sb.String()
}
