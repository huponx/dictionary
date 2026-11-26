package service

import (
	"strings"
	"testing"

	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestSettingsStorageDefaultsAndValidation(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	g, err := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	st := g.Settings
	if st.MaxContentBytes != 64<<10 || st.ContentUserQuota != 256<<20 || st.ImageUserQuota != 20<<20 || st.AudioUserQuota != 10<<20 ||
		st.MaxContentBytesCeiling != 256<<10 || st.ContentUserQuotaCeiling != 1<<30 {
		t.Fatalf("defaults: %+v", st)
	}
	// a client that predates the fields keeps the values
	in := g.Settings
	in.MaxContentBytes, in.ContentUserQuota, in.ImageUserQuota, in.AudioUserQuota = 0, 0, 0, 0
	if r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil || r.Settings.MaxContentBytes != 64<<10 || r.Settings.ImageUserQuota != 20<<20 {
		t.Fatalf("old client: %+v, %v", r, err)
	}
	for name, mut := range map[string]func(*dictionaryv1.Settings){
		"content above the ceiling": func(x *dictionaryv1.Settings) { x.MaxContentBytes = 256<<10 + 1 },
		"content below the minimum": func(x *dictionaryv1.Settings) { x.MaxContentBytes = 100 },
		"content quota too big":     func(x *dictionaryv1.Settings) { x.ContentUserQuota = 1<<30 + 1 },
		"image quota too big":       func(x *dictionaryv1.Settings) { x.ImageUserQuota = 1<<30 + 1 },
		"audio quota negative":      func(x *dictionaryv1.Settings) { x.AudioUserQuota = -1 },
	} {
		bad := proto.Clone(g.Settings).(*dictionaryv1.Settings)
		mut(bad)
		if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: bad}); code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := proto.Clone(g.Settings).(*dictionaryv1.Settings)
	ok.MaxContentBytes, ok.ContentUserQuota, ok.ImageUserQuota, ok.AudioUserQuota = 256<<10, 1<<30, 1<<30, 5<<20
	ok.MaxContentBytesCeiling = 1 // read only: ignored
	if r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: ok}); err != nil ||
		r.Settings.MaxContentBytes != 256<<10 || r.Settings.AudioUserQuota != 5<<20 || r.Settings.MaxContentBytesCeiling != 256<<10 {
		t.Fatalf("valid: %+v, %v", r, err)
	}
}

// The content size an administrator sets is the limit of SaveWord: 100 KiB is refused at 64 KiB and accepted at 128 KiB.
func TestMaxContentBytesIsASetting(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	big := strings.Repeat("x", 100<<10)
	_, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "hello", Content: big})
	if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("at 64 KiB: %v", err)
	}
	g, _ := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	g.Settings.MaxContentBytes = 128 << 10
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: g.Settings}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "hello", Content: big}); err != nil {
		t.Fatalf("at 128 KiB: %v", err)
	}
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "hello", Content: strings.Repeat("x", 128<<10+1)}); code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "128 KiB") {
		t.Fatalf("above 128 KiB: %v", err)
	}
}

func setSettings(t *testing.T, s *Service, mut func(*dictionaryv1.Settings)) {
	t.Helper()
	g, err := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	mut(g.Settings)
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: g.Settings}); err != nil {
		t.Fatal(err)
	}
}

func TestSaveWordContentQuota(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	setSettings(t, s, func(x *dictionaryv1.Settings) { x.ContentUserQuota = 2 << 10 })
	save := func(word string, n int) error {
		_, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: word, Content: strings.Repeat("x", n)})
		return err
	}
	if err := save("hello", 1500); err != nil {
		t.Fatal(err)
	}
	err := save("world", 600)
	if code(err) != codes.ResourceExhausted || !strings.HasPrefix(status.Convert(err).Message(), CodeMaxSavedBytes) {
		t.Fatalf("over the quota: %v", err)
	}
	if err := save("world", 548); err != nil { // 1500 + 548 = 2048
		t.Fatal(err)
	}
	// the own quota of the user wins over the setting
	if _, err := s.SetUserQuota(userCtx(), &dictionaryv1.SetUserQuotaRequest{UserId: userUUID, Quotas: &dictionaryv1.StorageQuota{Content: 10 << 10}}); err != nil {
		t.Fatal(err)
	}
	if err := save("hello", 5000); err != nil {
		t.Fatalf("own quota: %v", err)
	}
	if _, err := s.SetUserQuota(userCtx(), &dictionaryv1.SetUserQuotaRequest{UserId: userUUID}); err != nil {
		t.Fatal(err)
	}
	// back to 2 KiB while the user holds more: shrinking works, growing does not
	if err := save("hello", 6000); code(err) != codes.ResourceExhausted {
		t.Fatalf("grow over the quota: %v", err)
	}
	if err := save("hello", 100); err != nil {
		t.Fatalf("shrink while over: %v", err)
	}
}

func TestUserQuotaRPCs(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	setSettings(t, s, func(x *dictionaryv1.Settings) { x.ImageUserQuota = 5 << 20 })
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "hello", Content: "12345"}); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetUserQuota(userCtx(), &dictionaryv1.GetUserQuotaRequest{UserId: userUUID})
	if err != nil || g.Used.Content != 5 || g.Override.Content != 0 || g.Effective.Content != 256<<20 || g.Effective.Image != 5<<20 || g.Effective.Audio != 10<<20 {
		t.Fatalf("get: %+v, %v", g, err)
	}
	r, err := s.SetUserQuota(userCtx(), &dictionaryv1.SetUserQuotaRequest{UserId: userUUID, Quotas: &dictionaryv1.StorageQuota{Image: 50 << 20, Audio: 1 << 20}})
	if err != nil || r.Override.Image != 50<<20 || r.Effective.Image != 50<<20 || r.Effective.Audio != 1<<20 || r.Effective.Content != 256<<20 {
		t.Fatalf("set: %+v, %v", r, err)
	}
	// setting replaces all three: the missing kind goes back to the setting
	r, err = s.SetUserQuota(userCtx(), &dictionaryv1.SetUserQuotaRequest{UserId: userUUID, Quotas: &dictionaryv1.StorageQuota{Audio: 2 << 20}})
	if err != nil || r.Override.Image != 0 || r.Effective.Image != 5<<20 || r.Effective.Audio != 2<<20 {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	for name, req := range map[string]*dictionaryv1.SetUserQuotaRequest{
		"not a uuid":    {UserId: "nope"},
		"above ceiling": {UserId: userUUID, Quotas: &dictionaryv1.StorageQuota{Content: 1<<30 + 1}},
		"negative":      {UserId: userUUID, Quotas: &dictionaryv1.StorageQuota{Image: -5}},
		"below minimum": {UserId: userUUID, Quotas: &dictionaryv1.StorageQuota{Audio: 10}},
	} {
		if _, err := s.SetUserQuota(userCtx(), req); code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.GetUserQuota(userCtx(), &dictionaryv1.GetUserQuotaRequest{UserId: "nope"}); code(err) != codes.InvalidArgument {
		t.Errorf("get, not a uuid: %v", err)
	}
	st.unknown = true
	if _, err := s.GetUserQuota(userCtx(), &dictionaryv1.GetUserQuotaRequest{UserId: otherUUID}); code(err) != codes.NotFound {
		t.Errorf("get, unknown user: %v", err)
	}
	if _, err := s.SetUserQuota(userCtx(), &dictionaryv1.SetUserQuotaRequest{UserId: otherUUID}); code(err) != codes.NotFound {
		t.Errorf("set, unknown user: %v", err)
	}
}

func TestGetMyStorage(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "hello", Content: "12345"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetMyStorage(userCtx(), &dictionaryv1.GetMyStorageRequest{})
	if err != nil || r.Content.Used != 5 || r.Content.Limit != 256<<20 || r.Image.Limit != 20<<20 || r.Audio.Limit != 10<<20 {
		t.Fatalf("without an override: %+v, %v", r, err)
	}
	if _, err := s.SetUserQuota(userCtx(), &dictionaryv1.SetUserQuotaRequest{UserId: userUUID, Quotas: &dictionaryv1.StorageQuota{Content: 8 << 20, Image: 1 << 20}}); err != nil {
		t.Fatal(err)
	}
	r, err = s.GetMyStorage(userCtx(), &dictionaryv1.GetMyStorageRequest{})
	if err != nil || r.Content.Limit != 8<<20 || r.Image.Limit != 1<<20 || r.Audio.Limit != 10<<20 {
		t.Fatalf("with an override: %+v, %v", r, err)
	}
	if _, err := s.GetMyStorage(t.Context(), &dictionaryv1.GetMyStorageRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("anonymous: %v", err)
	}
}

// The quota of images and audio is the own quota of the user when there is one, else the setting, which starts as
// the value of the configuration.
func TestUploadQuotaOverride(t *testing.T) {
	s, st, _ := audioService(t, 1<<10, 300)
	up := func(n int) error {
		_, err := s.UploadAudio(userCtx(), &dictionaryv1.UploadAudioRequest{Data: mp3Bytes(n)})
		return err
	}
	if err := up(200); err != nil {
		t.Fatal(err)
	}
	if err := up(150); code(err) != codes.ResourceExhausted {
		t.Fatalf("over the configured default: %v", err)
	}
	q := int64(1000)
	st.quotas.Audio = &q
	if err := up(150); err != nil {
		t.Fatalf("own quota: %v", err)
	}
	setSettings(t, s, func(x *dictionaryv1.Settings) { x.AudioUserQuota = 2 << 10 })
	st.quotas.Audio = nil
	if err := up(160); err != nil {
		t.Fatalf("the setting wins over the configured default: %v", err)
	}
}
