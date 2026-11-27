package settings

import (
	"encoding/json"
	"reflect"
	"testing"
)

// testDefaults is a system with a registry of three dictionaries: two on, one (a new dictionary) off.
func testDefaults() System {
	return Defaults(100, 168).WithDictionaries(
		[]string{"english", "english-vietnamese", "vietnamese-japanese"},
		map[string]bool{"english": true, "english-vietnamese": true})
}

func TestSystemDefaultsAndRows(t *testing.T) {
	def := testDefaults()
	if got := FromRows(nil, def); !reflect.DeepEqual(got, def) {
		t.Fatalf("no rows: %+v", got)
	}
	rows := map[string]json.RawMessage{
		"max_saved_words": json.RawMessage(`250`), "allow_export_link": json.RawMessage(`false`),
		"unknown": json.RawMessage(`1`), "dictionaries": json.RawMessage(`"yes"`), // wrong type: default
	}
	got := FromRows(rows, def)
	want := System{MaxSavedWords: 250, AllowExportDeck: true, AllowExportLink: false, Dictionaries: def.Dictionaries, DictionaryOrder: def.DictionaryOrder, RemoveAfterExport: true, ExportedRetentionHours: 168, MaxCategories: 50, MaxCategoryNameLength: 40,
		ExportMaxBytes: DefaultExportMaxBytes, ExportPartMaxBytes: DefaultExportPartMaxBytes, ExportSyncMaxWords: DefaultExportSyncWords,
		ExportMaxBytesCeiling: DefaultExportMaxBytesCeiling, ExportPartMaxBytesCeiling: DefaultExportPartCeiling,
		MaxContentBytes: DefaultMaxContentBytes, ContentUserQuota: DefaultContentUserQuota, ImageUserQuota: DefaultImageUserQuota, AudioUserQuota: DefaultAudioUserQuota,
		MaxContentBytesCeiling: DefaultMaxContentCeiling, ContentUserQuotaCeiling: DefaultContentUserQuotaCeil,
		ImageUserQuotaCeiling: DefaultImageUserQuotaCeil, AudioUserQuotaCeiling: DefaultAudioUserQuotaCeil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSystemDictionariesFromRows(t *testing.T) {
	def := testDefaults()
	// a missing entry takes the registry default: the two old ones on, a new one off
	got := FromRows(map[string]json.RawMessage{"dictionaries": json.RawMessage(`{"english":false,"removed":true}`)}, def)
	want := map[string]bool{"english": false, "english-vietnamese": true, "vietnamese-japanese": false}
	if !reflect.DeepEqual(got.Dictionaries, want) {
		t.Fatalf("got %v want %v", got.Dictionaries, want)
	}
	if !def.Dictionaries["english"] {
		t.Fatal("the defaults were changed through the shared map")
	}
	if !reflect.DeepEqual(FromRows(nil, def).Dictionaries, map[string]bool{"english": true, "english-vietnamese": true, "vietnamese-japanese": false}) {
		t.Fatal("no row: the registry defaults")
	}
	// the legacy keys are not read
	if got := FromRows(map[string]json.RawMessage{"english_enabled": json.RawMessage(`false`)}, def); !got.Dictionaries["english"] {
		t.Fatal("legacy key read")
	}
	// the row is the whole map, and only a change of it is written
	next := def
	next.Dictionaries = map[string]bool{"english": true, "english-vietnamese": true, "vietnamese-japanese": true}
	ch := Changed(def, next)
	if len(ch) != 1 || string(ch["dictionaries"]) != `{"english":true,"english-vietnamese":true,"vietnamese-japanese":true}` {
		t.Fatalf("%v", ch)
	}
	// validation
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
	next.Dictionaries = map[string]bool{"english": false, "english-vietnamese": false, "vietnamese-japanese": false}
	if next.Validate() == nil {
		t.Error("no dictionary on accepted")
	}
	next.Dictionaries = map[string]bool{"english": true, "latin": true}
	if next.Validate() == nil {
		t.Error("an unknown dictionary accepted")
	}
}

func TestChangedWritesOnlyWhatDiffers(t *testing.T) {
	cur := testDefaults()
	next := cur
	next.MaxSavedWords = 50
	next.AllowExportLink = false
	ch := Changed(cur, next)
	if len(ch) != 2 || string(ch["max_saved_words"]) != "50" || string(ch["allow_export_link"]) != "false" {
		t.Fatalf("%v", ch)
	}
	if len(Changed(cur, cur)) != 0 {
		t.Fatal("nothing changed, nothing written")
	}
}

func TestSystemValidate(t *testing.T) {
	ok := testDefaults()
	on := map[string]bool{"english": true}
	order := []string{"english"}
	bad := []System{
		{MaxSavedWords: 0, Dictionaries: on, DictionaryOrder: order},
		{MaxSavedWords: MaxSavedWordsLimit + 1, Dictionaries: on, DictionaryOrder: order},
		{MaxSavedWords: 10},
		{MaxSavedWords: 10, Dictionaries: on, DictionaryOrder: order, ExportedRetentionHours: 0},
		{MaxSavedWords: 10, Dictionaries: on, DictionaryOrder: order, ExportedRetentionHours: MaxRetentionHours + 1},
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	big := ok
	big.MaxSavedWords = 1_000_000
	if err := big.Validate(); err != nil {
		t.Errorf("a million words: %v", err)
	}
	for i, s := range bad {
		if s.Validate() == nil {
			t.Errorf("%d accepted", i)
		}
	}
}

func TestUserRoundTripAndDefaults(t *testing.T) {
	if got := ParseUser(""); !reflect.DeepEqual(got, UserDefaults()) {
		t.Fatalf("%+v", got)
	}
	if got := ParseUser("{not json"); !reflect.DeepEqual(got, UserDefaults()) {
		t.Fatalf("%+v", got)
	}
	u := User{Dictionaries: map[string]bool{"english": false, "english-vietnamese": true}, DefaultDictionary: "english-vietnamese", DeckName: "Words", MaxLookupWords: 5, RemoveAfterExport: false, RemoveAcknowledged: true, FreshDeck: true,
		ReviewAlgorithm: AlgorithmFSRS, DesiredRetention: 85}
	if got := ParseUser(u.JSON()); !reflect.DeepEqual(got, u) {
		t.Fatalf("%+v", got)
	}
	if UserDefaults().FreshDeck || ParseUser(`{"deckName":"x"}`).FreshDeck {
		t.Fatal("a deck is not fresh unless the user chose it")
	}
	// a missing key is the default, not false
	if got := ParseUser(`{"deckName":"x"}`); got.Dictionaries != nil || got.DeckName != "x" || got.MaxLookupWords != MaxLookupLimit {
		t.Fatalf("%+v", got)
	}
}

func TestUserValidateAgainstSystem(t *testing.T) {
	sys := testDefaults()
	u := UserDefaults()
	if err := u.Validate(sys); err != nil {
		t.Fatal(err)
	}
	cases := map[string]User{
		"no dictionary":       {Dictionaries: map[string]bool{"english": false, "english-vietnamese": false}, MaxLookupWords: 5, DefaultDictionary: "english"},
		"default is off":      {Dictionaries: map[string]bool{"english": false}, DefaultDictionary: "english", MaxLookupWords: 5},
		"unknown default":     {DefaultDictionary: "latin", MaxLookupWords: 5},
		"unknown dictionary":  {Dictionaries: map[string]bool{"latin": true}, DefaultDictionary: "english", MaxLookupWords: 5},
		"off by the system":   {DefaultDictionary: "vietnamese-japanese", MaxLookupWords: 5},
		"too many per lookup": {DefaultDictionary: "english", MaxLookupWords: MaxLookupLimit + 1},
		"none per lookup":     {DefaultDictionary: "english"},
	}
	for name, c := range cases {
		if c.Validate(sys) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// the system turned a dictionary off: the user cannot choose it
	noVi := sys.WithDictionaries(sys.DictionaryOrder, map[string]bool{"english": true})
	if (User{DefaultDictionary: "english-vietnamese", MaxLookupWords: 5, ReviewAlgorithm: AlgorithmSM2, DesiredRetention: 90}).Validate(noVi) == nil {
		t.Error("a dictionary the system turned off was accepted")
	}
}

func TestEffectiveFollowsTheSystem(t *testing.T) {
	sys := testDefaults()
	sys.Dictionaries = map[string]bool{"english": false, "english-vietnamese": true, "vietnamese-japanese": false}
	got := User{DefaultDictionary: "english"}.Effective(sys) // English was the default
	want := map[string]bool{"english": false, "english-vietnamese": true, "vietnamese-japanese": false}
	if !reflect.DeepEqual(got.Dictionaries, want) || got.DefaultDictionary != "english-vietnamese" {
		t.Fatalf("%+v", got)
	}
	// never chosen: the first one that is on
	if got := UserDefaults().Effective(sys); got.DefaultDictionary != "english-vietnamese" {
		t.Fatalf("%+v", got)
	}
	// a dictionary the user turned off stays off, a new one the system turned on is on for the user
	sys.Dictionaries["vietnamese-japanese"] = true
	got = User{Dictionaries: map[string]bool{"english-vietnamese": false}}.Effective(sys)
	if !reflect.DeepEqual(got.Dictionaries, map[string]bool{"english": false, "english-vietnamese": false, "vietnamese-japanese": true}) || got.DefaultDictionary != "vietnamese-japanese" {
		t.Fatalf("%+v", got)
	}
}

func TestRemoveAfterExport(t *testing.T) {
	// a missing key is on, for the system and for the user
	if got := FromRows(map[string]json.RawMessage{}, testDefaults()); !got.RemoveAfterExport || got.ExportedRetentionHours != 168 {
		t.Fatalf("%+v", got)
	}
	if !ParseUser(`{"deckName":"x"}`).RemoveAfterExport {
		t.Fatal("the user default is on")
	}
	if u := ParseUser(`{"removeAfterExport":false}`); u.RemoveAfterExport {
		t.Fatal("false must stay false")
	}
	// retention rows: a valid number is read, a bad one is the default
	rows := map[string]json.RawMessage{"exported_retention_hours": json.RawMessage(`24`), "remove_after_export": json.RawMessage(`false`)}
	got := FromRows(rows, testDefaults())
	if got.ExportedRetentionHours != 24 || got.RemoveAfterExport {
		t.Fatalf("%+v", got)
	}
	rows["exported_retention_hours"] = json.RawMessage(`0`)
	if got := FromRows(rows, testDefaults()); got.ExportedRetentionHours != 168 {
		t.Fatalf("%+v", got)
	}
	// effective = system AND user; Effective keeps the choice of the user, so saving it back does not lose it
	sys, u := testDefaults(), UserDefaults()
	if !u.RemovesAfterExport(sys) {
		t.Fatal("both on")
	}
	sys.RemoveAfterExport = false
	if u.RemovesAfterExport(sys) || !u.Effective(sys).RemoveAfterExport {
		t.Fatal("the system is off")
	}
	sys.RemoveAfterExport, u.RemoveAfterExport = true, false
	if u.RemovesAfterExport(sys) {
		t.Fatal("the user is off")
	}
}

func TestReviewNewPerDay(t *testing.T) {
	if UserDefaults().ReviewNewPerDay != DefaultReviewNewPerDay || ParseUser(`{"deckName":"x"}`).ReviewNewPerDay != DefaultReviewNewPerDay {
		t.Fatal("a missing key is the default")
	}
	// 0 is a choice (no new words), not a missing key
	if got := ParseUser(`{"reviewNewPerDay":0}`); got.ReviewNewPerDay != 0 {
		t.Fatalf("%+v", got)
	}
	u := UserDefaults()
	u.ReviewNewPerDay = 0
	if got := ParseUser(u.JSON()); got.ReviewNewPerDay != 0 {
		t.Fatalf("0 did not survive the round trip: %s", u.JSON())
	}
	for raw, want := range map[string]int{`{"reviewNewPerDay":100}`: 100, `{"reviewNewPerDay":101}`: DefaultReviewNewPerDay, `{"reviewNewPerDay":-1}`: DefaultReviewNewPerDay} {
		if got := ParseUser(raw).ReviewNewPerDay; got != want {
			t.Errorf("%s: %d", raw, got)
		}
	}
	sys := testDefaults()
	for n, ok := range map[int]bool{0: true, 1: true, 100: true, 101: false, -1: false} {
		u := UserDefaults()
		u.ReviewNewPerDay = n
		if (u.Validate(sys) == nil) != ok {
			t.Errorf("%d: %v", n, u.Validate(sys))
		}
	}
}

func TestReviewMaxPerDay(t *testing.T) {
	if UserDefaults().ReviewMaxPerDay != DefaultReviewMaxPerDay || ParseUser(`{"deckName":"x"}`).ReviewMaxPerDay != DefaultReviewMaxPerDay {
		t.Fatal("a missing key is the default")
	}
	// 0 is a choice (no review cards), not a missing key
	u := UserDefaults()
	u.ReviewMaxPerDay = 0
	if got := ParseUser(u.JSON()); got.ReviewMaxPerDay != 0 {
		t.Fatalf("0 did not survive the round trip: %s", u.JSON())
	}
	for raw, want := range map[string]int{`{"reviewMaxPerDay":0}`: 0, `{"reviewMaxPerDay":9999}`: 9999, `{"reviewMaxPerDay":10000}`: DefaultReviewMaxPerDay, `{"reviewMaxPerDay":-1}`: DefaultReviewMaxPerDay} {
		if got := ParseUser(raw).ReviewMaxPerDay; got != want {
			t.Errorf("%s: %d", raw, got)
		}
	}
	sys := testDefaults()
	for n, ok := range map[int]bool{0: true, 200: true, 9999: true, 10000: false, -1: false} {
		u := UserDefaults()
		u.ReviewMaxPerDay = n
		if (u.Validate(sys) == nil) != ok {
			t.Errorf("%d: %v", n, u.Validate(sys))
		}
	}
}

func TestCategoryLimitSettings(t *testing.T) {
	def := testDefaults()
	if def.MaxCategories != 50 || def.MaxCategoryNameLength != 40 {
		t.Fatalf("defaults: %+v", def)
	}
	got := FromRows(map[string]json.RawMessage{"max_categories": json.RawMessage(`9`), "max_category_name_length": json.RawMessage(`20`)}, def)
	if got.MaxCategories != 9 || got.MaxCategoryNameLength != 20 {
		t.Fatalf("rows: %+v", got)
	}
	for _, v := range []string{`0`, `201`, `"x"`, `-3`} {
		got := FromRows(map[string]json.RawMessage{"max_categories": json.RawMessage(v), "max_category_name_length": json.RawMessage(v)}, def)
		if got.MaxCategories != 50 || got.MaxCategoryNameLength != 40 {
			t.Errorf("%s not ignored: %+v", v, got)
		}
	}
	next := def
	next.MaxCategories = 5
	if ch := Changed(def, next); len(ch) != 1 || string(ch["max_categories"]) != "5" {
		t.Fatalf("%v", ch)
	}
	for _, mod := range []func(*System){
		func(s *System) { s.MaxCategories = 0 }, func(s *System) { s.MaxCategories = MaxCategoriesLimit + 1 },
		func(s *System) { s.MaxCategoryNameLength = 0 }, func(s *System) { s.MaxCategoryNameLength = MaxCategoryNameLimit + 1 },
	} {
		s := def
		mod(&s)
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

func TestUserReviewAlgorithm(t *testing.T) {
	if u := ParseUser(`{"reviewAlgorithm":"leitner","desiredRetention":99}`); u.ReviewAlgorithm != AlgorithmSM2 || u.DesiredRetention != 90 {
		t.Fatalf("%+v", u)
	}
	if u := ParseUser(`{"reviewAlgorithm":"fsrs","desiredRetention":80}`); u.ReviewAlgorithm != AlgorithmFSRS || u.DesiredRetention != 80 {
		t.Fatalf("%+v", u)
	}
	sys := testDefaults()
	u := UserDefaults()
	u.ReviewAlgorithm = "x"
	if u.Validate(sys) == nil {
		t.Fatal("unknown algorithm accepted")
	}
	u = UserDefaults()
	u.DesiredRetention = 98
	if u.Validate(sys) == nil {
		t.Fatal("retention above 97 accepted")
	}
}

func TestExportLimitsAreBoundedByTheCeilings(t *testing.T) {
	sys := testDefaults().WithExportLimits(1<<20, 2<<20, 4<<20, 8<<20)
	if err := sys.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*System){
		"sync over the ceiling":  func(s *System) { s.ExportMaxBytes = 4<<20 + 1 },
		"sync under the minimum": func(s *System) { s.ExportMaxBytes = MinExportBytes - 1 },
		"part over the ceiling":  func(s *System) { s.ExportPartMaxBytes = 8<<20 + 1 },
		"part under the minimum": func(s *System) { s.ExportPartMaxBytes = 0 },
	} {
		s := sys
		mut(&s)
		if s.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	edge := sys
	edge.ExportMaxBytes, edge.ExportPartMaxBytes = 4<<20, 8<<20
	if err := edge.Validate(); err != nil {
		t.Errorf("the ceilings themselves are allowed: %v", err)
	}
}

func TestExportLimitRowsAboveTheCeilingKeepTheDefault(t *testing.T) {
	def := testDefaults().WithExportLimits(1<<20, 2<<20, 4<<20, 8<<20)
	rows := map[string]json.RawMessage{
		keyExportMaxBytes: json.RawMessage(`3145728`), keyExportPartMaxBytes: json.RawMessage(`8388608`),
	}
	got := FromRows(rows, def)
	if got.ExportMaxBytes != 3<<20 || got.ExportPartMaxBytes != 8<<20 {
		t.Errorf("in range rows apply: %+v", got)
	}
	rows[keyExportMaxBytes] = json.RawMessage(`99999999`)
	if got := FromRows(rows, def); got.ExportMaxBytes != 1<<20 {
		t.Errorf("a row above the ceiling keeps the default: %d", got.ExportMaxBytes)
	}
}

func TestExportSyncMaxWords(t *testing.T) {
	def := testDefaults().WithExportSyncMaxWords(1000)
	if def.ExportSyncMaxWords != 1000 {
		t.Fatalf("default from the configuration: %d", def.ExportSyncMaxWords)
	}
	if got := FromRows(map[string]json.RawMessage{"export_sync_max_words": json.RawMessage(`300`)}, def); got.ExportSyncMaxWords != 300 {
		t.Errorf("stored value: %d", got.ExportSyncMaxWords)
	}
	for _, bad := range []string{`0`, `5001`, `"x"`} { // out of range or wrong type keeps the default
		if got := FromRows(map[string]json.RawMessage{"export_sync_max_words": json.RawMessage(bad)}, def); got.ExportSyncMaxWords != 1000 {
			t.Errorf("%s: %d", bad, got.ExportSyncMaxWords)
		}
	}
	for _, n := range []int{0, MaxExportSyncWords + 1} {
		s := def
		s.ExportSyncMaxWords = n
		if s.Validate() == nil {
			t.Errorf("Validate accepted %d", n)
		}
	}
	next := def
	next.ExportSyncMaxWords = 4000
	if ch := Changed(def, next); len(ch) != 1 || string(ch["export_sync_max_words"]) != "4000" {
		t.Errorf("changed: %v", ch)
	}
}

func TestStorageSettingsRowsAndLimits(t *testing.T) {
	def := testDefaults().WithStorageLimits(StorageLimits{
		MaxContentBytes: 32 << 10, MaxContentBytesCeiling: 128 << 10, ImageUserQuota: 5 << 20, ImageUserQuotaCeiling: 50 << 20,
	})
	if def.MaxContentBytes != 32<<10 || def.MaxContentBytesCeiling != 128<<10 || def.ImageUserQuota != 5<<20 || def.ImageUserQuotaCeiling != 50<<20 ||
		def.AudioUserQuota != DefaultAudioUserQuota {
		t.Fatalf("limits: %+v", def)
	}
	rows := map[string]json.RawMessage{
		"max_content_bytes": json.RawMessage(`100000`), "content_user_quota": json.RawMessage(`2048`),
		"image_user_quota": json.RawMessage(`99999999999`), // above the ceiling, which was lowered since: the default
		"audio_user_quota": json.RawMessage(`"big"`),       // wrong type: the default
	}
	got := FromRows(rows, def)
	if got.MaxContentBytes != 100000 || got.ContentUserQuota != 2048 || got.ImageUserQuota != 5<<20 || got.AudioUserQuota != DefaultAudioUserQuota {
		t.Fatalf("rows: %+v", got)
	}
	// only what differs is written, and it reads back
	next := got
	next.AudioUserQuota = 3 << 20
	next.MaxContentBytes = 40 << 10
	ch := Changed(got, next)
	if len(ch) != 2 || string(ch["audio_user_quota"]) != "3145728" || string(ch["max_content_bytes"]) != "40960" {
		t.Fatalf("changed: %v", ch)
	}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*System){
		"content above its ceiling": func(s *System) { s.MaxContentBytes = s.MaxContentBytesCeiling + 1 },
		"content tiny":              func(s *System) { s.MaxContentBytes = 10 },
		"quota above its ceiling":   func(s *System) { s.ImageUserQuota = s.ImageUserQuotaCeiling + 1 },
		"quota zero":                func(s *System) { s.ContentUserQuota = 0 },
	} {
		bad := next
		mut(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
