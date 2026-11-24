package cardref

import (
	"reflect"
	"testing"
)

const a = "11111111-1111-4111-8111-111111111111"
const b = "22222222-2222-4222-8222-222222222222"

func TestIDs(t *testing.T) {
	got := IDs(`<p>x</p><img src="dictimg:` + a + `"><img src="dictimg:` + b + `"><img src="dictimg:` + a + `">`)
	if !reflect.DeepEqual(got, []string{a, b}) {
		t.Fatalf("got %v", got)
	}
	if IDs("<p>no images</p>") != nil || IDs(`<img src="dictimg:nope">`) != nil {
		t.Fatal("expected none")
	}
}

func TestReplaceTags(t *testing.T) {
	in := `x<img src="dictimg:` + a + `">y<img src="dictimg:` + b + `" alt="z">`
	out := ReplaceTags(in, func(id string) string {
		if id == a {
			return "[A]"
		}
		return ""
	})
	if out != "x[A]y" {
		t.Fatalf("got %q", out)
	}
}

func TestReplaceTagsKeepsWidth(t *testing.T) {
	in := `<img data-x="1" width="350" src="dictimg:` + a + `"><img src="dictimg:` + b + `">`
	out := ReplaceTags(in, func(id string) string { return `<img src="` + id[:1] + `.webp">` })
	if out != `<img width="350" src="1.webp"><img src="2.webp">` {
		t.Fatalf("got %q", out)
	}
}
