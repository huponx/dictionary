// Package cardref finds the images a card content refers to. The editor writes an image as
// <img src="dictimg:<id>">; the id is the one UploadImage returned.
package cardref

import (
	"regexp"
	"strings"
)

// Prefix is the scheme of the image references in a card content.
const Prefix = "dictimg:"

const uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

var (
	idRe = regexp.MustCompile(Prefix + `(` + uuidPattern + `)`)
	// tagRe is a whole <img> tag whose src is a reference.
	widthRe = regexp.MustCompile(`(?i)\bwidth\s*=\s*["'](\d{1,4})["']`)
	tagRe   = regexp.MustCompile(`(?i)<img\b[^>]*?\bsrc\s*=\s*["']` + Prefix + `(` + uuidPattern + `)["'][^>]*>`)
)

// IDs returns the distinct ids a content refers to, in order of appearance.
func IDs(content string) []string {
	if !strings.Contains(content, Prefix) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range idRe.FindAllStringSubmatch(content, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// ReplaceTags rewrites every <img> tag that refers to an image: fn gets the id and returns the new HTML
// (an empty string drops the tag).
func ReplaceTags(content string, fn func(id string) string) string {
	return tagRe.ReplaceAllStringFunc(content, func(tag string) string {
		out := fn(tagRe.FindStringSubmatch(tag)[1])
		// the size the user gave the image stays on the tag fn made
		if w := widthRe.FindStringSubmatch(tag); w != nil && strings.HasPrefix(out, "<img") {
			out = "<img width=\"" + w[1] + "\"" + out[len("<img"):]
		}
		return out
	})
}
