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
	"testing"
	"time"

	"rdio-scanner/internal/application/ports"
)

func TestSearchPageWindowReadsTheSecondHalfFromTheFarEnd(t *testing.T) {
	cases := []struct {
		name                  string
		search                ports.CallSearch
		count                 uint
		wantAsc               bool
		wantLimit, wantOffset uint
		wantFlipped           bool
	}{
		{"first page", ports.CallSearch{Limit: 200}, 1000, false, 200, 0, false},
		{"middle page", ports.CallSearch{Limit: 200, Offset: 400}, 1000, false, 200, 400, false},
		{"late page", ports.CallSearch{Limit: 200, Offset: 600}, 1000, true, 200, 200, true},
		{"short last page", ports.CallSearch{Limit: 200, Offset: 900}, 1000, true, 100, 0, true},
		{"ascending last page", ports.CallSearch{Limit: 200, Offset: 800, Ascending: true}, 1000, false, 200, 0, true},
		{"past the end", ports.CallSearch{Limit: 200, Offset: 1000}, 1000, false, 200, 1000, false},
		{"no rows asked", ports.CallSearch{Offset: 900}, 1000, false, 0, 900, false},
	}

	for _, c := range cases {
		asc, limit, offset, flipped := searchPageWindow(c.search, c.count)

		if asc != c.wantAsc || limit != c.wantLimit || offset != c.wantOffset || flipped != c.wantFlipped {
			t.Errorf("%s: got asc=%v limit=%d offset=%d flipped=%v, want asc=%v limit=%d offset=%d flipped=%v",
				c.name, asc, limit, offset, flipped, c.wantAsc, c.wantLimit, c.wantOffset, c.wantFlipped)
		}
	}
}

// Every page of the history is the same calls in the same order whether it is read from the near
// end or the far one, including calls that share a timestamp and a short last page.
func TestSearchCallsReadsEveryPageTheSameFromEitherEnd(t *testing.T) {
	fixture := newCallsFixture(t)
	ctx := context.Background()

	base := callHistoryTime()

	const total = 23

	for i := 0; i < total; i++ {
		// Pairs of calls share a timestamp, so the order between them rests on the identity.
		call := fixture.newCall(10001, 1514000, base.Add(time.Duration(i/2)*time.Second))

		if _, err := fixture.calls.SaveCall(ctx, call); err != nil {
			t.Fatalf("SaveCall %d: %v", i, err)
		}
	}

	for _, ascending := range []bool{false, true} {
		all, err := fixture.calls.SearchCalls(ctx, ports.CallSearch{Limit: total, Ascending: ascending})
		if err != nil {
			t.Fatalf("the whole history: %v", err)
		}

		if len(all.Results) != total {
			t.Fatalf("the whole history holds %d calls, want %d", len(all.Results), total)
		}

		for offset := uint(0); offset < total; offset += 5 {
			page, err := fixture.calls.SearchCalls(ctx, ports.CallSearch{Limit: 5, Offset: offset, Ascending: ascending})
			if err != nil {
				t.Fatalf("page at %d: %v", offset, err)
			}

			end := offset + 5
			if end > total {
				end = total
			}

			want := all.Results[offset:end]

			if len(page.Results) != len(want) {
				t.Fatalf("ascending=%v page at %d holds %d calls, want %d", ascending, offset, len(page.Results), len(want))
			}

			for i := range want {
				if page.Results[i].ID != want[i].ID {
					t.Errorf("ascending=%v page at %d row %d is call %d, want call %d",
						ascending, offset, i, page.Results[i].ID, want[i].ID)
				}
			}
		}
	}
}
