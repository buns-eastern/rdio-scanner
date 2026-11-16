// Copyright (C) 2019-2026 Chrystian Huot <chrystian.huot@saubeo.solutions>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>

package persistence

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// searchCountCache keeps how many calls each history search matches, so the page a
// listener turns to does not count six million rows again before it is read. A count
// younger than fresh is answered as it is; one younger than stale is answered as it is
// while one background read brings it up to date; an older one, or a search not seen
// before, is counted while the listener waits. A count is a few seconds behind the
// calls arriving at most, which is no more than a page read a moment later already is.
type searchCountCache struct {
	fresh, stale time.Duration
	limit        int
	now          func() time.Time
	background   func() (context.Context, context.CancelFunc)

	mu      sync.Mutex
	entries map[string]*searchCount
}

type searchCount struct {
	count      int64
	at         time.Time
	used       time.Time
	refreshing bool
}

func newSearchCountCache(fresh, stale time.Duration) *searchCountCache {
	return &searchCountCache{
		fresh:   fresh,
		stale:   stale,
		limit:   256,
		now:     time.Now,
		entries: map[string]*searchCount{},
		background: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 2*time.Minute)
		},
	}
}

// searchCountKey names a search by the filter it reads and the values bound to it.
func searchCountKey(where string, args []any) string {
	return fmt.Sprintf("%s\x00%#v", where, args)
}

// get answers the count of one search, counting it with load when no count is kept,
// or the one kept is too old to answer with.
func (cache *searchCountCache) get(ctx context.Context, key string, load func(ctx context.Context) (int64, error)) (int64, error) {
	cache.mu.Lock()

	now := cache.now()

	if entry, ok := cache.entries[key]; ok {
		age := now.Sub(entry.at)

		if age < cache.fresh {
			entry.used = now
			count := entry.count
			cache.mu.Unlock()

			return count, nil
		}

		if age < cache.stale {
			entry.used = now
			count := entry.count

			if !entry.refreshing {
				entry.refreshing = true

				go cache.refresh(key, load)
			}

			cache.mu.Unlock()

			return count, nil
		}
	}

	cache.mu.Unlock()

	count, err := load(ctx)
	if err != nil {
		return 0, err
	}

	cache.put(key, count)

	return count, nil
}

// refresh counts one search again in the background, and keeps the old count when the
// read fails: the next search past the stale age counts it again in the open.
func (cache *searchCountCache) refresh(key string, load func(ctx context.Context) (int64, error)) {
	ctx, cancel := cache.background()
	defer cancel()

	count, err := load(ctx)

	cache.mu.Lock()
	defer cache.mu.Unlock()

	entry, ok := cache.entries[key]
	if !ok {
		return
	}

	entry.refreshing = false

	if err == nil {
		entry.count = count
		entry.at = cache.now()
	}
}

// put keeps a count just read, and lets go of the search used longest ago once more
// searches are kept than the limit.
func (cache *searchCountCache) put(key string, count int64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	now := cache.now()

	if entry, ok := cache.entries[key]; ok {
		entry.count, entry.at, entry.used = count, now, now

		return
	}

	cache.entries[key] = &searchCount{count: count, at: now, used: now}

	for len(cache.entries) > cache.limit {
		oldestKey := ""
		var oldest time.Time

		for k, entry := range cache.entries {
			if oldestKey == "" || entry.used.Before(oldest) {
				oldestKey, oldest = k, entry.used
			}
		}

		delete(cache.entries, oldestKey)
	}
}
