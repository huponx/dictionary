package anki

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAllowedURL(t *testing.T) {
	hosts := []string{"api.dictionaryapi.dev", "upload.wikimedia.org"}
	for raw, want := range map[string]bool{
		"https://api.dictionaryapi.dev/media/hello-us.mp3":  true,
		"https://UPLOAD.wikimedia.org/a.mp3":                true,
		"http://api.dictionaryapi.dev/a.mp3":                false, // not https
		"https://evil.example/a.mp3":                        false,
		"https://api.dictionaryapi.dev.evil.example/a.mp3":  false,
		"https://user@api.dictionaryapi.dev/a.mp3":          false,
		"https://127.0.0.1/a.mp3":                           false,
		"file:///etc/passwd":                                false,
		"":                                                  false,
		"https://api.dictionaryapi.dev:8443/a.mp3":          true, // the host decides, not the port
		"//api.dictionaryapi.dev/a.mp3":                     false,
		"https://metadata.google.internal/computeMetadata/": false,
	} {
		if got := AllowedURL(raw, hosts); got != want {
			t.Errorf("AllowedURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// files lists the names inside the .apkg (a zip).
func files(t *testing.T, apkg []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(apkg), int64(len(apkg)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(rc)
		rc.Close()
		out[f.Name] = b.Bytes()
	}
	return out
}

func TestBuild(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ID3-audio:" + r.URL.Path))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	host = host[:strings.LastIndex(host, ":")]

	b := &Builder{Client: srv.Client(), AllowedHosts: []string{host}}
	words := []Word{
		{Word: "hello", Content: "<b>hi</b> " + PlaceholderUS + " " + PlaceholderUK, AudioUS: srv.URL + "/hello-us.mp3", AudioUK: srv.URL + "/hello-uk.mp3"},
		{Word: "world", Content: "w " + PlaceholderUS, AudioUS: srv.URL + "/missing.mp3"},    // fails: placeholder dropped
		{Word: "evil", Content: "e " + PlaceholderUK, AudioUK: "https://evil.example/x.mp3"}, // not allowed: never fetched
	}
	data, err := b.Build(context.Background(), "My Words", words)
	if err != nil {
		t.Fatal(err)
	}
	f := files(t, data)
	if _, ok := f["collection.anki2"]; !ok {
		t.Errorf("no collection in %v", keys(f))
	}
	media := string(f["media"])
	if !strings.Contains(media, "w000_us.mp3") || !strings.Contains(media, "w000_uk.mp3") {
		t.Errorf("media list = %s", media)
	}
	if strings.Contains(media, "w001") || strings.Contains(media, "w002") {
		t.Errorf("a failed or refused file was attached: %s", media)
	}
	if hits.Load() < 3 { // hello us, hello uk, missing (retried)
		t.Errorf("hits = %d", hits.Load())
	}
}

func TestBuildImages(t *testing.T) {
	const a, b, c = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	bd := &Builder{Client: http.DefaultClient}
	imgs := map[string]Image{a: {Data: []byte("jpegbytes"), Ext: "jpg"}, c: {Data: []byte("pngbytes"), Ext: "png"}}
	data, err := bd.Build(context.Background(), "d", []Word{
		{Word: "one", Content: `<p>x</p><img src="dictimg:` + a + `"><img src="dictimg:` + b + `">`, Images: imgs}, // b has no file
		{Word: "two", Content: `<img src="dictimg:` + c + `">`, Images: imgs},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := files(t, data)
	media := string(f["media"])
	if !strings.Contains(media, "w000_11111111.jpg") || !strings.Contains(media, "w001_33333333.png") || strings.Contains(media, "22222222") {
		t.Errorf("media = %s", media)
	}
	if bytes.Contains(f["collection.anki2"], []byte("dictimg:")) {
		t.Error("a dictimg: reference is left in the deck")
	}
}

func TestBuildTooLarge(t *testing.T) {
	b := &Builder{Client: http.DefaultClient, MaxBytes: 10}
	if _, err := b.Build(context.Background(), "d", []Word{{Word: "a", Content: "b"}}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
	if _, err := b.Build(context.Background(), "d", nil); err == nil {
		t.Error("no words must fail")
	}
}

func TestFetchLimitsAndRedirects(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 2000))
		case "/redirect":
			http.Redirect(w, r, "https://evil.example/x.mp3", http.StatusFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	host = host[:strings.LastIndex(host, ":")]
	b := &Builder{Client: srv.Client(), AllowedHosts: []string{host}, MaxAudioBytes: 1000}
	if _, err := b.fetch(context.Background(), srv.URL+"/big"); err == nil {
		t.Error("a file over the limit must be refused")
	}
	if _, err := b.fetch(context.Background(), srv.URL+"/redirect"); err == nil {
		t.Error("a redirect to another host must be refused")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
