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
	"path/filepath"
	"time"
)

// ImageGenerate creates an image from a text prompt using OpenRouter's image models.
func ImageGenerate(ctx context.Context, args map[string]interface{}) (string, error) {
	prompt, _ := args["prompt"].(string)
	if prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}

	apiKey, _ := args["openrouter_api_key"].(string)
	if apiKey == "" {
		return "", fmt.Errorf("image generation requires an OpenRouter provider in config — add one or set its api_key")
	}

	model := "google/gemini-2.5-flash-image"
	if v, ok := args["model"].(string); ok && v != "" {
		model = v
	}

	payload := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Generate an image: " + prompt},
				},
			},
		},
		"modalities": []string{"image", "text"},
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("image gen API: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("image gen error %d", resp.StatusCode)
	}

	// Parse response looking for image data.
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Images  []struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "[Image gen: empty response]", nil
	}

	msg := result.Choices[0].Message

	// Check for inline base64 images.
	if len(msg.Images) > 0 && msg.Images[0].ImageURL.URL != "" {
		imgURL := msg.Images[0].ImageURL.URL
		if len(imgURL) > 100 && imgURL[:11] == "data:image/" {
			comma := bytes.IndexByte([]byte(imgURL), ',')
			if comma < 0 {
				return "[Image generated but could not decode]", nil
			}
			imgData, err := base64.StdEncoding.DecodeString(imgURL[comma+1:])
			if err != nil {
				return "[Image generated but decode failed]", nil
			}

			absPath, err := saveGeneratedImage(resolveUploadsDir(ctx), imgData)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Image generated and saved to: %s", absPath), nil
		}
	}

	// Check for JSON-embedded image in content.
	if msg.Content != "" {
		return fmt.Sprintf("[Image generation response]\n%s", msg.Content[:500]), nil
	}

	return "[Image gen: no image in response]", nil
}

// resolveUploadsDir returns the generated-image output directory, jailed to
// the active workspace root. The request-scoped workspace injected by the
// agent takes precedence; the registry closure seeds the configured workspace
// as fallback when none is present — never the process CWD.
func resolveUploadsDir(ctx context.Context) string {
	workspace := WorkspaceFromContext(ctx)
	if workspace == "" {
		workspace = "."
	}
	return filepath.Join(workspace, "uploads")
}

// saveGeneratedImage writes decoded image bytes into uploadDir (created if
// needed) and returns the absolute path for the tool result.
func saveGeneratedImage(uploadDir string, imgData []byte) (string, error) {
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return "", fmt.Errorf("save image: create uploads dir: %w", err)
	}
	// UnixNano keeps rapid successive generations from overwriting each other.
	filename := fmt.Sprintf("generated_%d.png", time.Now().UnixNano())
	path := filepath.Join(uploadDir, filename)
	if err := os.WriteFile(path, imgData, 0644); err != nil {
		return "", fmt.Errorf("save image: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("save image: %w", err)
	}
	return absPath, nil
}
