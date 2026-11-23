package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// FreeDict reads the Free Dictionary API (https://dictionaryapi.dev): English definitions, examples,
// phonetics and audio.
type FreeDict struct {
	BaseURL string // https://api.dictionaryapi.dev/api/v2/entries/en
	get     *getter
}

// NewFreeDict builds the provider.
func NewFreeDict(baseURL string, client *http.Client, userAgent string) *FreeDict {
	return &FreeDict{BaseURL: strings.TrimRight(baseURL, "/"), get: newGetter(client, userAgent)}
}

// Limits of what is kept from an entry.
const (
	maxMeaningsPerPOS = 5
	maxMeanings       = 20
)

type fdEntry struct {
	Word      string `json:"word"`
	Phonetic  string `json:"phonetic"`
	Phonetics []struct {
		Text  string `json:"text"`
		Audio string `json:"audio"`
	} `json:"phonetics"`
	Meanings []struct {
		PartOfSpeech string `json:"partOfSpeech"`
		Definitions  []struct {
			Definition string `json:"definition"`
			Example    string `json:"example"`
		} `json:"definitions"`
	} `json:"meanings"`
}

// Lookup implements Provider.
func (p *FreeDict) Lookup(ctx context.Context, word string) (*Word, error) {
	body, status, err := p.get.get(ctx, p.BaseURL+"/"+url.PathEscape(word))
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusNotFound:
		return nil, ErrNotFound
	case status != http.StatusOK:
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, status)
	}
	return parseFreeDict(body)
}

func parseFreeDict(body []byte) (*Word, error) {
	var entries []fdEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		// an error document ({"title": "No Definitions Found"}) is an object, not a list
		return nil, ErrNotFound
	}
	if len(entries) == 0 {
		return nil, ErrNotFound
	}
	w := &Word{Text: entries[0].Word}
	seenPOS := map[string]bool{}
	for _, e := range entries {
		if w.Phonetic == "" {
			w.Phonetic = e.Phonetic
		}
		for _, ph := range e.Phonetics {
			if w.Phonetic == "" {
				w.Phonetic = ph.Text
			}
			switch accent(ph.Audio) {
			case "uk":
				setFirst(&w.AudioUK, ph.Audio)
				setFirst(&w.PhoneticUK, ph.Text)
			case "us":
				setFirst(&w.AudioUS, ph.Audio)
				setFirst(&w.PhoneticUS, ph.Text)
			}
		}
		for _, m := range e.Meanings {
			if pos := strings.TrimSpace(m.PartOfSpeech); pos != "" && !seenPOS[pos] {
				seenPOS[pos] = true
				w.PartOfSpeech = append(w.PartOfSpeech, pos)
			}
			for i, d := range m.Definitions {
				if i >= maxMeaningsPerPOS || len(w.Meanings) >= maxMeanings {
					break
				}
				def := strings.TrimSpace(d.Definition)
				if def == "" {
					continue
				}
				mean := Meaning{Definition: def}
				if ex := strings.TrimSpace(d.Example); ex != "" {
					mean.Examples = []string{ex}
				}
				w.Meanings = append(w.Meanings, mean)
			}
		}
	}
	if len(w.Meanings) == 0 {
		return nil, ErrNotFound
	}
	return w, nil
}

// accent tells the accent of an audio file from its name ("hello-uk.mp3", "hello--_gb_1.mp3").
func accent(audioURL string) string {
	a := strings.ToLower(audioURL)
	switch {
	case a == "":
		return ""
	case strings.Contains(a, "-uk.") || strings.Contains(a, "_gb_"):
		return "uk"
	case strings.Contains(a, "-us.") || strings.Contains(a, "_us_"):
		return "us"
	}
	return ""
}

func setFirst(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}
