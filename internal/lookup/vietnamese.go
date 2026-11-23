package lookup

import (
	"context"
	"errors"
	"sync"
)

// Vietnamese is the english-vietnamese dictionary: the Vietnamese meanings come from the Vietnamese
// Wiktionary; the phonetics and audio fill in from the English dictionary. When the Wiktionary has no
// entry, the first English definitions are machine translated (Translator not nil) or the English
// meanings are returned as they are.
type Vietnamese struct {
	English    Provider
	Wiktionary Provider
	// Translator is the machine translation fallback; nil turns it off.
	Translator Translator
	// MaxTranslated is the number of English definitions translated (default 3), to spare the quota.
	MaxTranslated int
}

// Lookup implements Provider.
func (v *Vietnamese) Lookup(ctx context.Context, word string) (*Word, error) {
	var (
		wg           sync.WaitGroup
		en, vi       *Word
		enErr, viErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); en, enErr = v.English.Lookup(ctx, word) }()
	go func() { defer wg.Done(); vi, viErr = v.Wiktionary.Lookup(ctx, word) }()
	wg.Wait()

	// An error other than "not found" on a side that decides the answer is not hidden: the word may exist.
	switch {
	case viErr == nil:
		return mergeVi(vi, en), nil
	case !errors.Is(viErr, ErrNotFound):
		return nil, viErr
	case enErr != nil:
		return nil, enErr // not found, or unavailable
	}
	return v.fromEnglish(ctx, en), nil
}

// mergeVi keeps the Vietnamese meanings of the Wiktionary and takes from the English entry what it lacks.
func mergeVi(vi, en *Word) *Word {
	out := *vi
	if en == nil {
		return &out
	}
	setFirst(&out.Phonetic, en.Phonetic)
	setFirst(&out.PhoneticUK, en.PhoneticUK)
	setFirst(&out.PhoneticUS, en.PhoneticUS)
	setFirst(&out.AudioUK, en.AudioUK)
	setFirst(&out.AudioUS, en.AudioUS)
	return &out
}

// fromEnglish builds the entry of a word the Wiktionary does not have.
func (v *Vietnamese) fromEnglish(ctx context.Context, en *Word) *Word {
	if v.Translator == nil {
		return en
	}
	limit := v.MaxTranslated
	if limit <= 0 {
		limit = 3
	}
	out := *en
	out.Meanings = nil
	for _, m := range en.Meanings {
		if len(out.Meanings) >= limit {
			break
		}
		vi, err := v.Translator.Translate(ctx, m.Definition)
		if err != nil {
			// the quota is gone or the service is down: the English meanings are better than nothing
			if len(out.Meanings) == 0 {
				return en
			}
			break
		}
		out.Meanings = append(out.Meanings, Meaning{Definition: vi, Examples: m.Examples, MachineTranslated: true})
	}
	return &out
}
