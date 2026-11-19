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
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

// LiveListenersKey is the options row the snapshot is kept in. Nothing in the server reads it;
// it is for tools outside the server, such as a status page, that want the listeners as the
// server holds them right now rather than pieced together from the log.
const LiveListenersKey = "liveListeners"

// LiveListenersInterval is how often the snapshot is written.
const LiveListenersInterval = 15 * time.Second

// LiveListener is one listener in the snapshot.
type LiveListener struct {
	Ident    string    // the access code's name, empty for a listener without one
	Address  string    // where the listener connected from
	Since    time.Time // when the connection was taken over
	LastSeen time.Time // the last time the listener answered a ping or sent a message
	Live     bool      // whether the listener has live feed switched on for anything
}

// LiveListeners writes, every LiveListenersInterval, the listeners the server is holding: the
// same list the count sent to every listener is taken from, so a status page reading it shows
// what the web page and the apps show.
type LiveListeners struct {
	store  store
	source func() []LiveListener
	now    func() time.Time

	mutex sync.Mutex
	stop  chan struct{}
	done  chan struct{}
}

func NewLiveListeners(db *sql.DB, dialect Dialect, source func() []LiveListener) *LiveListeners {
	return &LiveListeners{store: newStore(db, dialect), source: source, now: time.Now}
}

type liveListenerRow struct {
	Ident    string `json:"ident"`
	Address  string `json:"address"`
	Since    int64  `json:"since"`
	LastSeen int64  `json:"lastSeen"`
	Live     bool   `json:"live"`
}

type liveListenersRow struct {
	At        int64             `json:"at"`    // when the snapshot was written, milliseconds since 1970
	Count     int               `json:"count"` // the count the web page and the apps are shown
	Running   bool              `json:"running"`
	Listeners []liveListenerRow `json:"listeners"`
}

// Write stores one snapshot now. A server that is stopping writes an empty one, so a status
// page shows nobody connected rather than whoever was connected last.
func (live *LiveListeners) Write(ctx context.Context, running bool) error {
	row := liveListenersRow{At: live.now().UnixMilli(), Running: running, Listeners: []liveListenerRow{}}

	if running {
		for _, l := range live.source() {
			row.Listeners = append(row.Listeners, liveListenerRow{
				Ident: l.Ident, Address: l.Address, Since: l.Since.UnixMilli(), LastSeen: l.LastSeen.UnixMilli(), Live: l.Live,
			})
		}
	}

	row.Count = len(row.Listeners)

	b, err := json.Marshal(row)
	if err != nil {
		return operationError("livelisteners", "write", err)
	}

	res, err := live.store.exec(ctx, optionsUpdateQuery, string(b), LiveListenersKey)
	if err != nil {
		return statementError("livelisteners", "write", err, optionsUpdateQuery)
	}

	if stored, err := res.RowsAffected(); err == nil && stored > 0 {
		return nil
	}

	if _, err := live.store.exec(ctx, optionsInsertQuery, LiveListenersKey, string(b)); err != nil {
		return statementError("livelisteners", "write", err, optionsInsertQuery)
	}

	return nil
}

// Start writes a snapshot at once and then every LiveListenersInterval.
func (live *LiveListeners) Start() {
	live.mutex.Lock()
	defer live.mutex.Unlock()

	if live.stop != nil {
		return
	}

	live.stop = make(chan struct{})
	live.done = make(chan struct{})

	stop, done := live.stop, live.done

	go func() {
		defer close(done)

		ticker := time.NewTicker(LiveListenersInterval)
		defer ticker.Stop()

		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = live.Write(ctx, true)
			cancel()

			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop ends the writes and stores an empty snapshot marked not running.
func (live *LiveListeners) Stop(ctx context.Context) {
	live.mutex.Lock()
	stop, done := live.stop, live.done
	live.stop = nil
	live.mutex.Unlock()

	if stop == nil {
		return
	}

	close(stop)
	<-done

	_ = live.Write(ctx, false)
}
