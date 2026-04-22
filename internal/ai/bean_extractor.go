// Vision-based bean bag extractor.
//
// Given a photo of a coffee bag, ask an OpenAI vision model to read the
// label and extract the fields our Beans form wants (name, roaster,
// origin, process, roast level, roast date, notes). Returned as JSON
// the UI can splat straight into the form for the user to review and
// tweak before saving.
//
// Two providers are supported: OpenAI chat (inline image_url data URI)
// and Google Gemini generateContent (inline_data). The API handler
// prefers OpenAI when a key is configured and falls back to Gemini
// otherwise. Anthropic vision would follow the same pattern but isn't
// wired here yet.
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ExtractedBean mirrors the beans.Input struct, JSON-tagged to match.
// Every field is optional — the LLM might only be confident about the
// roaster's name and the roast date, and we want to surface whatever it
// found without forcing it to guess.
type ExtractedBean struct {
	Name       string `json:"name"`
	Roaster    string `json:"roaster"`
	Origin     string `json:"origin"`
	Process    string `json:"process"`
	RoastLevel string `json:"roast_level"`
	RoastDate  string `json:"roast_date"` // ISO yyyy-mm-dd
	Notes      string `json:"notes"`
	// Confidence is the model's self-reported confidence on a 0..1
	// scale. Handy for the UI to flag low-quality reads ("we couldn't
	// read much — please double-check").
	Confidence float64 `json:"confidence"`
}

// ExtractBeanRequest is the input bundle for a single extraction.
type ExtractBeanRequest struct {
	APIKey string
	Model  string // e.g. "gpt-4o-mini" — must be a vision-capable OpenAI model
	Image  []byte
	MIME   string // e.g. "image/jpeg", "image/png", "image/webp"
}

// ExtractBeanResponse bundles the parsed bean plus usage so the caller
// can record the ledger entry.
type ExtractBeanResponse struct {
	Bean  ExtractedBean
	Usage TokenUsage
}

const beanExtractorSystemPrompt = `You are reading the front of a coffee bag from a photograph.

Extract the following fields and return STRICT JSON with these keys:
- name        : product/blend name (e.g. "Monarch", "Honey Boo"). Not the roaster.
- roaster     : the roastery's name (e.g. "Onyx", "Sey", "La Cabra")
- origin      : country / region / farm / blend origin as printed. Short.
- process     : one of "washed", "natural", "honey", "anaerobic", "carbonic maceration", etc. Lowercase.
- roast_level : "light", "medium-light", "medium", "medium-dark", "dark". Lowercase.
- roast_date  : ISO yyyy-mm-dd if visible; empty string if not.
- notes       : the tasting notes printed on the bag, comma-separated. Short.
- confidence  : 0..1 float self-scoring how legible the label was.

Rules:
- Return ONLY valid JSON, no prose, no code fences.
- Leave a field as an empty string "" if you can't read it. Do NOT guess.
- If the photo is clearly not a coffee bag, return all empty strings and confidence 0.
- Keep each field short and suitable for form input (no multi-line values except notes).`

// ExtractBeanFromImage sends the image to OpenAI's chat endpoint using
// a vision-capable model and returns a parsed ExtractedBean plus usage.
func ExtractBeanFromImage(ctx context.Context, req ExtractBeanRequest) (*ExtractBeanResponse, error) {
	if req.APIKey == "" {
		return nil, fmt.Errorf("openai api key required")
	}
	if len(req.Image) == 0 {
		return nil, fmt.Errorf("empty image")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" || !looksVisionCapable(model) {
		// gpt-4o-mini is cheap and vision-capable; use it as the
		// default if the user's configured text model can't see.
		model = "gpt-4o-mini"
	}
	mime := req.MIME
	if mime == "" {
		mime = "image/jpeg"
	}

	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(req.Image)
	body := map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "system", "content": beanExtractorSystemPrompt},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Extract bean details from this coffee bag photo."},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]string{"url": dataURL},
					},
				},
			},
		},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, defaultOpenAIEndpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai vision: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		if len(raw) > 500 {
			raw = raw[:500]
		}
		return nil, fmt.Errorf("openai vision: http %d: %s", resp.StatusCode, string(raw))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode vision response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("openai vision: empty response")
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	// json_object mode should guarantee valid JSON, but strip fences
	// defensively for when models forget.
	content = stripJSONFences(content)

	var out ExtractedBean
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("parse extracted bean: %w (content=%q)", err, content)
	}
	// Clamp confidence to [0,1] so the UI can trust it.
	if out.Confidence < 0 {
		out.Confidence = 0
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	return &ExtractBeanResponse{
		Bean: out,
		Usage: TokenUsage{
			InputTokens:  parsed.Usage.PromptTokens,
			OutputTokens: parsed.Usage.CompletionTokens,
		},
	}, nil
}

// looksVisionCapable is a coarse allow-list so we don't accidentally
// send an image to a text-only model (e.g. gpt-3.5-turbo). The API
// would reject it anyway, but a pre-check produces a nicer error.
func looksVisionCapable(model string) bool {
	m := strings.ToLower(model)
	prefixes := []string{"gpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4", "chatgpt-4o"}
	for _, p := range prefixes {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// ExtractBeanRequestGemini is the Gemini-flavoured input bundle.
type ExtractBeanRequestGemini struct {
	APIKey   string
	Model    string // e.g. "gemini-2.5-flash" — any multimodal Gemini works
	Image    []byte
	MIME     string
	Endpoint string // optional override; defaults to the public v1beta endpoint
}

// ExtractBeanFromImageGemini sends the image to Gemini's generateContent
// endpoint as inline_data and asks for a strict JSON response. All
// Gemini 1.5+/2.x/2.5 models are multimodal, so we don't need a
// vision-capability allow-list — we just fall back to gemini-2.5-flash
// if the caller's configured model string is empty.
func ExtractBeanFromImageGemini(ctx context.Context, req ExtractBeanRequestGemini) (*ExtractBeanResponse, error) {
	if req.APIKey == "" {
		return nil, fmt.Errorf("gemini api key required")
	}
	if len(req.Image) == 0 {
		return nil, fmt.Errorf("empty image")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "gemini-2.5-flash"
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = defaultGeminiBase
	}
	mime := req.MIME
	if mime == "" {
		mime = "image/jpeg"
	}
	// Strip any `; codecs=...` suffix — Gemini rejects compound mimes.
	if i := indexOfSemicolon(mime); i >= 0 {
		mime = mime[:i]
	}

	body := map[string]any{
		"system_instruction": map[string]any{
			"parts": []map[string]string{{"text": beanExtractorSystemPrompt}},
		},
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []any{
					map[string]string{"text": "Extract bean details from this coffee bag photo."},
					map[string]any{
						"inlineData": map[string]string{
							"mimeType": mime,
							"data":     base64.StdEncoding.EncodeToString(req.Image),
						},
					},
				},
			},
		},
		"generationConfig": map[string]any{
			"temperature":      0,
			"responseMimeType": "application/json",
			"maxOutputTokens":  2048,
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/models/%s:generateContent", endpoint, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", req.APIKey)

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini vision: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		if len(raw) > 500 {
			raw = raw[:500]
		}
		return nil, fmt.Errorf("gemini vision: http %d: %s", resp.StatusCode, string(raw))
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode gemini response: %w", err)
	}
	if parsed.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini vision: blocked: %s", parsed.PromptFeedback.BlockReason)
	}
	var content strings.Builder
	for _, c := range parsed.Candidates {
		for _, p := range c.Content.Parts {
			content.WriteString(p.Text)
		}
	}
	text := stripJSONFences(strings.TrimSpace(content.String()))
	if text == "" {
		return nil, fmt.Errorf("gemini vision: empty response")
	}
	var out ExtractedBean
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("parse extracted bean: %w (content=%q)", err, text)
	}
	if out.Confidence < 0 {
		out.Confidence = 0
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	return &ExtractBeanResponse{
		Bean: out,
		Usage: TokenUsage{
			InputTokens:  parsed.UsageMetadata.PromptTokenCount,
			OutputTokens: parsed.UsageMetadata.CandidatesTokenCount,
		},
	}, nil
}
