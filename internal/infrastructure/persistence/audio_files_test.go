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
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rdio-scanner/internal/domain"
)

func TestAudioPathForLaysFilesOutByUTCDayAndCallIdentity(t *testing.T) {
	chicago := time.FixedZone("CST", -6*3600)

	cases := []struct {
		id       uint64
		at       time.Time
		filename string
		mime     string
		want     string
	}{
		{5986008, time.Date(2026, 11, 15, 14, 9, 0, 0, time.UTC), "call.mp3", "audio/mpeg", "2026/11/15/5986008.mp3"},
		// 20:30 in Chicago is already the next day in UTC.
		{7, time.Date(2026, 11, 15, 20, 30, 0, 0, chicago), "", "audio/mp4", "2026/11/16/7.m4a"},
		{8, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "", "", "2026/01/02/8.bin"},
		{9, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), `C:\rec\Engine 12.WAV`, "audio/mpeg", "2026/01/02/9.wav"},
		{10, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "weird.m p3", "audio/mpeg", "2026/01/02/10.mp3"},
	}

	for _, c := range cases {
		if got := audioPathFor(c.id, c.at, c.filename, c.mime); got != c.want {
			t.Errorf("audioPathFor(%d, %v, %q, %q) = %q, want %q", c.id, c.at, c.filename, c.mime, got, c.want)
		}
	}
}

func TestAudioFilesWriteReadAndRemoveARecording(t *testing.T) {
	files := NewAudioFiles(t.TempDir())
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	rel, err := files.Write(42, at, "42.mp3", "audio/mpeg", testAudio)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if rel != "2026/11/15/42.mp3" {
		t.Errorf("Write stored the recording at %q", rel)
	}

	got, err := files.Read(rel)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if !bytes.Equal(got, testAudio) {
		t.Errorf("Read returned %v, want the bytes written", got)
	}

	leftovers, _ := filepath.Glob(filepath.Join(files.Dir(), "2026", "11", "15", ".writing-*"))
	if len(leftovers) != 0 {
		t.Errorf("Write left temporary files behind: %v", leftovers)
	}

	if err := files.Remove(rel); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if err := files.Remove(rel); err != nil {
		t.Errorf("Remove of a file already gone: %v, want nothing", err)
	}

	if _, err := files.Read(rel); err == nil {
		t.Errorf("Read after Remove found the recording")
	}
}

func TestAudioFilesRefusesAPathOutsideTheFolder(t *testing.T) {
	files := NewAudioFiles(t.TempDir())

	for _, rel := range []string{"", "/etc/passwd", "../outside.mp3", "2026/../../outside.mp3", `2026\11\15\1.mp3`} {
		if _, err := files.Read(rel); err == nil {
			t.Errorf("Read(%q) was allowed", rel)
		}
	}
}

func TestNewAudioFilesWithNoFolderKeepsRecordingsInTheDatabase(t *testing.T) {
	if NewAudioFiles("") != nil {
		t.Errorf("an empty folder name gave an audio folder, want the recordings kept in the database")
	}
}

func TestRemoveDaysBeforeRemovesOnlyWholeDaysOlderThanTheCutoff(t *testing.T) {
	files := NewAudioFiles(t.TempDir())

	for i, day := range []time.Time{
		time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
	} {
		if _, err := files.Write(uint64(i+1), day, "", "audio/mpeg", []byte{1}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Something that is not a day folder is left alone.
	if err := os.WriteFile(filepath.Join(files.Dir(), "README"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The cutoff falls inside 2026-11-14, so that day is kept whole.
	removed, err := files.RemoveDaysBefore(time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RemoveDaysBefore: %v", err)
	}

	if removed != 2 {
		t.Errorf("RemoveDaysBefore removed %d day folders, want 2", removed)
	}

	for _, gone := range []string{"2025", "2026/11/13"} {
		if _, err := os.Stat(filepath.Join(files.Dir(), filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s is still there", gone)
		}
	}

	for _, kept := range []string{"2026/11/14/3.mp3", "2026/11/15/4.mp3", "README"} {
		if _, err := os.Stat(filepath.Join(files.Dir(), filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

func TestRemoveDaysBeforeOnAFolderThatDoesNotExistYetDoesNothing(t *testing.T) {
	files := NewAudioFiles(filepath.Join(t.TempDir(), "never-written"))

	if removed, err := files.RemoveDaysBefore(time.Now()); err != nil || removed != 0 {
		t.Errorf("RemoveDaysBefore = %d, %v, want 0 and no error", removed, err)
	}
}

// newAudioFilesCallsFixture is the calls fixture with recordings kept as files: the migration
// pass has added the audio path column and the repository writes into a fresh folder.
func newAudioFilesCallsFixture(t *testing.T) (*callsFixture, *AudioFiles) {
	t.Helper()

	fixture := newCallsFixture(t)
	files := NewAudioFiles(filepath.Join(t.TempDir(), "audio"))

	if err := MigrateWithAudioFiles(context.Background(), fixture.db, DialectSqlite, fixedClock{migrationInstant}, nil, files); err != nil {
		t.Fatalf("MigrateWithAudioFiles: %v", err)
	}

	fixture.calls.UseAudioFiles(files)

	return fixture, files
}

func storedAudioOf(t *testing.T, db *sql.DB, id uint64) ([]byte, string) {
	t.Helper()

	var (
		blob []byte
		rel  string
	)

	if err := db.QueryRow(`SELECT "audio", "audioPath" FROM "calls" WHERE "callId" = ?`, int64(id)).Scan(&blob, &rel); err != nil {
		t.Fatalf("reading call %d: %v", id, err)
	}

	return blob, rel
}

func TestSaveCallWithAudioFilesKeepsTheRecordingOutOfTheRow(t *testing.T) {
	fixture, files := newAudioFilesCallsFixture(t)

	call := fixture.newCall(10001, 1514000, callHistoryTime())
	call.Audio = testAudio
	call.AudioFilename = "1514000-call.mp3"
	call.AudioMime = "audio/mpeg"

	saved, err := fixture.calls.SaveCall(context.Background(), call)
	if err != nil {
		t.Fatalf("SaveCall: %v", err)
	}

	blob, rel := storedAudioOf(t, fixture.db, saved.ID)

	if len(blob) != 0 {
		t.Errorf("the row holds %d bytes of audio, want none", len(blob))
	}

	if want := audioPathFor(saved.ID, call.Timestamp, call.AudioFilename, call.AudioMime); rel != want {
		t.Errorf("the row names %q, want %q", rel, want)
	}

	onDisk, err := os.ReadFile(filepath.Join(files.Dir(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("the recording file: %v", err)
	}

	if !bytes.Equal(onDisk, testAudio) {
		t.Errorf("the file holds %v, want the recording", onDisk)
	}

	found, err := fixture.calls.GetCall(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}

	if !bytes.Equal(found.Audio, testAudio) {
		t.Errorf("GetCall returned %v, want the recording read back from its file", found.Audio)
	}

	if found.AudioFilename != call.AudioFilename || found.AudioMime != call.AudioMime {
		t.Errorf("GetCall returned %q %q, want the name and type the call carried", found.AudioFilename, found.AudioMime)
	}
}

func TestGetCallWithAudioFilesStillPlaysARecordingKeptInTheRow(t *testing.T) {
	fixture, _ := newAudioFilesCallsFixture(t)

	// A call stored before the switch: recording in the row, no path.
	saved, err := NewCalls(fixture.db, DialectSqlite).SaveCall(context.Background(), func() (call domain.Call) {
		call = fixture.newCall(10001, 1514000, callHistoryTime())
		call.Audio = testAudio

		return call
	}())
	if err != nil {
		t.Fatalf("SaveCall without audio files: %v", err)
	}

	found, err := fixture.calls.GetCall(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}

	if !bytes.Equal(found.Audio, testAudio) {
		t.Errorf("GetCall returned %v, want the recording still kept in the row", found.Audio)
	}
}

func TestGetCallWithAudioFilesReportsAMissingFile(t *testing.T) {
	fixture, files := newAudioFilesCallsFixture(t)

	call := fixture.newCall(10001, 1514000, callHistoryTime())
	call.Audio = testAudio

	saved, err := fixture.calls.SaveCall(context.Background(), call)
	if err != nil {
		t.Fatalf("SaveCall: %v", err)
	}

	_, rel := storedAudioOf(t, fixture.db, saved.ID)

	if err := files.Remove(rel); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.calls.GetCall(context.Background(), saved.ID); err == nil {
		t.Errorf("GetCall of a call whose file is gone answered no error")
	}
}

func TestSaveCallWithAudioFilesRemovesTheFileWhenTheSaveRollsBack(t *testing.T) {
	fixture, files := newAudioFilesCallsFixture(t)

	call := fixture.newCall(10001, 1514000, callHistoryTime())
	call.Audio = testAudio

	// A child row that cannot be stored rolls the whole save back.
	if _, err := fixture.db.Exec(`DROP TABLE "callUnits"`); err != nil {
		t.Fatal(err)
	}

	call.Units = []domain.CallUnit{{Offset: 0.5, UnitRef: 1}}

	if _, err := fixture.calls.SaveCall(context.Background(), call); err == nil {
		t.Fatalf("SaveCall succeeded with no callUnits table")
	}

	left := 0

	filepath.WalkDir(files.Dir(), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			left++
		}

		return nil
	})

	if left != 0 {
		t.Errorf("a rolled-back save left %d files behind", left)
	}
}

func TestDeleteCallsBeforeWithAudioFilesRemovesTheOldDays(t *testing.T) {
	fixture, files := newAudioFilesCallsFixture(t)

	old := fixture.newCall(10001, 1514000, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	old.Audio = testAudio
	kept := fixture.newCall(10001, 1514000, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	kept.Audio = testAudio

	savedOld, err := fixture.calls.SaveCall(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}

	savedKept, err := fixture.calls.SaveCall(context.Background(), kept)
	if err != nil {
		t.Fatal(err)
	}

	_, oldRel := storedAudioOf(t, fixture.db, savedOld.ID)
	_, keptRel := storedAudioOf(t, fixture.db, savedKept.ID)

	removed, err := fixture.calls.DeleteCallsBefore(context.Background(), time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeleteCallsBefore: %v", err)
	}

	if removed != 1 {
		t.Errorf("DeleteCallsBefore removed %d rows, want 1", removed)
	}

	if _, err := files.Read(oldRel); err == nil {
		t.Errorf("the pruned call's file %s is still there", oldRel)
	}

	if _, err := files.Read(keptRel); err != nil {
		t.Errorf("the kept call's file: %v", err)
	}

	if found, err := fixture.calls.GetCall(context.Background(), savedKept.ID); err != nil || !bytes.Equal(found.Audio, testAudio) {
		t.Errorf("the kept call after the prune: %v", err)
	}
}

func TestMigrateWithAudioFilesAddsThePathColumnOnceAndMigrateLeavesItOut(t *testing.T) {
	plain := openSchemaDatabase(t)

	if err := Migrate(context.Background(), plain, DialectSqlite, fixedClock{migrationInstant}, nil); err != nil {
		t.Fatal(err)
	}

	columns, err := callTableColumns(context.Background(), newStore(plain, DialectSqlite))
	if err != nil {
		t.Fatal(err)
	}

	if hasColumn(columns, "audioPath") {
		t.Errorf("Migrate without an audio folder added the audio path column")
	}

	files := NewAudioFiles(t.TempDir())

	for pass := 0; pass < 2; pass++ {
		if err := MigrateWithAudioFiles(context.Background(), plain, DialectSqlite, fixedClock{migrationInstant}, nil, files); err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
	}

	columns, err = callTableColumns(context.Background(), newStore(plain, DialectSqlite))
	if err != nil {
		t.Fatal(err)
	}

	if !hasColumn(columns, "audioPath") {
		t.Errorf("MigrateWithAudioFiles did not add the audio path column: %v", columns)
	}
}

// legacyCallAudio reads every v6 call's recording before the pass drops the table.
func legacyCallAudio(t *testing.T, db *sql.DB) map[uint64][]byte {
	t.Helper()

	rows, err := db.Query(`SELECT "id", "audio" FROM "rdioScannerCalls"`)
	if err != nil {
		t.Fatal(err)
	}

	defer rows.Close()

	audio := map[uint64][]byte{}

	for rows.Next() {
		var (
			id   int64
			blob []byte
		)

		if err := rows.Scan(&id, &blob); err != nil {
			t.Fatal(err)
		}

		audio[uint64(id)] = blob
	}

	return audio
}

func TestMigrateWithAudioFilesWritesEveryLegacyRecordingToAFile(t *testing.T) {
	db := openLegacyV6Database(t)
	original := legacyCallAudio(t, db)

	if len(original) == 0 {
		t.Fatal("the v6.6.3 fixture holds no calls, so this proves nothing")
	}

	files := NewAudioFiles(filepath.Join(t.TempDir(), "audio"))

	if err := MigrateWithAudioFiles(context.Background(), db, DialectSqlite, fixedClock{migrationInstant}, nil, files); err != nil {
		t.Fatalf("MigrateWithAudioFiles: %v", err)
	}

	calls := NewCalls(db, DialectSqlite)
	calls.UseAudioFiles(files)

	carried := 0

	for id, want := range original {
		var exists int

		if err := db.QueryRow(`SELECT COUNT(*) FROM "calls" WHERE "callId" = ?`, int64(id)).Scan(&exists); err != nil {
			t.Fatal(err)
		}

		if exists == 0 {
			// Calls the pass archives instead of carrying are covered by the upstream tests.
			continue
		}

		carried++

		blob, rel := storedAudioOf(t, db, id)

		if len(blob) != 0 || rel == "" {
			t.Errorf("call %d: the row holds %d bytes and path %q, want no bytes and a path", id, len(blob), rel)
		}

		found, err := calls.GetCall(context.Background(), id)
		if err != nil {
			t.Errorf("GetCall(%d): %v", id, err)
			continue
		}

		if !bytes.Equal(found.Audio, want) {
			t.Errorf("call %d: GetCall returned %d bytes, want the %d the v6 row held", id, len(found.Audio), len(want))
		}
	}

	if carried == 0 {
		t.Fatal("the pass carried none of the fixture's calls, so this proves nothing")
	}

	var stillInRows int

	if err := db.QueryRow(`SELECT COUNT(*) FROM "calls" WHERE length("audio") > 0`).Scan(&stillInRows); err != nil {
		t.Fatal(err)
	}

	if stillInRows != 0 {
		t.Errorf("%d migrated calls still hold their recording in the row", stillInRows)
	}

	t.Logf("%d of %d legacy calls carried, each with its recording in a file", carried, len(original))
}
