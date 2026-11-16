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
	"errors"
	"sync"
	"testing"
	"time"

	"rdio-scanner/internal/application/ports"
)

// a cache on a clock the test moves, whose background reads run when the test says.
type countCacheFixture struct {
	cache *searchCountCache
	clock time.Time
	loads int
	next  int64
	fail  bool
	wg    sync.WaitGroup
}

func newCountCacheFixture() *countCacheFixture {
	f := &countCacheFixture{clock: time.Unix(1_800_000_000, 0), next: 100}

	f.cache = newSearchCountCache(15*time.Second, 10*time.Minute)
	f.cache.now = func() time.Time { return f.clock }

	return f
}

func (f *countCacheFixture) load(context.Context) (int64, error) {
	defer f.wg.Done()

	f.loads++

	if f.fail {
		return 0, errors.New("the database went away")
	}

	return f.next, nil
}

func (f *countCacheFixture) get(t *testing.T, key string, expectLoad bool) int64 {
	t.Helper()

	if expectLoad {
		f.wg.Add(1)
	}

	count, err := f.cache.get(context.Background(), key, f.load)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}

	f.wg.Wait()
	f.settle(t)

	return count
}

// settle waits for a background read to file what it read.
func (f *countCacheFixture) settle(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		f.cache.mu.Lock()

		busy := false

		for _, entry := range f.cache.entries {
			busy = busy || entry.refreshing
		}

		f.cache.mu.Unlock()

		if !busy {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("a background read never filed what it read")
		}

		time.Sleep(time.Millisecond)
	}
}

func TestSearchCountCacheAnswersAFreshCountWithoutCounting(t *testing.T) {
	f := newCountCacheFixture()

	if got := f.get(t, "all", true); got != 100 {
		t.Fatalf("first count %d, want 100", got)
	}

	f.next = 105
	f.clock = f.clock.Add(14 * time.Second)

	if got := f.get(t, "all", false); got != 100 || f.loads != 1 {
		t.Fatalf("a fresh count answered %d after %d reads, want 100 after 1", got, f.loads)
	}
}

func TestSearchCountCacheAnswersAStaleCountAndBringsItUpToDate(t *testing.T) {
	f := newCountCacheFixture()

	f.get(t, "all", true)

	f.next = 105
	f.clock = f.clock.Add(time.Minute)

	if got := f.get(t, "all", true); got != 100 {
		t.Fatalf("a stale count answered %d, want the kept 100 while it is read again", got)
	}

	if got := f.get(t, "all", false); got != 105 || f.loads != 2 {
		t.Fatalf("after the background read the count is %d after %d reads, want 105 after 2", got, f.loads)
	}
}

func TestSearchCountCacheCountsAnExpiredSearchInTheOpen(t *testing.T) {
	f := newCountCacheFixture()

	f.get(t, "all", true)

	f.next = 300
	f.clock = f.clock.Add(11 * time.Minute)

	if got := f.get(t, "all", true); got != 300 {
		t.Fatalf("an expired count answered %d, want the 300 counted again", got)
	}
}

func TestSearchCountCacheKeepsTheOldCountWhenTheBackgroundReadFails(t *testing.T) {
	f := newCountCacheFixture()

	f.get(t, "all", true)

	f.fail = true
	f.clock = f.clock.Add(time.Minute)

	f.get(t, "all", true)

	if got := f.get(t, "all", true); got != 100 {
		t.Fatalf("after a failed background read the count is %d, want the kept 100", got)
	}
}

func TestSearchCountCacheKeepsEachSearchApart(t *testing.T) {
	f := newCountCacheFixture()

	f.get(t, searchCountKey("a", []any{uint(1)}), true)

	f.next = 7

	if got := f.get(t, searchCountKey("a", []any{uint(2)}), true); got != 7 {
		t.Fatalf("a search with another value answered %d, want its own 7", got)
	}
}

func TestSearchCountCacheLetsGoOfTheSearchUsedLongestAgo(t *testing.T) {
	f := newCountCacheFixture()

	f.cache.limit = 2

	f.get(t, "a", true)
	f.clock = f.clock.Add(time.Second)
	f.get(t, "b", true)
	f.clock = f.clock.Add(time.Second)
	f.get(t, "a", false)
	f.clock = f.clock.Add(time.Second)
	f.get(t, "c", true)

	if _, kept := f.cache.entries["b"]; kept {
		t.Fatalf("the cache kept the search used longest ago over its limit")
	}

	if len(f.cache.entries) != 2 {
		t.Fatalf("the cache keeps %d searches, want its limit of 2", len(f.cache.entries))
	}
}

// with the counts kept, a page read a moment after a call arrived answers the count
// it kept; without them, every page counts again.
func TestSearchCallsAnswersTheKeptCountWhenTheServerKeepsCounts(t *testing.T) {
	ctx := context.Background()

	for _, keep := range []bool{false, true} {
		fixture := newCallsFixture(t)

		if keep {
			fixture.calls.counts = newSearchCountCache(time.Hour, time.Hour)
		}

		if _, err := fixture.calls.SaveCall(ctx, fixture.newCall(10001, 1514000, callHistoryTime())); err != nil {
			t.Fatalf("SaveCall: %v", err)
		}

		first, err := fixture.calls.SearchCalls(ctx, ports.CallSearch{Limit: 10})
		if err != nil {
			t.Fatalf("SearchCalls: %v", err)
		}

		if _, err := fixture.calls.SaveCall(ctx, fixture.newCall(10001, 1514000, callHistoryTime().Add(time.Second))); err != nil {
			t.Fatalf("SaveCall: %v", err)
		}

		second, err := fixture.calls.SearchCalls(ctx, ports.CallSearch{Limit: 10})
		if err != nil {
			t.Fatalf("SearchCalls: %v", err)
		}

		want := uint(2)
		if keep {
			want = first.Count
		}

		if second.Count != want {
			t.Errorf("keep=%v: the second page counts %d, want %d", keep, second.Count, want)
		}
	}
}
