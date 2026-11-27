package service

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/gocommon/authn"
	"github.com/huponx/gocommon/jwtx"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
)

const otherUUID = "0c0c0c0c-1111-4222-8333-444444444444"

func otherCtx() context.Context {
	return authn.WithPrincipal(context.Background(), &authn.Principal{Subject: otherUUID, ClientID: "gatekeeper", Claims: &jwtx.Claims{}})
}

func imageService(t *testing.T, quota int64) (*Service, *fakeStore, *fakeDecks) {
	t.Helper()
	st, d := newFakeStore(), &fakeDecks{}
	s := newService(st, d).WithImages(&filestore.Store{Dir: t.TempDir()},
		filestore.ImageOptions{MaxBytes: 300 << 10, MaxDim: 1024, MaxPixels: 25_000_000}, quota)
	return s, st, d
}

func pngBytes(t *testing.T, w, h int, shade uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = shade, byte(i), 7, 255
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestUploadAndGetImage(t *testing.T) {
	s, _, _ := imageService(t, 1<<20)
	up, err := s.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{ContentType: "image/png", Data: pngBytes(t, 1500, 800, 1)})
	if err != nil || up.Id == "" {
		t.Fatalf("%+v %v", up, err)
	}
	got, err := s.GetImage(userCtx(), &dictionaryv1.GetImageRequest{Id: up.Id})
	if err != nil || got.ContentType != "image/jpeg" {
		t.Fatalf("%+v %v", got, err)
	}
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(got.Data))
	if cfg.Width != 1024 || cfg.Height != 546 {
		t.Fatalf("size %dx%d", cfg.Width, cfg.Height)
	}
	// the same picture is stored once
	again, err := s.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 1500, 800, 1)})
	if err != nil || again.Id != up.Id {
		t.Fatalf("dedup: %+v %v", again, err)
	}
	// another user neither sees it nor shares it
	if _, err := s.GetImage(otherCtx(), &dictionaryv1.GetImageRequest{Id: up.Id}); code(err) != codes.NotFound {
		t.Fatalf("other user: %v", err)
	}
	other, err := s.UploadImage(otherCtx(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 1500, 800, 1)})
	if err != nil || other.Id == up.Id {
		t.Fatalf("other upload: %+v %v", other, err)
	}
	if _, err := s.GetImage(userCtx(), &dictionaryv1.GetImageRequest{Id: "../../etc/passwd"}); code(err) != codes.NotFound {
		t.Fatalf("bad id: %v", err)
	}
}

func TestUploadImageRejects(t *testing.T) {
	s, _, _ := imageService(t, 1<<20)
	for name, data := range map[string][]byte{
		"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"text":  []byte("hello"),
		"empty": nil,
	} {
		if _, err := s.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{ContentType: "image/png", Data: data}); code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// no volume: images are off
	off := newService(newFakeStore(), &fakeDecks{})
	if _, err := off.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 8, 8, 1)}); code(err) != codes.FailedPrecondition {
		t.Errorf("disabled: %v", err)
	}
	if _, err := s.UploadImage(context.Background(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 8, 8, 1)}); code(err) != codes.Unauthenticated {
		t.Errorf("anonymous: %v", err)
	}
}

func TestUploadImageQuota(t *testing.T) {
	s, _, _ := imageService(t, 2000)
	if _, err := s.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 20, 20, 1)}); err != nil {
		t.Fatal(err)
	}
	var err error
	for i := 2; i < 40 && err == nil; i++ {
		_, err = s.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 20+i, 20+i, uint8(i))})
	}
	if code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), CodeImageQuota) {
		t.Fatalf("got %v", err)
	}
}

func TestSaveWordRefs(t *testing.T) {
	s, _, _ := imageService(t, 1<<20)
	up, err := s.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 30, 30, 2)})
	if err != nil {
		t.Fatal(err)
	}
	card := `<p>x</p><img src="dictimg:` + up.Id + `">`
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", Content: card}); err != nil {
		t.Fatal(err)
	}
	// the image of another user cannot be used, nor an unknown one
	if _, err := s.SaveWord(otherCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", Content: card}); code(err) != codes.InvalidArgument {
		t.Fatalf("other user's image: %v", err)
	}
	unknown := `<img src="dictimg:99999999-9999-4999-8999-999999999999">`
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", Content: unknown}); code(err) != codes.InvalidArgument {
		t.Fatalf("unknown image: %v", err)
	}
	var many strings.Builder
	for i := 0; i < 21; i++ {
		many.WriteString(`<img src="dictimg:` + strings.Replace("00000000-0000-4000-8000-0000000000XX", "XX", string(rune('a'+i))+string(rune('a'+i)), 1) + `">`)
	}
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", Content: many.String()}); code(err) != codes.InvalidArgument {
		t.Fatalf("too many images: %v", err)
	}
}

func TestExportAttachesImages(t *testing.T) {
	s, _, decks := imageService(t, 1<<20)
	up, _ := s.UploadImage(userCtx(), &dictionaryv1.UploadImageRequest{Data: pngBytes(t, 30, 30, 3)})
	card := `<img src="dictimg:` + up.Id + `">`
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello", Content: card}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); err != nil {
		t.Fatal(err)
	}
	if len(decks.built) != 1 {
		t.Fatalf("built %d", len(decks.built))
	}
	img, ok := decks.built[0].Images[up.Id]
	if !ok || len(img.Data) == 0 || img.Ext != "jpg" {
		t.Fatalf("images %+v", decks.built[0].Images)
	}
}
