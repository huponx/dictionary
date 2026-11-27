package lookup

import (
	"errors"
	"fmt"
)

// ErrUnknownDictionary: the id is not in the registry.
var ErrUnknownDictionary = errors.New("lookup: unknown dictionary")

// Entry is one dictionary of the registry. The dictionaries of one Group merge into one card of a saved word.
type Entry struct {
	// ID is what a lookup takes and what the settings are keyed by ("english", "english-vietnamese").
	ID string
	// Group of the saved words the dictionary belongs to ("en"). The key of a saved word is (user, group, word).
	Group string
	// Source is the language of the words, Target the language of the meanings (BCP 47 primary subtag).
	Source, Target string
	// DefaultOn: the dictionary is on until an administrator turns it off. A new dictionary starts off, so a
	// deployment shows it only when its administrator enabled it.
	DefaultOn bool
	Provider  Provider
}

// Group is the dictionaries that merge into one card.
type Group struct {
	ID      string
	Entries []Entry
}

// Registry lists the dictionaries in the order they were registered.
type Registry struct {
	entries []Entry
	byID    map[string]int
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{byID: map[string]int{}} }

// Register adds a dictionary. The id and the group may not be empty and the id is unique: a registry is built
// at start, so a mistake panics.
func (r *Registry) Register(e Entry) *Registry {
	if e.ID == "" || e.Group == "" {
		panic("lookup: a dictionary needs an id and a group")
	}
	if _, dup := r.byID[e.ID]; dup {
		panic(fmt.Sprintf("lookup: dictionary %q registered twice", e.ID))
	}
	r.byID[e.ID] = len(r.entries)
	r.entries = append(r.entries, e)
	return r
}

// Entries returns the dictionaries in registration order.
func (r *Registry) Entries() []Entry { return append([]Entry(nil), r.entries...) }

// IDs returns the ids in registration order.
func (r *Registry) IDs() []string {
	out := make([]string, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.ID
	}
	return out
}

// Entry returns the dictionary with the id.
func (r *Registry) Entry(id string) (Entry, bool) {
	i, ok := r.byID[id]
	if !ok {
		return Entry{}, false
	}
	return r.entries[i], true
}

// Groups returns the groups in the order of their first dictionary, each with its dictionaries in order.
func (r *Registry) Groups() []Group {
	var out []Group
	at := map[string]int{}
	for _, e := range r.entries {
		i, ok := at[e.Group]
		if !ok {
			i = len(out)
			at[e.Group] = i
			out = append(out, Group{ID: e.Group})
		}
		out[i].Entries = append(out[i].Entries, e)
	}
	return out
}

// Group returns the dictionaries of the group, in order; ok is false for an unknown group.
func (r *Registry) Group(id string) (Group, bool) {
	var g Group
	for _, e := range r.entries {
		if e.Group == id {
			g.ID = id
			g.Entries = append(g.Entries, e)
		}
	}
	return g, len(g.Entries) > 0
}
