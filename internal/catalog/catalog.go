// Package catalog lists the dictionaries the service offers. dictionary and dictionary-worker both build the
// registry from it, so the settings of the system know the same dictionaries in both; only the API has providers.
package catalog

import "github.com/huponx/dictionary/internal/lookup"

// Dictionary ids.
const (
	English           = "english"
	EnglishVietnamese = "english-vietnamese"
)

// Entries are the dictionaries in the order they are shown, without providers. Add a dictionary here (see the
// README, "Add a dictionary"); a dictionary that is not DefaultOn stays off until an administrator enables it.
func Entries() []lookup.Entry {
	return []lookup.Entry{
		{ID: English, Group: "en", Source: "en", Target: "en", DefaultOn: true},
		{ID: EnglishVietnamese, Group: "en", Source: "en", Target: "vi", DefaultOn: true},
	}
}

// Registry builds the registry with the provider of each dictionary id; ids without one are listed all the same
// (a process that does not look words up passes none).
func Registry(providers map[string]lookup.Provider) *lookup.Registry {
	r := lookup.NewRegistry()
	for _, e := range Entries() {
		e.Provider = providers[e.ID]
		r.Register(e)
	}
	return r
}
