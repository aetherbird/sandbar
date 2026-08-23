package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// VisionAnalyze sends an image to a vision-capable model and returns analysis.
func VisionAnalyze(ctx context.Context, args map[string]interface{}) (string, error) {
	imagePath, _ := args["image_path"].(string)
	if imagePath == "" {
		return "", fmt.Errorf("image_path is required")
	}

	question, _ := args["question"].(string)
	if question == "" {
		question = "Describe this image in detail."
	}

	data, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("read image: %w", err)
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	mime := detectMIME(imagePath)

	apiKey, _ := args["openrouter_api_key"].(string)
	if apiKey == "" {
		return "", fmt.Errorf("vision analysis requires an OpenRouter provider in config — add one or set its api_key")
	}

	// Use gemini-2.5-flash as default vision model (fast, cheap, good vision).
	visionModel := "google/gemini-2.5-flash"
	if v, ok := args["model"].(string); ok && v != "" {
		visionModel = v
	}

	payload := map[string]interface{}{
		"model": visionModel,
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": question,
					},
					{
						"type": "image_url",
						"image_url": map[string]string{
							"url": "data:" + mime + ";base64," + b64,
						},
					},
				},
			},
		},
		"max_tokens": 1024,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vision API: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		trunc := 300
		if len(respBody) < trunc {
			trunc = len(respBody)
		}
		return "", fmt.Errorf("vision API error %d: %s", resp.StatusCode, string(respBody[:trunc]))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(result.Choices) == 0 || result.Choices[0].Message.Content == "" {
		return "[Vision: empty response from model]", nil
	}

	return result.Choices[0].Message.Content, nil
}

func detectMIME(path string) string {
	ext := strings.ToLower(path[strings.LastIndex(path, ".")+1:])
	switch ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	default:
		return "application/octet-stream"
	}
}
