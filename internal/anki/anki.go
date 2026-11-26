// Package anki builds an Anki deck (.apkg) from saved words.
package anki

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/huponx/dictionary/internal/cardref"
	goanki "github.com/huponx/go-anki-deck"
	"github.com/huponx/gocommon/retry"
	"go.uber.org/zap"
)

// Placeholders of the card back that become the sound tags of the audio files.
const (
	PlaceholderUK = "replace_audio_uk"
	PlaceholderUS = "replace_audio_us"
)

// ErrTooLarge: the deck is larger than the configured limit.
var ErrTooLarge = errors.New("anki: deck too large")

// Word is a card: the word on the front, Content (HTML) on the back, and the audio to attach.
type Word struct {
	Word    string
	Content string
	AudioUK string
	AudioUS string
	// Images are the files of the images Content refers to as <img src="dictimg:<id>">; a reference without a
	// file loses its tag.
	Images map[string]Image
	// AudioFiles are the files of the uploaded audio: AudioUK / AudioUS hold dictaud:<id> and the key is that
	// whole value. A reference without a file leaves its sound out.
	AudioFiles map[string]Image
}

// Image is a file of a card.
type Image struct {
	Data []byte
	Ext  string
}

// Builder builds decks. The audio files are downloaded from AllowedHosts only.
type Builder struct {
	Client       *http.Client
	AllowedHosts []string
	// MaxBytes is the largest deck returned (0: no limit) unless the context of Build says otherwise (WithMaxBytes).
	MaxBytes int
	// MaxAudioBytes is the largest audio file attached (default 512 KiB); a larger one is skipped.
	MaxAudioBytes int64
	// AudioBudget is how long all the downloads of one deck may take together (default 20s);
	// what is not downloaded in time is left out of the deck.
	AudioBudget time.Duration
	Log         *zap.Logger
}

const (
	defaultMaxAudio    = 512 << 10
	defaultAudioBudget = 20 * time.Second
	downloadWorkers    = 5
)

// AllowedURL reports whether the URL is an https address on one of the hosts (lower case).
func AllowedURL(raw string, hosts []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		if host == h {
			return true
		}
	}
	return false
}

type audio struct {
	data []byte
}

type progressKey struct{}

type maxBytesKey struct{}

// WithMaxBytes returns a context whose Build limits the deck to n bytes instead of Builder.MaxBytes (0: no limit),
// so the limit can follow a setting that changes while the service runs.
func WithMaxBytes(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, maxBytesKey{}, n)
}

func (b *Builder) maxBytes(ctx context.Context) int {
	if n, ok := ctx.Value(maxBytesKey{}).(int); ok {
		return n
	}
	return b.MaxBytes
}

// WithProgress returns a context whose Build reports its progress to fn: a fraction from 0 to 1 that grows while the
// audio is downloaded and the cards are added (the last part, saving the deck, is not reported: it is the end of Build).
// fn is called from several goroutines and must not block for long.
func WithProgress(ctx context.Context, fn func(fraction float64)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ReportProgress tells the function of WithProgress (when the context has one) how far a build is.
func ReportProgress(ctx context.Context, fraction float64) {
	if fn, ok := ctx.Value(progressKey{}).(func(float64)); ok {
		fn(fraction)
	}
}

// the share of Build that each phase takes in the progress: downloading the audio, then adding the cards
const (
	downloadShare = 0.7
	cardsShare    = 0.25
)

// Build returns the .apkg of the words (one card each). A file that cannot be downloaded is left out
// of its card without failing the deck.
//
// fresh decides what Anki does when the deck is imported again. Off: the cards keep their identity (guid of the
// word, ids of the deck and of the note type from the deck name), so Anki updates them. On: every export is a
// new deck, so Anki adds the cards again.
func (b *Builder) Build(ctx context.Context, deckName string, words []Word, fresh bool) ([]byte, error) {
	if len(words) == 0 {
		return nil, errors.New("anki: no words")
	}
	files := b.download(ctx, words)
	ReportProgress(ctx, downloadShare)

	deck, err := goanki.NewDeckWithOptions(deckName, &goanki.Options{Fresh: fresh})
	if err != nil {
		return nil, fmt.Errorf("anki: new deck: %w", err)
	}
	defer deck.Close()

	for i, w := range words {
		back := w.Content
		for _, side := range []struct{ placeholder, name, ref string }{
			{PlaceholderUS, "us", w.AudioUS}, {PlaceholderUK, "uk", w.AudioUK},
		} {
			a, ext := files[side.ref], "mp3"
			if up, ok := w.AudioFiles[side.ref]; ok && cardref.IsAudioRef(side.ref) {
				a, ext = &audio{data: up.Data}, up.Ext
			}
			back = strings.ReplaceAll(back, side.placeholder, soundTag(deck, fmt.Sprintf("w%03d_%s.%s", i, side.name, ext), a))
		}
		back = cardref.ReplaceTags(back, func(id string) string {
			img, ok := w.Images[id]
			if !ok || len(img.Data) == 0 {
				return ""
			}
			name := fmt.Sprintf("w%03d_%s.%s", i, id[:8], img.Ext)
			return deck.AddImage(name, img.Data)
		})
		if err := deck.AddCard(w.Word, back); err != nil {
			return nil, fmt.Errorf("anki: card %q: %w", w.Word, err)
		}
		if (i+1)%50 == 0 {
			ReportProgress(ctx, downloadShare+cardsShare*float64(i+1)/float64(len(words)))
		}
	}
	ReportProgress(ctx, downloadShare+cardsShare)
	data, err := deck.Save()
	if err != nil {
		return nil, fmt.Errorf("anki: save deck: %w", err)
	}
	if limit := b.maxBytes(ctx); limit > 0 && len(data) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

// soundTag attaches the file and returns its tag, or "" when there is no file.
func soundTag(deck *goanki.Deck, name string, a *audio) string {
	if a == nil || len(a.data) == 0 {
		return ""
	}
	return deck.AddAudio(name, a.data)
}

// download fetches every distinct audio URL (the uploaded files are in Word.AudioFiles, not downloaded) of the words, a few at a time, within the budget.
func (b *Builder) download(ctx context.Context, words []Word) map[string]*audio {
	budget := b.AudioBudget
	if budget <= 0 {
		budget = defaultAudioBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var urls []string
	seen := map[string]bool{}
	for _, w := range words {
		for _, u := range []string{w.AudioUS, w.AudioUK} {
			if u != "" && !seen[u] && !cardref.IsAudioRef(u) {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
	out := make(map[string]*audio, len(urls))
	var (
		mu   sync.Mutex
		done int
		wg   sync.WaitGroup
		sem  = make(chan struct{}, downloadWorkers)
	)
	for _, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, err := b.fetch(ctx, u)
			defer func() { // a file that failed counts too: the progress is the files tried
				mu.Lock()
				done++
				share := downloadShare * float64(done) / float64(len(urls))
				mu.Unlock()
				ReportProgress(ctx, share)
			}()
			if err != nil {
				if b.Log != nil {
					b.Log.Warn("audio skipped", zap.String("url", u), zap.Error(err))
				}
				return
			}
			mu.Lock()
			out[u] = &audio{data: data}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// fetch downloads one file (retrying), checking the host again on every redirect.
func (b *Builder) fetch(ctx context.Context, raw string) ([]byte, error) {
	if !AllowedURL(raw, b.AllowedHosts) {
		return nil, errors.New("audio host not allowed")
	}
	limit := b.MaxAudioBytes
	if limit <= 0 {
		limit = defaultMaxAudio
	}
	client := *b.Client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !AllowedURL(req.URL.String(), b.AllowedHosts) {
			return errors.New("redirect not allowed")
		}
		return nil
	}
	var data []byte
	// a missing file (4xx) will not appear by asking again: stop the backoff at once
	var gone error
	waitCtx, stop := context.WithCancel(ctx)
	defer stop()
	err := retry.Do(waitCtx, retry.Config{MaxAttempts: 3, InitialWait: 300 * time.Millisecond, MaxWait: 2 * time.Second, Multiplier: 2, Jitter: 0.2}, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("status %d", resp.StatusCode)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
				gone = err
				stop()
			}
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return err
		}
		if int64(len(body)) > limit {
			return fmt.Errorf("file larger than %d bytes", limit)
		}
		data = body
		return nil
	})
	if gone != nil {
		return nil, gone
	}
	return data, err
}
