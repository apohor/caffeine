package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"
)

// TranscribeOpenAIRequest configures a single Whisper-style speech-to-text
// call against OpenAI's /v1/audio/transcriptions endpoint.
type TranscribeOpenAIRequest struct {
	APIKey  string
	Model   string        // default "whisper-1"
	Audio   []byte        // raw audio bytes
	MIME    string        // e.g. "audio/webm", "audio/mp4"
	Base    string        // default https://api.openai.com/v1
	Timeout time.Duration // default 2m
	// Language is an optional ISO-639-1 hint (e.g. "en"). Leave empty to
	// let Whisper auto-detect.
	Language string
	// Prompt is an optional short hint that nudges the decoder (helpful
	// for domain words like "profile", "preinfusion"). Leave empty for
	// generic transcription.
	Prompt string
}

// Transcription is the decoded response from OpenAI's transcription API.
type Transcription struct {
	Text     string `json:"text"`
	Language string `json:"language,omitempty"`
}

// TranscribeOpenAI sends audio bytes as multipart/form-data and returns
// the transcribed text. Audio can be in any format the Whisper endpoint
// accepts (webm/opus, mp4/aac, mp3, wav, flac, ogg — up to 25 MB).
func TranscribeOpenAI(ctx context.Context, req TranscribeOpenAIRequest) (*Transcription, error) {
	if req.APIKey == "" {
		return nil, fmt.Errorf("openai transcribe: api key is required")
	}
	if len(req.Audio) == 0 {
		return nil, fmt.Errorf("openai transcribe: audio is empty")
	}
	if req.Model == "" {
		req.Model = "whisper-1"
	}
	if req.Base == "" {
		req.Base = defaultOpenAIImageBase
	}
	if req.Timeout <= 0 {
		req.Timeout = 2 * time.Minute
	}
	if req.MIME == "" {
		req.MIME = "audio/webm"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// File part. Whisper requires a filename with a recognised extension;
	// derive it from the MIME type so the server doesn't refuse the upload.
	filename := "audio" + extForAudio(req.MIME)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", req.MIME)
	fw, err := mw.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	if _, err := fw.Write(req.Audio); err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	// Simple string fields.
	for k, v := range map[string]string{
		"model":           req.Model,
		"response_format": "json",
	} {
		if err := mw.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	if req.Language != "" {
		if err := mw.WriteField("language", req.Language); err != nil {
			return nil, err
		}
	}
	if req.Prompt != "" {
		if err := mw.WriteField("prompt", req.Prompt); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	endpoint := req.Base + "/audio/transcriptions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)

	client := &http.Client{Timeout: req.Timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai transcribe: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		preview := body
		if len(preview) > 500 {
			preview = preview[:500]
		}
		return nil, fmt.Errorf("openai transcribe: http %d: %s", resp.StatusCode, string(preview))
	}

	var out Transcription
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// extForAudio maps common audio MIME types to the file extensions
// Whisper is known to accept. Falls back to ".webm" since browser
// MediaRecorder defaults to webm/opus.
func extForAudio(mime string) string {
	switch mime {
	case "audio/webm", "audio/webm;codecs=opus":
		return ".webm"
	case "audio/ogg", "audio/ogg;codecs=opus":
		return ".ogg"
	case "audio/mp4", "audio/aac", "audio/x-m4a":
		return ".m4a"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/flac":
		return ".flac"
	default:
		return ".webm"
	}
}
