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
	"strings"
	"testing"
	"time"
)

type storageFixture struct {
	storage   *AudioStorage
	files     *AudioFiles
	bucket    *fakeBucket
	pruneDays uint
	logged    []string
	clock     time.Time
}

func newStorageFixture(t *testing.T) *storageFixture {
	t.Helper()

	calls, files := newAudioFilesCallsFixture(t)
	bucket, _ := newFakeBucket(t)

	f := &storageFixture{files: files, bucket: bucket, pruneDays: 365}

	f.storage = NewAudioStorage(calls.db, DialectSqlite, files,
		func(context.Context) (uint, error) { return f.pruneDays, nil },
		func(warn bool, message string) { f.logged = append(f.logged, message) })

	f.clock = time.Date(2026, 11, 16, 3, 30, 0, 0, time.Local)
	f.storage.now = func() time.Time { return f.clock }

	if err := f.storage.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	return f
}

func (f *storageFixture) settings(serverURL string) AudioStorageSettings {
	return AudioStorageSettings{LocalDays: 30, CloudEnabled: true, Bucket: f.bucket.name, Endpoint: serverURL, KeyID: "k", AppKey: "s"}
}

func TestAudioStorageValidateUsesTheApprovedWording(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()

	errs, _ := f.storage.Validate(ctx, AudioStorageSettings{CloudEnabled: true}, false)

	want := map[string]string{
		"localDays": "Keep on local disk is required",
		"bucket":    "Bucket name is required",
		"endpoint":  "Endpoint is required",
		"keyId":     "Key ID is required",
		"appKey":    "Application key is required",
	}

	for field, message := range want {
		if errs[field] != message {
			t.Errorf("%s: %q, want %q", field, errs[field], message)
		}
	}

	errs, _ = f.storage.Validate(ctx, AudioStorageSettings{LocalDays: 0}, true)
	if errs["localDays"] != "Keep on local disk is invalid" {
		t.Errorf("zero days: %q", errs["localDays"])
	}

	f.pruneDays = 30

	errs, _ = f.storage.Validate(ctx, AudioStorageSettings{LocalDays: 30, CloudEnabled: true, Bucket: "b", Endpoint: "e", KeyID: "k", AppKey: "s"}, true)
	if !strings.HasPrefix(errs["localDays"], "Keep on local disk must be fewer days than Prune Days") || len(errs) != 1 {
		t.Errorf("as many days as the retention: %v", errs)
	}

	f.pruneDays = 0

	if errs, _ := f.storage.Validate(ctx, AudioStorageSettings{LocalDays: 30, CloudEnabled: true, Bucket: "b", Endpoint: "e", KeyID: "k", AppKey: "s"}, true); len(errs) != 0 {
		t.Errorf("with pruning off any number of days is fine, got %v", errs)
	}

	if errs, _ := f.storage.Validate(ctx, AudioStorageSettings{LocalDays: 30}, true); len(errs) != 0 {
		t.Errorf("with cloud storage off the bucket details are not needed, got %v", errs)
	}
}

func TestAudioStorageKeepsTheSavedKeyWhenTheBoxIsLeftBlankAndReloadsIt(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()

	url := newServer(t, f.bucket)

	if err := f.storage.Save(ctx, f.settings(url)); err != nil {
		t.Fatal(err)
	}

	blank := f.settings(url)
	blank.AppKey = ""

	if errs, _ := f.storage.Validate(ctx, blank, true); len(errs) != 0 {
		t.Errorf("a blank key box with a key saved: %v", errs)
	}

	blank.LocalDays = 45

	if err := f.storage.Save(ctx, blank); err != nil {
		t.Fatal(err)
	}

	if got := f.storage.Settings(); got.AppKey != "s" || got.LocalDays != 45 {
		t.Errorf("after a save with a blank key box: %+v", got)
	}

	reloaded := NewAudioStorage(f.storage.store.db, DialectSqlite, NewAudioFiles(t.TempDir()), nil, nil)

	if err := reloaded.Load(ctx); err != nil {
		t.Fatal(err)
	}

	if got := reloaded.Settings(); got.AppKey != "s" || got.LocalDays != 45 || got.Bucket != f.bucket.name {
		t.Errorf("the settings read back as %+v", got)
	}

	if reloaded.audio.Cloud() == nil {
		t.Errorf("the reloaded settings did not put the bucket in use")
	}
}

func TestAudioStorageKeepsReadingTheBucketWithTheMovesOff(t *testing.T) {
	f := newStorageFixture(t)

	settings := f.settings(newServer(t, f.bucket))
	settings.CloudEnabled = false

	if err := f.storage.Save(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	if f.files.Cloud() == nil {
		t.Fatal("turning the moves off stopped old recordings from being read from the bucket")
	}

	if f.storage.due(f.clock) {
		t.Error("a move is due with the moves turned off")
	}
}

func TestAudioStorageMovesOnceANightAfterThreeAndRetriesAFailureHourly(t *testing.T) {
	f := newStorageFixture(t)

	if err := f.storage.Save(context.Background(), f.settings(newServer(t, f.bucket))); err != nil {
		t.Fatal(err)
	}

	f.clock = time.Date(2026, 11, 16, 2, 59, 0, 0, time.Local)
	f.storage.lastAttempt = time.Date(2026, 11, 15, 3, 10, 0, 0, time.Local)

	if f.storage.due(f.clock) {
		t.Error("due before three with last night's move done")
	}

	f.clock = time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local)

	if !f.storage.due(f.clock) {
		t.Error("not due at three")
	}

	f.storage.lastAttempt = f.clock

	if f.storage.due(f.clock.Add(5 * time.Hour)) {
		t.Error("due again the same night after a good move")
	}

	f.storage.status.LastError = "the endpoint couldn't be reached"

	if f.storage.due(f.clock.Add(30 * time.Minute)) {
		t.Error("a failed move retried within the hour")
	}

	if !f.storage.due(f.clock.Add(61 * time.Minute)) {
		t.Error("a failed move not retried after an hour")
	}
}

func TestAudioStorageMoveRecordsAndLogsWhatItDid(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()

	if err := f.storage.Save(ctx, f.settings(newServer(t, f.bucket))); err != nil {
		t.Fatal(err)
	}

	f.clock = time.Date(2026, 11, 16, 3, 30, 0, 0, time.UTC)

	writeDay(t, f.files, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1, 2, 3)
	writeDay(t, f.files, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), 4)

	f.storage.Move(ctx)

	status := f.storage.Status()

	if status.LastMoveFiles != 3 || status.CloudFiles != 3 || status.LastError != "" || status.Moving {
		t.Fatalf("after the move: %+v", status)
	}

	if status.LocalFiles != 1 {
		t.Errorf("the local disk counts %d recordings, want 1", status.LocalFiles)
	}

	if len(f.logged) != 1 || !strings.HasPrefix(f.logged[0], "audio storage: moved 3 recordings (") || !strings.HasSuffix(f.logged[0], ") to cloud storage") {
		t.Errorf("logged %q", f.logged)
	}

	f.bucket.down = true

	writeDay(t, f.files, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), 5)

	f.storage.Move(ctx)

	status = f.storage.Status()

	if status.LastError == "" || !strings.HasPrefix(f.logged[len(f.logged)-1], "audio storage: could not move 2026/09/02: ") {
		t.Errorf("a failed move: %+v, logged %q", status, f.logged)
	}

	// The tally survives a restart.
	reloaded := NewAudioStorage(f.storage.store.db, DialectSqlite, NewAudioFiles(t.TempDir()), nil, nil)
	reloaded.Load(ctx)

	if got := reloaded.Status(); got.CloudFiles != 3 || got.LastError == "" {
		t.Errorf("the status read back as %+v", got)
	}
}
