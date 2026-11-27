package service

import (
	"context"
	"strings"
	"testing"

	"github.com/huponx/dictionary/internal/filestore"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
)

func audioService(t *testing.T, maxBytes int, quota int64) (*Service, *fakeStore, *fakeDecks) {
	t.Helper()
	st, d := newFakeStore(), &fakeDecks{}
	s := newService(st, d).WithAudio(&filestore.Store{Dir: t.TempDir()}, maxBytes, quota)
	return s, st, d
}

// mp3Bytes is a file the sniffer takes for an mp3; n makes different files.
func mp3Bytes(n int) []byte {
	return append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), []byte(strings.Repeat("x", n))...)
}

func TestUploadAndGetAudio(t *testing.T) {
	s, _, _ := audioService(t, 1<<10, 1<<20)
	data := mp3Bytes(100)
	up, err := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{ContentType: "audio/mpeg", Data: data})
	if err != nil || up.Id == "" {
		t.Fatalf("%+v %v", up, err)
	}
	got, err := s.GetAudio(userCtx(), &dictionaryv1.GetAudioRequest{Id: up.Id})
	if err != nil || got.ContentType != "audio/mpeg" || string(got.Data) != string(data) {
		t.Fatalf("%+v %v", got, err)
	}
	// the same file is stored once
	again, err := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: data})
	if err != nil || again.Id != up.Id {
		t.Fatalf("dedup: %+v %v", again, err)
	}
	// another user neither sees it nor shares it
	if _, err := s.GetAudio(otherCtx(), &dictionaryv1.GetAudioRequest{Id: up.Id}); code(err) != codes.NotFound {
		t.Fatalf("other user: %v", err)
	}
	other, err := s.UploadAudio(otherCtx(), &dictionaryv1.UploadAudioRequest{Data: data})
	if err != nil || other.Id == up.Id {
		t.Fatalf("other upload: %+v %v", other, err)
	}
	if _, err := s.GetAudio(userCtx(), &dictionaryv1.GetAudioRequest{Id: "../../etc/passwd"}); code(err) != codes.NotFound {
		t.Fatalf("bad id: %v", err)
	}
}

func TestUploadAudioRejects(t *testing.T) {
	s, _, _ := audioService(t, 1<<10, 1<<20)
	for name, c := range map[string]struct {
		data []byte
		code string
	}{
		"png":   {pngBytes(t, 8, 8, 1), CodeAudioInvalid},
		"text":  {[]byte("hello"), CodeAudioInvalid},
		"empty": {nil, CodeAudioInvalid},
		"large": {mp3Bytes(2000), CodeAudioTooLarge},
	} {
		_, err := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: c.data})
		if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.code) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// no volume: audio is off
	off := newService(newFakeStore(), &fakeDecks{})
	if _, err := off.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: mp3Bytes(10)}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeAudioDisabled) {
		t.Errorf("disabled: %v", err)
	}
	if _, err := off.GetAudio(userCtx(), &dictionaryv1.GetAudioRequest{Id: otherUUID}); code(err) != codes.FailedPrecondition {
		t.Errorf("get disabled: %v", err)
	}
	if _, err := s.UploadAudio(context.Background(), &dictionaryv1.UploadAudioRequest{Data: mp3Bytes(10)}); code(err) != codes.Unauthenticated {
		t.Errorf("anonymous: %v", err)
	}
}

func TestUploadAudioQuota(t *testing.T) {
	s, _, _ := audioService(t, 1<<10, 300)
	for i := 0; i < 2; i++ {
		if _, err := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: mp3Bytes(100 + i)}); err != nil {
			t.Fatal(i, err)
		}
	}
	_, err := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: mp3Bytes(120)})
	if code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), CodeAudioQuota) {
		t.Fatalf("got %v", err)
	}
}

func TestSaveWordAudioRefs(t *testing.T) {
	s, _, _ := audioService(t, 1<<10, 1<<20)
	up, err := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: mp3Bytes(50)})
	if err != nil {
		t.Fatal(err)
	}
	ref := "dictaud:" + up.Id
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", AudioUk: ref, AudioUs: "https://api.dictionaryapi.dev/a.mp3"}); err != nil {
		t.Fatal(err)
	}
	// the file of another user, an unknown one, a malformed reference and a URL off the list are refused
	for name, c := range map[string]struct {
		ctx context.Context
		uk  string
		msg string
	}{
		"other user": {otherCtx(), ref, CodeAudioUnknown},
		"unknown":    {userCtx(), "dictaud:99999999-9999-4999-8999-999999999999", CodeAudioUnknown},
		"malformed":  {userCtx(), "dictaud:nope", "audio must be"},
		"url":        {userCtx(), "https://evil.example/a.mp3", "audio must be"},
	} {
		_, err := s.SaveWord(c.ctx, &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", AudioUk: c.uk})
		if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// audio off: a reference has no row, so it is not found
	off := newService(newFakeStore(), &fakeDecks{})
	if _, err := off.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", AudioUk: ref}); code(err) != codes.InvalidArgument {
		t.Errorf("disabled: %v", err)
	}
}

func TestExportAttachesAudio(t *testing.T) {
	s, _, decks := audioService(t, 1<<10, 1<<20)
	data := mp3Bytes(50)
	up, _ := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: data})
	ref := "dictaud:" + up.Id
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", AudioUk: ref}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); err != nil {
		t.Fatal(err)
	}
	if len(decks.built) != 1 {
		t.Fatalf("built %d", len(decks.built))
	}
	f, ok := decks.built[0].AudioFiles[ref]
	if !ok || string(f.Data) != string(data) || f.Ext != "mp3" {
		t.Fatalf("audio files %+v", decks.built[0].AudioFiles)
	}
}
