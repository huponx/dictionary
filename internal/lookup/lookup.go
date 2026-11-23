package lookup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// workerLimit is the number of lookups of LookupMany that run at once (the providers are free services).
const workerLimit = 3

// Service looks words up in the provider of a dictionary and caches the entries.
type Service struct {
	providers map[Dictionary]Provider
	cache     *cache
}

// New builds the service. cacheTTL or cacheSize at zero turns the cache off.
func New(providers map[Dictionary]Provider, cacheTTL time.Duration, cacheSize int) *Service {
	return &Service{providers: providers, cache: newCache(cacheTTL, cacheSize)}
}

// Lookup returns the entry of one word: ErrInvalidWord, ErrNotFound, or an error wrapping
// ErrUnavailable. The result is shared, do not modify it.
func (s *Service) Lookup(ctx context.Context, d Dictionary, word string) (*Word, error) {
	p, ok := s.providers[d]
	if !ok {
		return nil, fmt.Errorf("lookup: no provider for dictionary %q", d)
	}
	word, err := NormalizeWord(word)
	if err != nil {
		return nil, err
	}
	key := string(d) + "\x00" + word
	if w, ok := s.cache.get(key); ok {
		return w, nil
	}
	w, err := p.Lookup(ctx, word)
	if err != nil {
		return nil, err
	}
	s.cache.put(key, w)
	return w, nil
}

// LookupMany looks the words up (at most workerLimit at a time), in order and without duplicates. Words
// without an entry come back in notFound; any other failure ends the call with that error.
func (s *Service) LookupMany(ctx context.Context, d Dictionary, words []string) (found []*Word, notFound []string, err error) {
	seen := map[string]bool{}
	var list []string
	for _, w := range words {
		n, err := NormalizeWord(w)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %q", err, w)
		}
		if !seen[n] {
			seen[n] = true
			list = append(list, n)
		}
	}
	type result struct {
		word *Word
		err  error
	}
	results := make([]result, len(list))
	sem := make(chan struct{}, workerLimit)
	var wg sync.WaitGroup
	for i, w := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			wd, err := s.Lookup(ctx, d, w)
			results[i] = result{wd, err}
		}()
	}
	wg.Wait()
	for i, r := range results {
		switch {
		case r.err == nil:
			found = append(found, r.word)
		case errors.Is(r.err, ErrNotFound):
			notFound = append(notFound, list[i])
		default:
			return nil, nil, r.err
		}
	}
	return found, notFound, nil
}
