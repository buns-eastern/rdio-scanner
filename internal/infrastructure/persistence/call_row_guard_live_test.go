//go:build live

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
	"path/filepath"
	"testing"
	"time"
)

// A 6.6.3 deployment whose units start at identity 1, with a call from a unit the deployment
// never listed: the calls stage creates that unit, and on PostgreSQL its identity used to come
// from a counter still at 1, so the stage failed on a duplicate key and took the pass down.
func TestMigrateLiveOnPostgreSQLCreatesAUnitACallNamesWithoutReusingACarriedIdentity(t *testing.T) {
	for _, files := range []*AudioFiles{nil, NewAudioFiles(filepath.Join(t.TempDir(), "audio"))} {
		db := liveDatabase(t, DialectPostgresql)
		s := newStore(db, DialectPostgresql)
		ctx := context.Background()

		var systemID int64

		if err := s.queryRow(ctx, `SELECT "_id" FROM "rdioScannerSystems" WHERE "id" = 10001`).Scan(&systemID); err != nil {
			t.Fatal(err)
		}

		mustExec(t, db, `INSERT INTO "rdioScannerUnits" ("_id", "id", "label", "order", "systemId") VALUES (1, 990001, 'Unit at identity one', 1, 10001)`)

		at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

		if _, err := s.exec(ctx, `INSERT INTO "rdioScannerCalls" ("id", "audio", "audioName", "audioType", "dateTime", "frequencies", "frequency", "patches", "source", "sources", "system", "talkgroup") VALUES (9001, ?, '9001.mp3', 'audio/mpeg', ?, '[]', 851012500, '[]', 777777, '[]', 10001, 1514000)`, []byte{1, 2, 3}, at); err != nil {
			t.Fatal(err)
		}

		if err := MigrateWithAudioFiles(ctx, db, DialectPostgresql, fixedClock{migrationInstant}, nil, files); err != nil {
			t.Fatalf("MigrateWithAudioFiles (audio files: %v): %v", files != nil, err)
		}

		var carried, units int

		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM "calls" WHERE "callId" = 9001`).Scan(&carried); err != nil {
			t.Fatal(err)
		}

		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM "units" WHERE "unitRef" = 777777`).Scan(&units); err != nil {
			t.Fatal(err)
		}

		if carried != 1 || units != 1 {
			t.Errorf("audio files %v: call 9001 carried %d times and unit 777777 created %d times, want 1 and 1", files != nil, carried, units)
		}

		var oneStillThere int

		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM "units" WHERE "unitId" = 1 AND "unitRef" = 990001`).Scan(&oneStillThere); err != nil {
			t.Fatal(err)
		}

		if oneStillThere != 1 {
			t.Errorf("the unit carried at identity 1 is not there")
		}
	}
}

// A call the database refuses part of is rolled back alone and archived; the calls around it
// are carried and the pass finishes.
func TestMigrateLiveOnPostgreSQLArchivesACallTheDatabaseRefusesAndCarriesTheRest(t *testing.T) {
	db := liveDatabase(t, DialectPostgresql)
	s := newStore(db, DialectPostgresql)
	ctx := context.Background()

	original := legacyCallAudio(t, db)

	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	if _, err := s.exec(ctx, `INSERT INTO "rdioScannerCalls" ("id", "audio", "audioName", "audioType", "dateTime", "frequencies", "frequency", "patches", "source", "sources", "system", "talkgroup") VALUES (9002, ?, '9002.mp3', 'audio/mpeg', ?, '[{"errorCount":0,"freq":2100000000,"pos":0,"spikeCount":0}]', 851012500, '[]', 0, '[]', 10001, 1514000)`, []byte{4, 5, 6}, at); err != nil {
		t.Fatal(err)
	}

	// The schema is applied first so the refusal can be planted: any carrier frequency above
	// 2 GHz is refused by the server, which only call 9002 has.
	if err := ApplySchema(ctx, db, DialectPostgresql); err != nil {
		t.Fatal(err)
	}

	mustExec(t, db, `ALTER TABLE "callFrequencies" ADD CONSTRAINT "test_refuse_9002" CHECK ("frequency" < 2000000000)`)

	files := NewAudioFiles(filepath.Join(t.TempDir(), "audio"))

	if err := MigrateWithAudioFiles(ctx, db, DialectPostgresql, fixedClock{migrationInstant}, nil, files); err != nil {
		t.Fatalf("MigrateWithAudioFiles: %v", err)
	}

	var refused int

	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM "calls" WHERE "callId" = 9002`).Scan(&refused); err != nil {
		t.Fatal(err)
	}

	if refused != 0 {
		t.Errorf("call 9002 was carried although part of it was refused; want it rolled back")
	}

	var reason string

	if err := s.queryRow(ctx, `SELECT "reason" FROM "migrationArchive" WHERE "tableName" = 'rdioScannerCalls' AND "rowId" = 9002`).Scan(&reason); err != nil {
		t.Fatalf("call 9002 is not in the migration archive: %v", err)
	}

	if reason != "the database refused part of it" {
		t.Errorf("call 9002 archived with %q", reason)
	}

	carried := 0

	for id := range original {
		var n int

		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM "calls" WHERE "callId" = ?`, int64(id)).Scan(&n); err != nil {
			t.Fatal(err)
		}

		carried += n
	}

	if carried == 0 {
		t.Errorf("none of the other calls were carried")
	}

	t.Logf("call 9002 archived (%s); %d of the fixture's %d calls carried", reason, carried, len(original))
}

