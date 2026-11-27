package service

import (
	"container/list"
	"sync"
	"time"

	"github.com/huponx/dictionary/internal/store"
)

// categoryCache keeps the category counts of the last users in the memory of the process for a short time. The counts
// (of each group) are bounded but still cost up to countCap row visits per category, and the page of the categories asks for them on
// every visit. It is dropped at once on this instance when the user changes the list or the categories; another
// replica shows the change after at most ttl.
type categoryCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	order *list.List // front: the most recently used
	items map[string]*list.Element
}

// categoryEntry is what the cache keeps of one user: the counts of each group they looked at.
type categoryEntry struct {
	user   string
	groups map[string]groupCounts
}

type groupCounts struct {
	expires time.Time
	value   store.Categories
}

// WithCategoryCache turns the cache of the category counts on: entries live ttl and the cache keeps at most max
// users. Without it every ListCategories reads the database.
func (s *Service) WithCategoryCache(ttl time.Duration, max int) *Service {
	s.cats = &categoryCache{ttl: ttl, max: max, order: list.New(), items: map[string]*list.Element{}}
	return s
}

func (c *categoryCache) get(user, group string, now time.Time) (store.Categories, bool) {
	if c == nil {
		return store.Categories{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[user]
	if !ok {
		return store.Categories{}, false
	}
	e := el.Value.(*categoryEntry)
	g, ok := e.groups[group]
	if !ok || !now.Before(g.expires) {
		delete(e.groups, group)
		return store.Categories{}, false
	}
	c.order.MoveToFront(el)
	return g.value, true
}

func (c *categoryCache) put(user, group string, now time.Time, v store.Categories) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g := groupCounts{expires: now.Add(c.ttl), value: v}
	if el, ok := c.items[user]; ok {
		el.Value.(*categoryEntry).groups[group] = g
		c.order.MoveToFront(el)
		return
	}
	c.items[user] = c.order.PushFront(&categoryEntry{user: user, groups: map[string]groupCounts{group: g}})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*categoryEntry).user)
	}
}

func (c *categoryCache) forget(user string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[user]; ok {
		c.order.Remove(el)
		delete(c.items, user)
	}
}
