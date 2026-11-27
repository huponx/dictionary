package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const freeDictHello = `[{"word":"hello","phonetic":"/həˈləʊ/","phonetics":[
 {"audio":"https://api.dictionaryapi.dev/media/pronunciations/en/hello-au.mp3"},
 {"text":"/həˈləʊ/","audio":"https://api.dictionaryapi.dev/media/pronunciations/en/hello-uk.mp3"},
 {"text":"/həˈloʊ/","audio":"https://api.dictionaryapi.dev/media/pronunciations/en/hello-us.mp3"}],
 "meanings":[{"partOfSpeech":"noun","definitions":[{"definition":"A greeting.","example":"She said hello."}]},
 {"partOfSpeech":"interjection","definitions":[{"definition":"Used to greet someone."},{"definition":""}]}]}]`

const viHello = `
== Tiếng Anh ==


=== Cách phát âm ===
IPA:
/hə.ˈləʊ̯/, /hɛ.ˈləʊ̯/ (Anh)
/hɛ.ˈloʊ̯/, /hə.ˈloʊ̯/ (Mỹ)


=== Từ nguyên ===
Từ hallo. Đôi khi được cho là do Thomas Edison.


=== Thán từ ===
hello

Chào anh!, chào chị!
A lô (trên điện thoại).


==== Đồng nghĩa ====
hi (Mỹ; Anh)


=== Danh từ ===
hello (số nhiều hellos)

Tiếng chào.


=== Tham khảo ===
Hồ Ngọc Đức (biên tập) (2003)

== Tiếng Pháp ==

=== Danh từ ===
hello

Không đọc.
`

func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *http.Client) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s, s.Client()
}

func TestNormalizeWord(t *testing.T) {
	for in, want := range map[string]string{"  Hello ": "hello", "ice   cream": "ice cream", "don't": "don't", "e-mail": "e-mail", "Café": "café"} {
		if got, err := NormalizeWord(in); err != nil || got != want {
			t.Errorf("NormalizeWord(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "a/b", "a?b=c", "../x", strings.Repeat("a", 65), "x<y"} {
		if _, err := NormalizeWord(in); !errors.Is(err, ErrInvalidWord) {
			t.Errorf("NormalizeWord(%q) = %v, want ErrInvalidWord", in, err)
		}
	}
}

func TestFreeDict(t *testing.T) {
	var path atomic.Value
	s, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		if r.Header.Get("User-Agent") != "ua/1" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if strings.HasSuffix(r.URL.Path, "/zzzxq") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"title":"No Definitions Found"}`))
			return
		}
		_, _ = w.Write([]byte(freeDictHello))
	})
	p := NewFreeDict(s.URL+"/entries/en/", c, "ua/1")

	w, err := p.Lookup(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if path.Load() != "/entries/en/hello" {
		t.Errorf("path = %v", path.Load())
	}
	if w.Text != "hello" || w.Phonetic != "/həˈləʊ/" || w.PhoneticUK != "/həˈləʊ/" || w.PhoneticUS != "/həˈloʊ/" {
		t.Errorf("phonetics: %+v", w)
	}
	if !strings.HasSuffix(w.AudioUK, "hello-uk.mp3") || !strings.HasSuffix(w.AudioUS, "hello-us.mp3") {
		t.Errorf("audio: %q %q", w.AudioUK, w.AudioUS)
	}
	if len(w.PartOfSpeech) != 2 || len(w.Meanings) != 2 || w.Meanings[0].Examples[0] != "She said hello." {
		t.Errorf("meanings: %+v", w)
	}

	if _, err := p.Lookup(context.Background(), "zzzxq"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown word: %v", err)
	}
}

func TestFreeDictUnavailable(t *testing.T) {
	var calls atomic.Int32
	s, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	p := NewFreeDict(s.URL, c, "ua")
	p.get.retry.InitialWait = time.Millisecond
	if _, err := p.Lookup(context.Background(), "hello"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 attempts", calls.Load())
	}
}

func TestParseViEntry(t *testing.T) {
	w, err := parseViEntry("hello", viHello)
	if err != nil {
		t.Fatal(err)
	}
	if w.PhoneticUK != "/hə.ˈləʊ̯/" || w.PhoneticUS != "/hɛ.ˈloʊ̯/" || w.Phonetic != "/hə.ˈləʊ̯/" {
		t.Errorf("phonetics: %+v", w)
	}
	if got := strings.Join(w.PartOfSpeech, ","); got != "thán từ,danh từ" {
		t.Errorf("part of speech = %q", got)
	}
	var defs []string
	for _, m := range w.Meanings {
		defs = append(defs, m.Definition)
	}
	want := "Chào anh!, chào chị!|A lô (trên điện thoại).|Tiếng chào."
	if strings.Join(defs, "|") != want {
		t.Errorf("definitions = %q, want %q", strings.Join(defs, "|"), want)
	}
}

func TestParseViEntryWithoutEnglish(t *testing.T) {
	if _, err := parseViEntry("x", "\n== Tiếng Pháp ==\n=== Danh từ ===\nx\n\nnghĩa\n"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestViWiktionary(t *testing.T) {
	s, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("titles") == "nope" {
			_, _ = w.Write([]byte(`{"query":{"pages":[{"title":"nope","missing":true}]}}`))
			return
		}
		out, _ := jsonString(viHello)
		_, _ = w.Write([]byte(`{"query":{"pages":[{"title":"hello","extract":` + out + `}]}}`))
	})
	p := NewViWiktionary(s.URL, c, "ua")
	if w, err := p.Lookup(context.Background(), "hello"); err != nil || len(w.Meanings) != 3 {
		t.Fatalf("hello: %+v, %v", w, err)
	}
	if _, err := p.Lookup(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing page: %v", err)
	}
}

func TestMyMemory(t *testing.T) {
	s, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("langpair") != "en|vi" {
			t.Errorf("langpair = %q", r.URL.Query().Get("langpair"))
		}
		switch r.URL.Query().Get("q") {
		case "quota":
			_, _ = w.Write([]byte(`{"responseData":{"translatedText":"MYMEMORY WARNING: YOU USED ALL AVAILABLE FREE TRANSLATIONS"},"quotaFinished":true,"responseStatus":429}`))
		case "same":
			_, _ = w.Write([]byte(`{"responseData":{"translatedText":"same"},"responseStatus":200}`))
		default:
			_, _ = w.Write([]byte(`{"responseData":{"translatedText":"xin chào"},"responseStatus":200}`))
		}
	})
	m := NewMyMemory(s.URL, c, "ua")
	if got, err := m.Translate(context.Background(), "hello"); err != nil || got != "xin chào" {
		t.Errorf("Translate = %q, %v", got, err)
	}
	for _, in := range []string{"quota", "same"} {
		if _, err := m.Translate(context.Background(), in); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Translate(%q) = %v, want ErrUnavailable", in, err)
		}
	}
}

type fakeProvider struct {
	word  *Word
	err   error
	calls atomic.Int32
}

func (f *fakeProvider) Lookup(context.Context, string) (*Word, error) {
	f.calls.Add(1)
	return f.word, f.err
}

type fakeTranslator struct{ fail bool }

func (f fakeTranslator) Translate(_ context.Context, text string) (string, error) {
	if f.fail {
		return "", ErrUnavailable
	}
	return "vi:" + text, nil
}

func TestVietnamese(t *testing.T) {
	en := &Word{Text: "hello", Phonetic: "/en/", PhoneticUS: "/us/", AudioUS: "us.mp3", AudioUK: "uk.mp3",
		Meanings: []Meaning{{Definition: "d1", Examples: []string{"e1"}}, {Definition: "d2"}, {Definition: "d3"}, {Definition: "d4"}}}
	wiki := &Word{Text: "hello", PhoneticUK: "/vi-uk/", Meanings: []Meaning{{Definition: "xin chào"}}}

	t.Run("wiktionary wins and borrows audio", func(t *testing.T) {
		v := &Vietnamese{English: &fakeProvider{word: en}, Wiktionary: &fakeProvider{word: wiki}}
		w, err := v.Lookup(context.Background(), "hello")
		if err != nil || w.Meanings[0].Definition != "xin chào" || w.AudioUS != "us.mp3" || w.PhoneticUK != "/vi-uk/" || w.PhoneticUS != "/us/" {
			t.Fatalf("%+v, %v", w, err)
		}
	})
	t.Run("wiktionary without english", func(t *testing.T) {
		v := &Vietnamese{English: &fakeProvider{err: ErrNotFound}, Wiktionary: &fakeProvider{word: wiki}}
		if w, err := v.Lookup(context.Background(), "hello"); err != nil || w.AudioUS != "" || len(w.Meanings) != 1 {
			t.Fatalf("%+v, %v", w, err)
		}
	})
	t.Run("machine translation of the first definitions", func(t *testing.T) {
		v := &Vietnamese{English: &fakeProvider{word: en}, Wiktionary: &fakeProvider{err: ErrNotFound}, Translator: fakeTranslator{}}
		w, err := v.Lookup(context.Background(), "hello")
		if err != nil || len(w.Meanings) != 3 || !w.Meanings[0].MachineTranslated || w.Meanings[0].Definition != "vi:d1" || w.Meanings[0].Examples[0] != "e1" {
			t.Fatalf("%+v, %v", w, err)
		}
	})
	t.Run("translation down: english meanings", func(t *testing.T) {
		v := &Vietnamese{English: &fakeProvider{word: en}, Wiktionary: &fakeProvider{err: ErrNotFound}, Translator: fakeTranslator{fail: true}}
		if w, err := v.Lookup(context.Background(), "hello"); err != nil || len(w.Meanings) != 4 || w.Meanings[0].MachineTranslated {
			t.Fatalf("%+v, %v", w, err)
		}
	})
	t.Run("translation off: english meanings", func(t *testing.T) {
		v := &Vietnamese{English: &fakeProvider{word: en}, Wiktionary: &fakeProvider{err: ErrNotFound}}
		if w, err := v.Lookup(context.Background(), "hello"); err != nil || w.Meanings[0].Definition != "d1" {
			t.Fatalf("%+v, %v", w, err)
		}
	})
	t.Run("unknown everywhere", func(t *testing.T) {
		v := &Vietnamese{English: &fakeProvider{err: ErrNotFound}, Wiktionary: &fakeProvider{err: ErrNotFound}}
		if _, err := v.Lookup(context.Background(), "zzz"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("wiktionary down is not hidden", func(t *testing.T) {
		v := &Vietnamese{English: &fakeProvider{word: en}, Wiktionary: &fakeProvider{err: ErrUnavailable}}
		if _, err := v.Lookup(context.Background(), "hello"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestServiceCacheAndMany(t *testing.T) {
	p := &fakeProvider{word: &Word{Text: "x"}}
	s := New(NewRegistry().Register(Entry{ID: "english", Group: "en", Provider: p}), time.Hour, 10)
	for range 3 {
		if _, err := s.Lookup(context.Background(), "english", " Hello "); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 1 {
		t.Errorf("provider calls = %d, want 1 (cached)", p.calls.Load())
	}
	if _, err := s.Lookup(context.Background(), "english-vietnamese", "x"); !errors.Is(err, ErrUnknownDictionary) {
		t.Errorf("unknown dictionary: %v", err)
	}
	if _, err := s.Lookup(context.Background(), "english", "a/b"); !errors.Is(err, ErrInvalidWord) {
		t.Errorf("invalid word: %v", err)
	}

	multi := &multiProvider{}
	s2 := New(NewRegistry().Register(Entry{ID: "english", Group: "en", Provider: multi}), 0, 0)
	found, nf, err := s2.LookupMany(context.Background(), "english", []string{"a", "A", "missing", "b"})
	if err != nil || len(found) != 2 || len(nf) != 1 || nf[0] != "missing" {
		t.Fatalf("found=%v notFound=%v err=%v", found, nf, err)
	}
	if _, _, err := s2.LookupMany(context.Background(), "english", []string{"a", "boom"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("provider failure must end the call: %v", err)
	}
}

type multiProvider struct{}

func (multiProvider) Lookup(_ context.Context, w string) (*Word, error) {
	switch w {
	case "missing":
		return nil, ErrNotFound
	case "boom":
		return nil, ErrUnavailable
	}
	return &Word{Text: w}, nil
}

func TestCacheExpiryAndEviction(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newCache(time.Minute, 2)
	c.now = func() time.Time { return now }
	c.put("a", &Word{Text: "a"})
	now = now.Add(30 * time.Second)
	c.put("b", &Word{Text: "b"})
	c.put("c", &Word{Text: "c"}) // full: the one that expires first (a) goes
	if _, ok := c.get("a"); ok {
		t.Error("a must be evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Error("b must be cached")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.get("b"); ok {
		t.Error("b must be expired")
	}
	off := newCache(0, 10)
	off.put("x", &Word{})
	if _, ok := off.get("x"); ok {
		t.Error("a cache with ttl 0 is off")
	}
}

func jsonString(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}
