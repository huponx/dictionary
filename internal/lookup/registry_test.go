package lookup

import (
	"reflect"
	"testing"
)

func TestRegistryOrderAndGroups(t *testing.T) {
	r := NewRegistry().
		Register(Entry{ID: "english", Group: "en", Source: "en", Target: "en", DefaultOn: true}).
		Register(Entry{ID: "vietnamese-japanese", Group: "vi-ja", Source: "vi", Target: "ja"}).
		Register(Entry{ID: "english-vietnamese", Group: "en", Source: "en", Target: "vi", DefaultOn: true})

	if got, want := r.IDs(), []string{"english", "vietnamese-japanese", "english-vietnamese"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	groups := r.Groups()
	if len(groups) != 2 || groups[0].ID != "en" || groups[1].ID != "vi-ja" {
		t.Fatalf("groups = %+v", groups)
	}
	if len(groups[0].Entries) != 2 || groups[0].Entries[1].ID != "english-vietnamese" {
		t.Errorf("group en = %+v, the dictionaries of a group keep registration order", groups[0].Entries)
	}
	if g, ok := r.Group("vi-ja"); !ok || len(g.Entries) != 1 || g.Entries[0].Target != "ja" {
		t.Errorf("group vi-ja = %+v %v", g, ok)
	}
	if _, ok := r.Group("x"); ok {
		t.Error("unknown group")
	}
	if e, ok := r.Entry("english-vietnamese"); !ok || e.Group != "en" {
		t.Errorf("entry = %+v %v", e, ok)
	}
	if _, ok := r.Entry("latin"); ok {
		t.Error("unknown id")
	}
}

func TestRegistryRejectsMistakes(t *testing.T) {
	for name, f := range map[string]func(){
		"no id":    func() { NewRegistry().Register(Entry{Group: "en"}) },
		"no group": func() { NewRegistry().Register(Entry{ID: "x"}) },
		"twice":    func() { NewRegistry().Register(Entry{ID: "x", Group: "g"}).Register(Entry{ID: "x", Group: "g"}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic", name)
				}
			}()
			f()
		}()
	}
}
