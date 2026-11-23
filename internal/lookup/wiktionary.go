package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ViWiktionary reads the Vietnamese Wiktionary (MediaWiki API, plain text extract): the section
// "Tiếng Anh" of an English word has its IPA, parts of speech and Vietnamese definitions. Text is
// available under CC BY-SA.
type ViWiktionary struct {
	APIURL string // https://vi.wiktionary.org/w/api.php
	get    *getter
}

// NewViWiktionary builds the provider.
func NewViWiktionary(apiURL string, client *http.Client, userAgent string) *ViWiktionary {
	return &ViWiktionary{APIURL: apiURL, get: newGetter(client, userAgent)}
}

type wiktionaryResponse struct {
	Query struct {
		Pages []struct {
			Title   string `json:"title"`
			Missing bool   `json:"missing"`
			Extract string `json:"extract"`
		} `json:"pages"`
	} `json:"query"`
}

// Lookup implements Provider.
func (p *ViWiktionary) Lookup(ctx context.Context, word string) (*Word, error) {
	q := url.Values{
		"action": {"query"}, "prop": {"extracts"}, "explaintext": {"1"}, "redirects": {"1"},
		"titles": {word}, "format": {"json"}, "formatversion": {"2"},
	}
	body, status, err := p.get.get(ctx, p.APIURL+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, status)
	}
	var r wiktionaryResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(r.Query.Pages) == 0 || r.Query.Pages[0].Missing || r.Query.Pages[0].Extract == "" {
		return nil, ErrNotFound
	}
	return parseViEntry(r.Query.Pages[0].Title, r.Query.Pages[0].Extract)
}

var (
	headingRE = regexp.MustCompile(`^(={2,6})\s*(.*?)\s*={2,6}\s*$`)
	ipaRE     = regexp.MustCompile(`/[^/\n]+/`)
)

// viPOS are the section titles (lower case) that hold the definitions of a part of speech.
var viPOS = map[string]bool{
	"danh từ": true, "động từ": true, "nội động từ": true, "ngoại động từ": true, "tính từ": true,
	"phó từ": true, "trạng từ": true, "giới từ": true, "liên từ": true, "đại từ": true, "thán từ": true,
	"mạo từ": true, "số từ": true, "từ hạn định": true, "trợ động từ": true, "tiền tố": true,
	"hậu tố": true, "cụm từ": true, "viết tắt": true,
}

const maxMeaningsPerViPOS = 6

// parseViEntry reads the English part of a page of the Vietnamese Wiktionary (explaintext form).
func parseViEntry(title, extract string) (*Word, error) {
	w := &Word{Text: title}
	const (
		none = iota
		pron
		pos
	)
	var (
		inEnglish  bool
		mode       = none
		sawHead    bool // the headword line of the current part of speech was skipped
		defsOfPOS  int
		wantHeadwd = strings.ToLower(title)
	)
	for _, raw := range strings.Split(extract, "\n") {
		line := strings.TrimSpace(raw)
		if m := headingRE.FindStringSubmatch(line); m != nil {
			level, name := len(m[1]), strings.TrimSpace(m[2])
			switch {
			case level == 2:
				inEnglish = strings.EqualFold(name, "Tiếng Anh")
				mode = none
			case !inEnglish:
			case level == 3 && strings.EqualFold(name, "Cách phát âm"):
				mode = pron
			case level == 3 && viPOS[strings.ToLower(name)]:
				mode, sawHead, defsOfPOS = pos, false, 0
				w.PartOfSpeech = append(w.PartOfSpeech, strings.ToLower(name))
			default: // etymology, synonyms, references, conjugation, sub sections: not read
				mode = none
			}
			continue
		}
		if !inEnglish || line == "" {
			continue
		}
		switch mode {
		case pron:
			ipa := ipaRE.FindString(line)
			if ipa == "" {
				continue
			}
			switch {
			case strings.Contains(line, "(Anh)"):
				setFirst(&w.PhoneticUK, ipa)
			case strings.Contains(line, "(Mỹ)"):
				setFirst(&w.PhoneticUS, ipa)
			}
			setFirst(&w.Phonetic, ipa)
		case pos:
			if !sawHead {
				sawHead = true
				// the first line repeats the headword ("hello", "hello (số nhiều hellos)")
				if strings.HasPrefix(strings.ToLower(line), wantHeadwd) && utf8.RuneCountInString(line) <= 60 {
					continue
				}
			}
			if defsOfPOS < maxMeaningsPerViPOS {
				w.Meanings = append(w.Meanings, Meaning{Definition: line})
				defsOfPOS++
			}
		}
	}
	if len(w.Meanings) == 0 {
		return nil, ErrNotFound
	}
	return w, nil
}
