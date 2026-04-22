package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/apohor/caffeine/internal/ai"
	"github.com/apohor/caffeine/internal/profileimages"
	"github.com/apohor/caffeine/internal/settings"
	"github.com/go-chi/chi/v5"
)

// handleProfileImageGet serves a previously-generated AI image for a profile.
// If none exists we return 404 so the SPA can fall back to the machine's
// built-in artwork.
func handleProfileImageGet(store *profileimages.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		img, err := store.Get(id)
		if err != nil {
			if err == profileimages.ErrNotFound {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", img.MimeType)
		// Short cache with ETag-ish mtime; browser will re-request after
		// Generate, which replaces the file.
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.Header().Set("Last-Modified", img.Modified.UTC().Format(http.TimeFormat))
		_, _ = w.Write(img.Data)
	}
}

// handleProfileImageDelete removes a stored image. Returns 204 regardless
// of whether one existed — idempotent.
func handleProfileImageDelete(store *profileimages.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		if err := store.Delete(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleProfileImageList returns the set of profile ids with AI-generated
// images so the list page can render a badge without N round-trips.
func handleProfileImageList(store *profileimages.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ids, err := store.List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
	}
}

// handleProfileImageUpload accepts a user-supplied image and stores it
// as the profile's artwork, treating it identically to an AI-generated
// image downstream (same Store, same list endpoint, same "AI" badge —
// the source is opaque to the list). Accepts either:
//   - multipart/form-data with a single "image" file field, or
//   - a raw image body with Content-Type set to an image/* mime.
//
// Max size is 10 MB; mime must be one of png/jpeg/webp/gif.
func handleProfileImageUpload(store *profileimages.Store) http.HandlerFunc {
	const maxBytes = 10 << 20
	allowed := map[string]bool{
		"image/png":  true,
		"image/jpeg": true,
		"image/jpg":  true,
		"image/webp": true,
		"image/gif":  true,
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing profile id"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

		var (
			data []byte
			mime string
			err  error
		)
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "multipart/") {
			if err := r.ParseMultipartForm(maxBytes); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upload too large or malformed", "detail": err.Error()})
				return
			}
			file, hdr, ferr := r.FormFile("image")
			if ferr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing form field 'image'"})
				return
			}
			defer file.Close()
			data, err = io.ReadAll(file)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read upload", "detail": err.Error()})
				return
			}
			mime = hdr.Header.Get("Content-Type")
		} else {
			data, err = io.ReadAll(r.Body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body", "detail": err.Error()})
				return
			}
			mime = ct
		}

		if len(data) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty image"})
			return
		}
		// Fall back to sniffing if the client didn't label it.
		if mime == "" || !allowed[strings.ToLower(mime)] {
			sniffed := http.DetectContentType(data)
			if !allowed[sniffed] {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
					"error":  "unsupported image type",
					"detail": fmt.Sprintf("got %q, sniffed %q; allowed: png, jpeg, webp, gif", mime, sniffed),
				})
				return
			}
			mime = sniffed
		}

		if err := store.Put(id, mime, data); err != nil {
			slog.Warn("profile image: upload store", "id", id, "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		slog.Info("profile image uploaded", "id", id, "bytes", len(data), "mime", mime)
		writeJSON(w, http.StatusOK, map[string]any{
			"url":   "/api/profiles/" + id + "/image",
			"bytes": len(data),
			"mime":  mime,
		})
	}
}

// generateImageRequest is the JSON body accepted by the generate endpoint.
// An empty prompt means "auto-build one from the profile". A non-empty
// prompt overrides the default for artistic control.
type generateImageRequest struct {
	Prompt string `json:"prompt,omitempty"`
}

// handleProfileImageGenerate calls Gemini's image model and persists the
// result as the profile's new artwork. It works even when AI text analysis
// is configured to use a different provider, provided a Gemini key is
// saved in settings.
func handleProfileImageGenerate(
	machineURL string,
	store *profileimages.Store,
	aiSettings *settings.Manager,
) http.HandlerFunc {
	httpClient := &http.Client{Timeout: 15 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing profile id"})
			return
		}
		if aiSettings == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "AI is not configured"})
			return
		}
		provider, apiKey, imageModel := aiSettings.ImageCreds()
		if provider == "" || apiKey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "no image-capable AI provider configured — add an OpenAI or Gemini key in Settings → AI",
			})
			return
		}

		var body generateImageRequest
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
		}

		profile, err := fetchMachineProfile(r.Context(), httpClient, machineURL, id)
		if err != nil {
			slog.Warn("profile image: fetch profile", "id", id, "err", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  "could not read profile from machine",
				"detail": err.Error(),
			})
			return
		}
		prompt := body.Prompt
		if prompt == "" {
			prompt = buildProfileImagePrompt(profile)
		}

		// Image generation can take 20-90s depending on provider. Use a
		// dedicated context rather than inheriting the 30s chi route
		// timeout; the handler group below mounts this route with a
		// 5-minute timeout too.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()

		var img *ai.GeneratedImage
		switch provider {
		case settings.ImageProviderGemini:
			img, err = ai.GenerateImage(ctx, ai.GenerateImageRequest{
				APIKey: apiKey,
				Model:  imageModel,
				Prompt: prompt,
			})
		case settings.ImageProviderOpenAI:
			img, err = ai.GenerateImageOpenAI(ctx, ai.OpenAIImageRequest{
				APIKey: apiKey,
				Model:  imageModel,
				Prompt: prompt,
			})
		default:
			err = fmt.Errorf("unsupported image provider %q", provider)
		}
		if err != nil {
			slog.Warn("profile image: generate",
				"id", id, "provider", provider, "model", imageModel, "err", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  "image generation failed",
				"detail": err.Error(),
			})
			return
		}
		if err := store.Put(id, img.MimeType, img.Data); err != nil {
			slog.Warn("profile image: store", "id", id, "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		slog.Info("profile image generated",
			"id", id,
			"name", profile.Name,
			"bytes", len(img.Data),
			"mime", img.MimeType,
			"provider", provider,
			"model", imageModel,
		)
		writeJSON(w, http.StatusOK, map[string]any{
			"url":      "/api/profiles/" + id + "/image",
			"bytes":    len(img.Data),
			"mime":     img.MimeType,
			"provider": provider,
			"model":    imageModel,
			"prompt":   prompt,
		})
	}
}

// machineProfile captures the handful of fields we need from the machine's
// profile/get response to build a prompt. The schema is wider; we ignore
// the rest.
type machineProfile struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Author      string  `json:"author"`
	Temperature float64 `json:"temperature"`
	FinalWeight float64 `json:"final_weight"`
	Display     struct {
		AccentColor string `json:"accentColor"`
	} `json:"display"`
	Variables []struct {
		Name  string      `json:"name"`
		Key   string      `json:"key"`
		Type  string      `json:"type"`
		Value interface{} `json:"value"`
	} `json:"variables"`
}

func fetchMachineProfile(ctx context.Context, client *http.Client, base, id string) (*machineProfile, error) {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return nil, fmt.Errorf("machine url not configured")
	}
	endpoint := fmt.Sprintf("%s/api/v1/profile/get/%s", base, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("machine http %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	var p machineProfile
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &p, nil
}

// buildProfileImagePrompt crafts a Nano-Banana-friendly description from the
// profile metadata. Keeps the prompt short and visual; the model doesn't
// care about extraction mechanics.
func buildProfileImagePrompt(p *machineProfile) string {
	var b strings.Builder
	b.WriteString("A richly detailed, minimalist square illustration for an espresso profile card")
	if p.Name != "" {
		fmt.Fprintf(&b, " titled %q", p.Name)
	}
	if p.Author != "" {
		fmt.Fprintf(&b, " by %s", p.Author)
	}
	b.WriteString(". ")
	if p.Display.AccentColor != "" {
		fmt.Fprintf(&b, "Dominant accent color %s. ", p.Display.AccentColor)
	}
	if p.Temperature > 0 {
		fmt.Fprintf(&b, "Brewing at %.0f°C. ", p.Temperature)
	}
	if p.FinalWeight > 0 {
		fmt.Fprintf(&b, "Final yield %.0fg. ", p.FinalWeight)
	}
	if len(p.Variables) > 0 {
		b.WriteString("Key variables: ")
		for i, v := range p.Variables {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%v", v.Name, v.Value)
		}
		b.WriteString(". ")
	}
	b.WriteString("Warm coffee palette, subtle textures, centered composition, no text, no logos, no watermarks.")
	return b.String()
}
