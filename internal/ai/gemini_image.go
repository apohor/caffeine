package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// GenerateImageRequest configures a single Gemini image-generation call.
type GenerateImageRequest struct {
	// APIKey is required.
	APIKey string
	// Model defaults to "gemini-2.5-flash-image-preview" (Nano Banana).
	// The preview family returns one or more inline_data parts.
	Model string
	// Prompt is the human-readable instruction.
	Prompt string
	// Endpoint lets tests inject a fake server. Defaults to the official
	// v1beta base URL.
	Endpoint string
	// Timeout bounds the entire HTTP round-trip (including image encoding
	// on the server). Image generation is slower than text; 90s is safe.
	Timeout time.Duration
}

// GeneratedImage is the decoded binary payload returned by the API.
type GeneratedImage struct {
	MimeType string
	Data     []byte
}

// GenerateImage calls Gemini's image-capable model with a plain text prompt
// and returns the first inline_data part from the response. Gemini does not
// have a dedicated "/images" endpoint — image generation rides on the same
// generateContent surface and is enabled by the model choice plus the
// response_modalities hint.
func GenerateImage(ctx context.Context, req GenerateImageRequest) (*GeneratedImage, error) {
	if req.APIKey == "" {
		return nil, fmt.Errorf("gemini image: api key is required")
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("gemini image: prompt is required")
	}
	if req.Model == "" {
		req.Model = "gemini-2.5-flash-image"
	}
	if req.Endpoint == "" {
		req.Endpoint = defaultGeminiBase
	}
	if req.Timeout <= 0 {
		req.Timeout = 90 * time.Second
	}

	body := map[string]any{
		"contents": []map[string]any{
			{
				"role":  "user",
				"parts": []map[string]string{{"text": req.Prompt}},
			},
		},
		"generationConfig": map[string]any{
			// Image-capable models still require response_modalities so the
			// server knows we want the inline image payload back.
			"responseModalities": []string{"IMAGE", "TEXT"},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent",
		req.Endpoint, url.PathEscape(req.Model))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", req.APIKey)

	client := &http.Client{Timeout: req.Timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini image: %w", err)
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
		return nil, fmt.Errorf("gemini image: http %d: %s", resp.StatusCode, string(preview))
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback,omitempty"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.PromptFeedback != nil && parsed.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini image: blocked: %s", parsed.PromptFeedback.BlockReason)
	}
	for _, c := range parsed.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData == nil || p.InlineData.Data == "" {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("decode image: %w", err)
			}
			mime := p.InlineData.MimeType
			if mime == "" {
				mime = "image/png"
			}
			return &GeneratedImage{MimeType: mime, Data: data}, nil
		}
	}
	return nil, fmt.Errorf("gemini image: no image in response")
}
