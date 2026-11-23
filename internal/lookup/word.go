// Package lookup finds the entry of a word in the open dictionary APIs behind the Provider interface.
package lookup

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Dictionary identifies the source of a lookup.
type Dictionary string

const (
	English           Dictionary = "english"
	EnglishVietnamese Dictionary = "english-vietnamese"
)

var (
	// ErrNotFound: no provider has an entry for the word.
	ErrNotFound = errors.New("lookup: word not found")
	// ErrInvalidWord: the text is not a word this service looks up.
	ErrInvalidWord = errors.New("lookup: invalid word")
	// ErrUnavailable: a provider failed (network, 5xx, rate limit); the word may exist.
	ErrUnavailable = errors.New("lookup: provider unavailable")
)

// Meaning is one definition with its examples. MachineTranslated is set when the definition is a
// machine translation of an English one (no human entry was found).
type Meaning struct {
	Definition        string
	Examples          []string
	MachineTranslated bool
}

// Word is the entry of a word. Values returned by a Service are shared (cached): do not modify them.
type Word struct {
	Text         string
	Phonetic     string
	PhoneticUK   string
	PhoneticUS   string
	AudioUK      string
	AudioUS      string
	PartOfSpeech []string
	Meanings     []Meaning
}

// Provider looks one (normalized) word up. It returns ErrNotFound when there is no entry and an error
// that wraps ErrUnavailable when it could not tell.
type Provider interface {
	Lookup(ctx context.Context, word string) (*Word, error)
}

// Translator translates English text into Vietnamese.
type Translator interface {
	Translate(ctx context.Context, text string) (string, error)
}

// MaxWordRunes is the longest word (or short phrase) that is looked up or saved.
const MaxWordRunes = 64

// NormalizeWord trims the word, collapses inner spaces and lower-cases it. Letters, digits, space,
// apostrophe, hyphen and dot are accepted: the text goes into URLs and keys, nothing else is needed for
// an entry of a dictionary.
func NormalizeWord(s string) (string, error) {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	if s == "" || utf8.RuneCountInString(s) > MaxWordRunes {
		return "", ErrInvalidWord
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '\'' || r == '-' || r == '.' {
			continue
		}
		return "", ErrInvalidWord
	}
	return s, nil
}
