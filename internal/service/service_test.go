package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/authn"
	"github.com/huponx/gocommon/jwtx"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const userUUID = "0b0b0b0b-1111-4222-8333-444444444444"

func userCtx() context.Context {
	return authn.WithPrincipal(context.Background(), &authn.Principal{Subject: userUUID, ClientID: "gatekeeper", Claims: &jwtx.Claims{}})
}

func code(err error) codes.Code { return status.Code(err) }

// Every RPC has an entry in Permissions and no entry is left over (the server is deny by default).
func TestEveryRPCHasAPermission(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range dictionaryv1.DictionaryService_ServiceDesc.Methods {
		full := "/" + dictionaryv1.DictionaryService_ServiceDesc.ServiceName + "/" + m.MethodName
		seen[full] = true
		if Permissions[full] == "" {
			t.Errorf("%s has no entry in Permissions", full)
		}
	}
	for full := range Permissions {
		if !seen[full] {
			t.Errorf("Permissions has %s, which is not an RPC of DictionaryService", full)
		}
	}
}

type fakeLooker struct {
	words map[string]*lookup.Word
	err   error
}

func (f fakeLooker) Lookup(_ context.Context, _ lookup.Dictionary, w string) (*lookup.Word, error) {
	if f.err != nil {
		return nil, f.err
	}
	n, err := lookup.NormalizeWord(w)
	if err != nil {
		return nil, err
	}
	if v, ok := f.words[n]; ok {
		return v, nil
	}
	return nil, lookup.ErrNotFound
}

func (f fakeLooker) LookupMany(ctx context.Context, d lookup.Dictionary, words []string) ([]*lookup.Word, []string, error) {
	var found []*lookup.Word
	var nf []string
	for _, w := range words {
		v, err := f.Lookup(ctx, d, w)
		switch {
		case err == nil:
			found = append(found, v)
		case errors.Is(err, lookup.ErrNotFound):
			nf = append(nf, w)
		default:
			return nil, nil, err
		}
	}
	return found, nf, nil
}

type fakeStore struct {
	rows     map[string]store.SavedWord // pending words of the user
	exported map[string]bool
	clock    time.Time
	failMark bool
	links    map[string]store.DeckLink // by hash of the token
	settings map[string]json.RawMessage
}

func (f *fakeStore) Settings(context.Context) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for k, v := range f.settings {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) PutSettings(_ context.Context, rows map[string]json.RawMessage) error {
	if f.settings == nil {
		f.settings = map[string]json.RawMessage{}
	}
	for k, v := range rows {
		f.settings[k] = v
	}
	return nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]store.SavedWord{}, exported: map[string]bool{}, links: map[string]store.DeckLink{}, clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeStore) SaveWord(_ context.Context, _ string, w store.SavedWord, max int) (int, error) {
	if _, ok := f.rows[w.Word]; !ok && len(f.rows) >= max {
		return 0, store.ErrLimit
	}
	f.clock = f.clock.Add(time.Second)
	w.CreatedAt = f.clock
	f.rows[w.Word] = w
	return len(f.rows), nil
}
func (f *fakeStore) CountPending(context.Context, string) (int, error) { return len(f.rows), nil }
func (f *fakeStore) sorted() []store.SavedWord {
	var out []store.SavedWord
	for _, r := range f.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
func (f *fakeStore) ListPending(_ context.Context, _ string, after time.Time, afterWord string, limit int) ([]store.SavedWord, error) {
	all := f.sorted()
	var out []store.SavedWord
	for i := len(all) - 1; i >= 0; i-- { // newest first
		if afterWord != "" && !all[i].CreatedAt.Before(after) {
			continue
		}
		out = append(out, all[i])
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func (f *fakeStore) Pending(context.Context, string) ([]store.SavedWord, error) {
	return f.sorted(), nil
}
func (f *fakeStore) DeletePending(_ context.Context, _, word string) error {
	if _, ok := f.rows[word]; !ok {
		return store.ErrNotFound
	}
	delete(f.rows, word)
	return nil
}
func (f *fakeStore) MarkExported(_ context.Context, _ string, words []string) (int64, error) {
	if f.failMark {
		return 0, errors.New("db down")
	}
	for _, w := range words {
		delete(f.rows, w)
		f.exported[w] = true
	}
	return int64(len(words)), nil
}

func (f *fakeStore) PutDeckLink(_ context.Context, _ string, h []byte, l store.DeckLink) error {
	f.links = map[string]store.DeckLink{string(h): l} // one link per user
	return nil
}
func (f *fakeStore) TakeDeckLink(_ context.Context, h []byte, now time.Time) (*store.DeckLink, error) {
	l, ok := f.links[string(h)]
	if !ok || !l.ExpiresAt.After(now) {
		return nil, store.ErrNotFound
	}
	delete(f.links, string(h))
	return &l, nil
}

type fakeDecks struct {
	err   error
	built []anki.Word
}

func (f *fakeDecks) Build(_ context.Context, _ string, w []anki.Word) ([]byte, error) {
	f.built = w
	return []byte("apkg"), f.err
}

func newService(st *fakeStore, d *fakeDecks) *Service {
	return New(fakeLooker{words: map[string]*lookup.Word{"hello": {Text: "hello", Meanings: []lookup.Meaning{{Definition: "a greeting", MachineTranslated: true}}}}},
		st, d, Config{MaxSavedWords: 3, AudioHosts: []string{"api.dictionaryapi.dev"}})
}

func TestUserIDRules(t *testing.T) {
	cases := map[string]context.Context{
		"no principal": context.Background(),
		"service token": authn.WithPrincipal(context.Background(),
			&authn.Principal{Subject: "gatekeeper", ClientID: "gatekeeper"}),
		"subject is not a uuid": authn.WithPrincipal(context.Background(), &authn.Principal{Subject: "bob", ClientID: "gatekeeper"}),
	}
	want := map[string]codes.Code{"no principal": codes.Unauthenticated, "service token": codes.PermissionDenied, "subject is not a uuid": codes.Unauthenticated}
	s := newService(newFakeStore(), &fakeDecks{})
	for name, ctx := range cases {
		_, err := s.SaveWord(ctx, &dictionaryv1.SaveWordRequest{Word: "hello"})
		if code(err) != want[name] {
			t.Errorf("%s: code = %v (%v), want %v", name, code(err), err, want[name])
		}
	}
}

func TestLookup(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	r, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "Hello"})
	if err != nil || r.Word.Text != "hello" || !r.Word.Meanings[0].MachineTranslated {
		t.Fatalf("%+v, %v", r, err)
	}
	if _, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "zzz"}); code(err) != codes.NotFound {
		t.Errorf("unknown word: %v", err)
	}
	if _, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "a/b"}); code(err) != codes.InvalidArgument {
		t.Errorf("invalid word: %v", err)
	}
	if _, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "x", Dictionary: 99}); code(err) != codes.InvalidArgument {
		t.Errorf("unknown dictionary: %v", err)
	}
	down := New(fakeLooker{err: fmt.Errorf("%w: boom", lookup.ErrUnavailable)}, newFakeStore(), &fakeDecks{}, Config{})
	_, err = down.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "x"})
	if code(err) != codes.Unavailable || strings.Contains(err.Error(), "boom") {
		t.Errorf("provider failure must be Unavailable without details: %v", err)
	}

	many, err := s.LookupWords(userCtx(), &dictionaryv1.LookupWordsRequest{Words: []string{"hello", "nope"}})
	if err != nil || len(many.Words) != 1 || len(many.NotFound) != 1 || many.NotFound[0] != "nope" {
		t.Errorf("LookupWords: %+v, %v", many, err)
	}
	if _, err := s.LookupWords(userCtx(), &dictionaryv1.LookupWordsRequest{}); code(err) != codes.InvalidArgument {
		t.Errorf("no words: %v", err)
	}
	tooMany := make([]string, 21)
	if _, err := s.LookupWords(userCtx(), &dictionaryv1.LookupWordsRequest{Words: tooMany}); code(err) != codes.InvalidArgument {
		t.Errorf("21 words: %v", err)
	}
}

func TestSaveWord(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	for i, w := range []string{"One", "two", "three"} {
		r, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: w, Content: "c"})
		if err != nil || int(r.SavedCount) != i+1 {
			t.Fatalf("save %q: %+v, %v", w, r, err)
		}
	}
	if _, ok := st.rows["one"]; !ok {
		t.Error("the word must be stored normalized")
	}
	_, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "four"})
	if code(err) != codes.ResourceExhausted || !strings.HasPrefix(status.Convert(err).Message(), CodeMaxSavedWords) {
		t.Errorf("limit: code=%v err=%v", code(err), err)
	}
	if r, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "one", Content: "again"}); err != nil || r.SavedCount != 3 {
		t.Errorf("saving a pending word again is an update: %+v, %v", r, err)
	}

	for _, req := range []*dictionaryv1.SaveWordRequest{
		{Word: "a/b"},
		{Word: "ok", Content: strings.Repeat("x", maxContentBytes+1)},
		{Word: "ok", AudioUs: "https://evil.example/a.mp3"},
		{Word: "ok", AudioUk: "http://api.dictionaryapi.dev/a.mp3"},
	} {
		if _, err := s.SaveWord(userCtx(), req); code(err) != codes.InvalidArgument {
			t.Errorf("%+v: code = %v, want InvalidArgument", req, code(err))
		}
	}
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "one", AudioUs: "https://api.dictionaryapi.dev/m/one-us.mp3"}); err != nil {
		t.Errorf("an allowed audio host: %v", err)
	}
}

func TestListSavedWordsPages(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	s.cfg.MaxSavedWords = 10
	for _, w := range []string{"a", "b", "c", "d", "e"} {
		if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: w}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	token := ""
	for pages := 0; pages < 5; pages++ {
		r, err := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if r.Total != 5 {
			t.Errorf("total = %d", r.Total)
		}
		for _, w := range r.Words {
			got = append(got, w.Word)
		}
		if token = r.NextPageToken; token == "" {
			break
		}
	}
	if strings.Join(got, "") != "edcba" {
		t.Errorf("order = %v, want newest first (edcba)", got)
	}
	if _, err := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{PageToken: "!!!"}); code(err) != codes.InvalidArgument {
		t.Errorf("bad token: %v", err)
	}
	if _, err := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{PageSize: -1}); code(err) != codes.InvalidArgument {
		t.Errorf("negative page size: %v", err)
	}
}

func TestDeleteSavedWord(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	_, _ = s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "gone"})
	if _, err := s.DeleteSavedWord(userCtx(), &dictionaryv1.DeleteSavedWordRequest{Word: "Gone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSavedWord(userCtx(), &dictionaryv1.DeleteSavedWordRequest{Word: "gone"}); code(err) != codes.NotFound {
		t.Errorf("second delete: %v", err)
	}
}

func TestExportAnki(t *testing.T) {
	st := newFakeStore()
	decks := &fakeDecks{}
	s := newService(st, decks)

	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{}); code(err) != codes.InvalidArgument ||
		!strings.HasPrefix(status.Convert(err).Message(), CodeNoWords) {
		t.Errorf("nothing to export: %v", err)
	}

	_, _ = s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "one", Content: "1"})
	_, _ = s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "two", Content: "2"})
	r, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{DeckName: "A/B: deck"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Filename != "A_B_ deck.apkg" || string(r.Data) != "apkg" || r.WordCount != 2 {
		t.Errorf("response = %+v", r)
	}
	if len(decks.built) != 2 || decks.built[0].Word != "one" {
		t.Errorf("cards = %+v (oldest first)", decks.built)
	}
	if len(st.rows) != 0 || !st.exported["one"] || !st.exported["two"] {
		t.Errorf("the words must be marked exported: pending=%v exported=%v", st.rows, st.exported)
	}
	if r, _ := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{}); r.Total != 0 {
		t.Errorf("a new batch starts: total = %d", r.Total)
	}

	// a deck that is too large, or a failure, leaves the words pending
	_, _ = s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "three"})
	decks.err = anki.ErrTooLarge
	_, err = s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{})
	if code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), CodeExportTooBig) {
		t.Errorf("too large: %v", err)
	}
	decks.err = errors.New("sqlite exploded")
	_, err = s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{})
	if code(err) != codes.Internal || strings.Contains(err.Error(), "sqlite") {
		t.Errorf("build failure must be Internal without details: %v", err)
	}
	if _, ok := st.rows["three"]; !ok {
		t.Error("a failed export must leave the word pending")
	}

	decks.err = nil
	long := strings.Repeat("x", maxDeckRunes+1)
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{DeckName: long}); code(err) != codes.InvalidArgument {
		t.Errorf("long deck name: %v", err)
	}
}

func serviceCtx() context.Context {
	return authn.WithPrincipal(context.Background(), &authn.Principal{Subject: "gatekeeper", ClientID: "gatekeeper", Claims: &jwtx.Claims{}})
}

func TestDeckLinkCreateAndDownloadOnce(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})

	if _, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{}); code(err) != codes.InvalidArgument {
		t.Fatalf("nothing to export: %v", err)
	}
	_, _ = s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "one", Content: "1"})
	r, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{DeckName: "A/B"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Token) < 40 || r.Filename != "A_B.apkg" || r.WordCount != 1 {
		t.Fatalf("response = %+v", r)
	}
	exp, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil || time.Until(exp) < 14*time.Minute || time.Until(exp) > 16*time.Minute {
		t.Errorf("expires_at = %q (%v), want about 15 minutes", r.ExpiresAt, err)
	}
	if len(st.rows) != 0 || !st.exported["one"] {
		t.Errorf("the words must be marked exported: %v %v", st.rows, st.exported)
	}
	for h := range st.links {
		if strings.Contains(h, r.Token) {
			t.Error("the store must keep only the hash of the token")
		}
	}

	// only a service token downloads, a user token does not
	if _, err := s.DownloadDeck(userCtx(), &dictionaryv1.DownloadDeckRequest{Token: r.Token}); code(err) != codes.PermissionDenied {
		t.Errorf("user token: %v", err)
	}
	if _, err := s.DownloadDeck(context.Background(), &dictionaryv1.DownloadDeckRequest{Token: r.Token}); code(err) != codes.PermissionDenied {
		t.Errorf("no token: %v", err)
	}
	d, err := s.DownloadDeck(serviceCtx(), &dictionaryv1.DownloadDeckRequest{Token: r.Token})
	if err != nil || d.Filename != "A_B.apkg" || string(d.Data) != "apkg" {
		t.Fatalf("download = %+v %v", d, err)
	}
	// it works once
	if _, err := s.DownloadDeck(serviceCtx(), &dictionaryv1.DownloadDeckRequest{Token: r.Token}); code(err) != codes.NotFound {
		t.Errorf("second download: %v", err)
	}
	for _, tok := range []string{"", "short", strings.Repeat("x", 200), strings.Repeat("x", 43)} {
		if _, err := s.DownloadDeck(serviceCtx(), &dictionaryv1.DownloadDeckRequest{Token: tok}); code(err) != codes.NotFound {
			t.Errorf("token %q: %v", tok, err)
		}
	}
}

func TestDeckLinkExpiresAndKeepsWordsPendingOnFailure(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	s.cfg.DeckLinkTTL = time.Millisecond
	_, _ = s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "one"})
	r, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := s.DownloadDeck(serviceCtx(), &dictionaryv1.DownloadDeckRequest{Token: r.Token}); code(err) != codes.NotFound {
		t.Errorf("expired link: %v", err)
	}

	_, _ = s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "two"})
	st.failMark = true
	if _, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{}); code(err) != codes.Internal {
		t.Errorf("mark failure: %v", err)
	}
	if len(st.rows) != 1 {
		t.Errorf("the words stay pending: %v", st.rows)
	}
}
