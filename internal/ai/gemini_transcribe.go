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

// TranscribeGeminiRequest configures a single speech-to-text call against
// Gemini's generateContent surface. Gemini accepts audio as an inlineData
// part alongside a text instruction; we ask the model to return nothing
// but the transcript so callers can store it verbatim.
type TranscribeGeminiRequest struct {
	APIKey   string
	Model    string        // default "gemini-2.5-flash"
	Audio    []byte        // raw audio bytes (20 MB inline cap)
	MIME     string        // e.g. "audio/webm", "audio/mp4", "audio/ogg"
	Endpoint string        // default https://generativelanguage.googleapis.com/v1beta
	Timeout  time.Duration // default 2m
	// Prompt overrides the default "transcribe this audio" instruction.
	Prompt string
}

// TranscribeGemini uploads the audio inline and returns the model's
// transcription. Gemini is more permissive than Whisper about content
// (multilingual, can handle overlapping speech, accepts long prompts)
// but is capped at ~20 MB of inline data per request.
func TranscribeGemini(ctx context.Context, req TranscribeGeminiRequest) (*Transcription, error) {
	if req.APIKey == "" {
		return nil, fmt.Errorf("gemini transcribe: api key is required")
	}
	if len(req.Audio) == 0 {
		return nil, fmt.Errorf("gemini transcribe: audio is empty")
	}
	if req.Model == "" {
		req.Model = "gemini-2.5-flash"
	}
	if req.Endpoint == "" {
		req.Endpoint = defaultGeminiBase
	}
	if req.Timeout <= 0 {
		req.Timeout = 2 * time.Minute
	}
	if req.MIME == "" {
		req.MIME = "audio/webm"
	}
	// Gemini's inlineData mime accepts the common containers but
	// MediaRecorder sometimes tacks on a codecs=... suffix. Strip it so
	// the server sees a pure top-level mime.
	mime := req.MIME
	if i := indexOfSemicolon(mime); i >= 0 {
		mime = mime[:i]
	}
	instruction := req.Prompt
	if instruction == "" {
		instruction = "Transcribe the following audio verbatim. Output only the transcript — no preamble, labels, or formatting. If the audio is silent, output an empty string."
	}

	body := map[string]any{
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []any{
					map[string]string{"text": instruction},
					map[string]any{
						"inlineData": map[string]string{
							"mimeType": mime,
							"data":     base64.StdEncoding.EncodeToString(req.Audio),
						},
					},
				},
			},
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
		return nil, fmt.Errorf("gemini transcribe: %w", err)
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
		return nil, fmt.Errorf("gemini transcribe: http %d: %s", resp.StatusCode, string(preview))
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini transcribe: blocked: %s", parsed.PromptFeedback.BlockReason)
	}
	var text string
	for _, c := range parsed.Candidates {
		for _, p := range c.Content.Parts {
			text += p.Text
		}
	}
	return &Transcription{Text: text}, nil
}

func indexOfSemicolon(s string) int {
	for i, c := range s {
		if c == ';' {
			return i
		}
	}
	return -1
}
