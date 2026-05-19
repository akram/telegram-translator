package translator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Translator struct {
	apiKey  string
	baseURL string
	client  *http.Client
	cache   *Cache
}

func NewTranslator(apiKey, baseURL string, cache *Cache) *Translator {
	return &Translator{
		apiKey:  apiKey,
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		cache: cache,
	}
}

type translateRequest struct {
	Text       []string `json:"text"`
	SourceLang string   `json:"source_lang,omitempty"`
	TargetLang string   `json:"target_lang"`
}

type translateResponse struct {
	Translations []struct {
		Text string `json:"text"`
	} `json:"translations"`
}

func (t *Translator) Translate(ctx context.Context, text, sourceLang, targetLang string) (string, error) {
	if text == "" {
		return "", nil
	}

	// Check cache first
	if t.cache != nil {
		if cached, ok := t.cache.Get(text, sourceLang, targetLang); ok {
			return cached, nil
		}
	}

	reqBody := translateRequest{
		Text:       []string{text},
		SourceLang: sourceLang,
		TargetLang: targetLang,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/v2/translate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("DeepL rate limit exceeded")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DeepL API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result translateResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}

	if len(result.Translations) == 0 {
		return "", fmt.Errorf("no translations returned")
	}

	translated := result.Translations[0].Text

	// Store in cache
	if t.cache != nil {
		t.cache.Put(text, sourceLang, targetLang, translated)
	}

	return translated, nil
}

type usageResponse struct {
	CharacterCount int64 `json:"character_count"`
	CharacterLimit int64 `json:"character_limit"`
}

func (t *Translator) Usage(ctx context.Context) (used, limit int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/v2/usage", nil)
	if err != nil {
		return 0, 0, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+t.apiKey)

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("DeepL API error (status %d)", resp.StatusCode)
	}

	var result usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, fmt.Errorf("parsing response: %w", err)
	}

	return result.CharacterCount, result.CharacterLimit, nil
}
