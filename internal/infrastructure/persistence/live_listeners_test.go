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
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func readLiveListeners(t *testing.T, live *LiveListeners) (liveListenersRow, int) {
	t.Helper()

	var raw string
	var rows int

	if err := live.store.queryRow(context.Background(), `SELECT COUNT(*) FROM "options" WHERE "key" = ?`, LiveListenersKey).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}

	if err := live.store.queryRow(context.Background(), `SELECT "value" FROM "options" WHERE "key" = ?`, LiveListenersKey).Scan(&raw); err != nil {
		t.Fatalf("read: %v", err)
	}

	var row liveListenersRow

	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}

	return row, rows
}

// The snapshot holds the listeners the source gives, with their count, and a stopping server
// leaves an empty snapshot marked not running, in one row however often it is written.
func TestLiveListenersWritesTheListenersAndClearsThemOnStop(t *testing.T) {
	fixture, _ := newAudioFilesCallsFixture(t)

	since := time.Date(2026, 11, 19, 6, 41, 43, 0, time.UTC)
	seen := since.Add(13 * time.Hour)

	var mutex sync.Mutex

	listeners := []LiveListener{
		{Ident: "Steve", Address: "184.60.97.101", Since: since, LastSeen: seen, Live: true},
		{Address: "198.51.100.4", Since: since, LastSeen: since},
	}

	live := NewLiveListeners(fixture.db, DialectSqlite, func() []LiveListener {
		mutex.Lock()
		defer mutex.Unlock()

		return listeners
	})

	at := time.Date(2026, 11, 19, 19, 45, 0, 0, time.UTC)
	live.now = func() time.Time { return at }

	if err := live.Write(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	if err := live.Write(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	row, rows := readLiveListeners(t, live)

	if rows != 1 {
		t.Errorf("%d rows after two writes, want 1", rows)
	}

	if row.Count != 2 || len(row.Listeners) != 2 || !row.Running || row.At != at.UnixMilli() {
		t.Fatalf("snapshot = %+v", row)
	}

	first := row.Listeners[0]

	if first.Ident != "Steve" || first.Address != "184.60.97.101" || first.Since != since.UnixMilli() || first.LastSeen != seen.UnixMilli() || !first.Live {
		t.Errorf("first listener = %+v", first)
	}

	live.Start()

	mutex.Lock()
	listeners = listeners[:1]
	mutex.Unlock()

	live.Stop(context.Background())

	row, rows = readLiveListeners(t, live)

	if rows != 1 || row.Running || row.Count != 0 || len(row.Listeners) != 0 {
		t.Errorf("after Stop: %+v in %d rows, want an empty snapshot marked not running", row, rows)
	}
}
