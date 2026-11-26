package service

import (
	"container/list"
	"sync"
	"time"

	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/dictionary/internal/srs"
)

// schedule is what the review needs of a user to schedule: the preferences (one call to identity) and the FSRS weights
// (one query).
type schedule struct {
	prefs     settings.User
	scheduler srs.Scheduler
}

// scheduleCache keeps the schedule of the last users in the memory of the process for a short time, so a grade does not
// call identity and the database again. It is dropped at once on this instance when the user changes the settings or
// the parameters; another replica sees the change after at most ttl, which only affects the dates of the next grades.
type scheduleCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	order *list.List // front: the most recently used
	items map[string]*list.Element
}

type cacheEntry struct {
	user    string
	expires time.Time
	value   schedule
}

// WithScheduleCache turns the cache of the schedule of a user on: entries live ttl and the cache keeps at most max
// users. Without it every request reads identity and the database.
func (s *Service) WithScheduleCache(ttl time.Duration, max int) *Service {
	s.sched = &scheduleCache{ttl: ttl, max: max, order: list.New(), items: map[string]*list.Element{}}
	return s
}

func (c *scheduleCache) get(user string, now time.Time) (schedule, bool) {
	if c == nil {
		return schedule{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[user]
	if !ok {
		return schedule{}, false
	}
	e := el.Value.(*cacheEntry)
	if !now.Before(e.expires) {
		c.order.Remove(el)
		delete(c.items, user)
		return schedule{}, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

func (c *scheduleCache) put(user string, now time.Time, v schedule) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[user]; ok {
		e := el.Value.(*cacheEntry)
		e.value, e.expires = v, now.Add(c.ttl)
		c.order.MoveToFront(el)
		return
	}
	c.items[user] = c.order.PushFront(&cacheEntry{user: user, expires: now.Add(c.ttl), value: v})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*cacheEntry).user)
	}
}

func (c *scheduleCache) forget(user string) {
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
