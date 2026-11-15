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
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"rdio-scanner/internal/domain"
)

// domainCallFor is a call on the given system and talkgroup identities.
func domainCallFor(systemID, talkgroupID uint64, at time.Time) domain.Call {
	call := domain.NewCall()

	call.Timestamp = at
	call.System = domain.System{ID: systemID}
	call.Talkgroup = domain.Talkgroup{ID: talkgroupID}

	return call
}

func TestMigrateWithAudioFilesLiveOnPostgreSQLWritesEveryLegacyRecordingToAFile(t *testing.T) {
	testMigrateWithAudioFilesLive(t, DialectPostgresql)
}

func TestMigrateWithAudioFilesLiveOnMariaDBWritesEveryLegacyRecordingToAFile(t *testing.T) {
	testMigrateWithAudioFilesLive(t, DialectMariadb)
}

// testMigrateWithAudioFilesLive upgrades the seeded v6.6.3 deployment on a real backend with an
// audio folder, then reads every carried call back from its file, stores a new call, and prunes it.
func testMigrateWithAudioFilesLive(t *testing.T, dialect Dialect) {
	db := liveDatabase(t, dialect)
	s := newStore(db, dialect)
	ctx := context.Background()

	original := legacyCallAudio(t, db)

	if len(original) == 0 {
		t.Fatal("the seeded v6.6.3 deployment holds no calls, so this proves nothing")
	}

	files := NewAudioFiles(filepath.Join(t.TempDir(), "audio"))

	if err := MigrateWithAudioFiles(ctx, db, dialect, fixedClock{migrationInstant}, nil, files); err != nil {
		t.Fatalf("MigrateWithAudioFiles on %s: %v", dialect, err)
	}

	calls := NewCalls(db, dialect)
	calls.UseAudioFiles(files)

	carried := 0

	for id, want := range original {
		var (
			blob []byte
			rel  string
		)

		err := s.queryRow(ctx, `SELECT "audio", "audioPath" FROM "calls" WHERE "callId" = ?`, int64(id)).Scan(&blob, &rel)
		if err != nil {
			// Calls the pass archives instead of carrying are covered by the upstream tests.
			continue
		}

		carried++

		if len(blob) != 0 || rel == "" {
			t.Errorf("call %d: the row holds %d bytes and path %q, want no bytes and a path", id, len(blob), rel)
		}

		found, err := calls.GetCall(ctx, id)
		if err != nil {
			t.Errorf("GetCall(%d): %v", id, err)
			continue
		}

		if !bytes.Equal(found.Audio, want) {
			t.Errorf("call %d: GetCall returned %d bytes, want the %d the v6 row held", id, len(found.Audio), len(want))
		}

		// The day folder comes from the call's real timestamp, not a zero time.
		if found.Timestamp.Year() < 2000 {
			t.Errorf("call %d carried with timestamp %v", id, found.Timestamp)
		}
	}

	if carried == 0 {
		t.Fatal("the pass carried none of the seeded calls, so this proves nothing")
	}

	var stillInRows int

	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM "calls" WHERE octet_length("audio") > 0`).Scan(&stillInRows); err != nil {
		t.Fatal(err)
	}

	if stillInRows != 0 {
		t.Errorf("%d migrated calls still hold their recording in the row", stillInRows)
	}

	// A call the upgraded server stores itself goes to a file too, reads back, and is pruned.
	var systemID, talkgroupID uint64

	if err := s.queryRow(ctx, `SELECT "systemId", "talkgroupId" FROM "talkgroups" ORDER BY "talkgroupId" LIMIT 1`).Scan(&systemID, &talkgroupID); err != nil {
		t.Fatal(err)
	}

	call := domainCallFor(systemID, talkgroupID, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	call.Audio = testAudio
	call.AudioFilename = "new.m4a"
	call.AudioMime = "audio/mp4"

	saved, err := calls.SaveCall(ctx, call)
	if err != nil {
		t.Fatalf("SaveCall on %s: %v", dialect, err)
	}

	found, err := calls.GetCall(ctx, saved.ID)
	if err != nil || !bytes.Equal(found.Audio, testAudio) {
		t.Fatalf("GetCall of the new call on %s: %v", dialect, err)
	}

	if _, err := calls.DeleteCallsBefore(ctx, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("DeleteCallsBefore on %s: %v", dialect, err)
	}

	if _, err := files.Read(audioPathFor(saved.ID, call.Timestamp, call.AudioFilename, call.AudioMime)); err == nil {
		t.Errorf("the pruned call's file is still there")
	}

	t.Logf("%s: %d of %d legacy calls carried into files; a new call stored, read back and pruned", dialect, carried, len(original))
}
