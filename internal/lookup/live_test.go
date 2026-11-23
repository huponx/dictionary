package lookup

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// Calls the real providers. Off by default (the free APIs are rate limited and the network may be absent):
//
//	DICTIONARY_LIVE=1 go test ./internal/lookup -run Live -v
func TestLiveProviders(t *testing.T) {
	if os.Getenv("DICTIONARY_LIVE") == "" {
		t.Skip("DICTIONARY_LIVE is not set")
	}
	const ua = "huponx-dictionary-test/1.0"
	c := &http.Client{Timeout: 15 * time.Second}
	en := NewFreeDict("https://api.dictionaryapi.dev/api/v2/entries/en", c, ua)
	wiki := NewViWiktionary("https://vi.wiktionary.org/w/api.php", c, ua)
	vi := &Vietnamese{English: en, Wiktionary: wiki, Translator: NewMyMemory("https://api.mymemory.translated.net/get", c, ua)}
	ctx := context.Background()

	w, err := en.Lookup(ctx, "hello")
	if err != nil || len(w.Meanings) == 0 || w.AudioUK == "" {
		t.Fatalf("english hello: %+v, %v", w, err)
	}
	t.Logf("english: %+v", w)
	if _, err := en.Lookup(ctx, "zzzxqzzz"); err != ErrNotFound {
		t.Errorf("english unknown: %v", err)
	}
	for _, word := range []string{"hello", "run", "serendipity"} {
		w, err := vi.Lookup(ctx, word)
		if err != nil {
			t.Errorf("vietnamese %s: %v", word, err)
			continue
		}
		t.Logf("vi %s: pos=%v phon=%q/%q/%q meanings=%d first=%+v", word, w.PartOfSpeech, w.Phonetic, w.PhoneticUK, w.PhoneticUS, len(w.Meanings), w.Meanings[0])
	}
}
