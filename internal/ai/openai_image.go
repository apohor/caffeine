package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIImageRequest configures a single OpenAI image-generation call. The
// provider uses the /v1/images/generations surface, which accepts a plain
// text prompt and returns base64-encoded PNG bytes.
//
// Docs: https://platform.openai.com/docs/api-reference/images/create
type OpenAIImageRequest struct {
	APIKey string
	Model  string // default "gpt-image-1"
	Prompt string
	Size   string        // e.g. "1024x1024" (default), "1024x1536", "1536x1024"
	Base   string        // default https://api.openai.com/v1
	Timeout time.Duration
}

const defaultOpenAIImageBase = "https://api.openai.com/v1"

// GenerateImageOpenAI calls the OpenAI images.generations endpoint.
func GenerateImageOpenAI(ctx context.Context, req OpenAIImageRequest) (*GeneratedImage, error) {
	if req.APIKey == "" {
		return nil, fmt.Errorf("openai image: api key is required")
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("openai image: prompt is required")
	}
	if req.Model == "" {
		req.Model = "gpt-image-1"
	}
	if req.Size == "" {
		req.Size = "1024x1024"
	}
	if req.Base == "" {
		req.Base = defaultOpenAIImageBase
	}
	if req.Timeout <= 0 {
		req.Timeout = 2 * time.Minute
	}

	body := map[string]any{
		"model":  req.Model,
		"prompt": req.Prompt,
		"size":   req.Size,
		"n":      1,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	endpoint := req.Base + "/images/generations"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)

	client := &http.Client{Timeout: req.Timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai image: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		preview := respBody
		if len(preview) > 500 {
			preview = preview[:500]
		}
		return nil, fmt.Errorf("openai image: http %d: %s", resp.StatusCode, string(preview))
	}

	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("openai image: empty response")
	}
	d := parsed.Data[0]
	if d.B64JSON == "" {
		return nil, fmt.Errorf("openai image: response did not include base64 data")
	}
	data, err := base64.StdEncoding.DecodeString(d.B64JSON)
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return &GeneratedImage{MimeType: "image/png", Data: data}, nil
}
