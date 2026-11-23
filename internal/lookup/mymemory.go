package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// MyMemory translates English into Vietnamese with the MyMemory API (machine translation; the anonymous
// quota is about 5000 characters a day, so only the first definitions of a word are translated).
type MyMemory struct {
	BaseURL string // https://api.mymemory.translated.net/get
	get     *getter
}

// NewMyMemory builds the translator.
func NewMyMemory(baseURL string, client *http.Client, userAgent string) *MyMemory {
	return &MyMemory{BaseURL: baseURL, get: newGetter(client, userAgent)}
}

// maxTranslateRunes keeps a request under the 500 byte limit of the API for ordinary text.
const maxTranslateRunes = 200

// Translate implements Translator. It fails with ErrUnavailable when the quota is used up or the text
// came back untranslated.
func (m *MyMemory) Translate(ctx context.Context, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	if utf8.RuneCountInString(text) > maxTranslateRunes {
		text = string([]rune(text)[:maxTranslateRunes])
	}
	q := url.Values{"q": {text}, "langpair": {"en|vi"}}
	body, status, err := m.get.get(ctx, m.BaseURL+"?"+q.Encode())
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: translate status %d", ErrUnavailable, status)
	}
	var r struct {
		ResponseData struct {
			TranslatedText string `json:"translatedText"`
		} `json:"responseData"`
		QuotaFinished bool `json:"quotaFinished"`
		// a number (200) or a string ("403") depending on the answer
		ResponseStatus any `json:"responseStatus"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	out := strings.TrimSpace(r.ResponseData.TranslatedText)
	if r.QuotaFinished || fmt.Sprint(r.ResponseStatus) != "200" || out == "" ||
		strings.Contains(strings.ToUpper(out), "MYMEMORY WARNING") || strings.EqualFold(out, text) {
		return "", fmt.Errorf("%w: no translation", ErrUnavailable)
	}
	return out, nil
}
