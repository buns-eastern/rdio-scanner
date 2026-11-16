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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rdio-scanner/internal/infrastructure/cloudstore"
)

// a day's recordings written the way the server writes them.
func writeDay(t *testing.T, files *AudioFiles, day time.Time, ids ...uint64) []string {
	t.Helper()

	rels := []string{}

	for _, id := range ids {
		rel, err := files.Write(id, day.Add(time.Hour), "call.m4a", "audio/mp4", []byte(strings.Repeat("a", int(id%7)+1)+"-"+day.Format("2006-01-02")))
		if err != nil {
			t.Fatal(err)
		}

		rels = append(rels, rel)
	}

	return rels
}

func TestMoveDaysBeforeMovesOnlyWholeOldDaysAndReadsThemBack(t *testing.T) {
	bucket, client := newFakeBucket(t)
	files := NewAudioFiles(t.TempDir())
	files.UseCloud(client)

	ctx := context.Background()

	old1 := writeDay(t, files, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1, 2, 3, 4, 5)
	old2 := writeDay(t, files, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), 6, 7)
	recent := writeDay(t, files, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), 8, 9)

	// A recording still being written is never moved.
	os.WriteFile(filepath.Join(files.Dir(), "2026", "09", "01", ".writing-123"), []byte("partial"), 0o640)

	result, err := files.MoveDaysBefore(ctx, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("move: %v", err)
	}

	if result.Days != 2 || result.Files != 7 {
		t.Fatalf("moved %d days and %d files, want 2 and 7", result.Days, result.Files)
	}

	if got := bucket.keys(); len(got) != 7 {
		t.Fatalf("the bucket holds %v", got)
	}

	for _, rel := range append(append([]string{}, old1...), old2...) {
		full, _ := files.fullPath(rel)

		if _, err := os.Stat(full); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still on the local disk after its move", rel)
		}

		data, err := files.ReadContext(ctx, rel)
		if err != nil || !strings.HasSuffix(string(data), "2026-09-0"+rel[9:10]) {
			t.Errorf("%s read back from the bucket as %q, %v", rel, data, err)
		}
	}

	for _, rel := range recent {
		full, _ := files.fullPath(rel)

		if _, err := os.Stat(full); err != nil {
			t.Errorf("a recent recording %s left the local disk: %v", rel, err)
		}
	}

	if _, err := os.Stat(filepath.Join(files.Dir(), "2026", "09", "02")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an emptied day folder was left behind")
	}

	// The day with the half-written recording keeps its folder, holding only that file.
	if _, err := os.Stat(filepath.Join(files.Dir(), "2026", "09", "01", ".writing-123")); err != nil {
		t.Errorf("a recording still being written was touched: %v", err)
	}

	before := bucket.gets

	if _, err := files.ReadContext(ctx, recent[0]); err != nil || bucket.gets != before {
		t.Errorf("a recording on the local disk was read from the bucket")
	}
}

func TestMoveDaysBeforeKeepsEveryRecordingWhenTheBucketIsDown(t *testing.T) {
	bucket, client := newFakeBucket(t)
	files := NewAudioFiles(t.TempDir())
	files.UseCloud(client)

	rels := writeDay(t, files, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1, 2, 3)

	bucket.down = true

	_, err := files.MoveDaysBefore(context.Background(), time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))

	var moveErr *MoveError
	if !errors.As(err, &moveErr) || moveErr.Day != "2026/09/01" {
		t.Fatalf("a move to a bucket that is down answered %v", err)
	}

	for _, rel := range rels {
		if _, err := files.ReadContext(context.Background(), rel); err != nil {
			t.Errorf("%s was lost when the bucket was down: %v", rel, err)
		}
	}
}

func TestMoveDaysBeforeKeepsARecordingTheBucketDidNotHoldWhole(t *testing.T) {
	bucket, client := newFakeBucket(t)
	files := NewAudioFiles(t.TempDir())
	files.UseCloud(client)

	rels := writeDay(t, files, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1)

	// A bucket that keeps a short copy: the check after the upload must catch it.
	short := &shortBucket{fakeBucket: bucket}
	server := newServer(t, short)

	shortClient, _ := cloudstore.New(cloudstore.Config{Endpoint: server, Bucket: bucket.name, KeyID: "k", AppKey: "s"})
	files.UseCloud(shortClient)

	if _, err := files.MoveDaysBefore(context.Background(), time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatalf("a short copy in the bucket counted as moved")
	}

	full, _ := files.fullPath(rels[0])

	if _, err := os.Stat(full); err != nil {
		t.Fatalf("the recording left the local disk though the bucket held a short copy: %v", err)
	}
}

func TestRemoveCloudDaysBeforePrunesWholeOldDaysOnly(t *testing.T) {
	bucket, client := newFakeBucket(t)
	files := NewAudioFiles(t.TempDir())
	files.UseCloud(client)

	ctx := context.Background()

	for _, key := range []string{"2026/08/30/1.m4a", "2026/08/31/2.m4a", "2026/08/31/3.m4a", "2026/09/01/4.m4a", "2026/09/02/5.m4a", "rdio-scanner-connection-test/x.txt", "notes/2026.txt"} {
		bucket.objects[key] = []byte("12345")
	}

	pruned := 0
	report := func(files int, bytes int64) { pruned += files }
	files.onCloudPruned.Store(&report)

	removed, bytes, err := files.RemoveCloudDaysBefore(ctx, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	if removed != 3 || bytes != 15 || pruned != 3 {
		t.Fatalf("removed %d recordings, %d bytes, reported %d; want 3, 15, 3", removed, bytes, pruned)
	}

	want := []string{"2026/09/01/4.m4a", "2026/09/02/5.m4a", "notes/2026.txt", "rdio-scanner-connection-test/x.txt"}

	if got := bucket.keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the bucket keeps %v, want %v", got, want)
	}
}

func TestReadContextSaysWhenTheBucketCannotGiveARecordingBack(t *testing.T) {
	bucket, client := newFakeBucket(t)
	files := NewAudioFiles(t.TempDir())
	files.UseCloud(client)

	reported := ""
	report := func(rel string, err error) { reported = rel }
	files.OnCloudReadFailure(report)

	bucket.down = true

	if _, err := files.ReadContext(context.Background(), "2026/09/01/1.m4a"); err == nil {
		t.Fatal("a recording on neither the disk nor the bucket read without an error")
	}

	if reported != "2026/09/01/1.m4a" {
		t.Errorf("the failure reported %q", reported)
	}
}

// The listener's path end to end: a call saved, its day moved to the bucket, the call fetched
// for playback, then pruned from both places.
func TestACallMovedToTheBucketStillPlaysAndIsPrunedFromIt(t *testing.T) {
	fixture, files := newAudioFilesCallsFixture(t)
	bucket, client := newFakeBucket(t)
	files.UseCloud(client)

	ctx := context.Background()

	old := fixture.newCall(10001, 1514000, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	old.Audio = testAudio
	old.AudioFilename = "1514000-call.m4a"
	old.AudioMime = "audio/mp4"

	savedOld, err := fixture.calls.SaveCall(ctx, old)
	if err != nil {
		t.Fatal(err)
	}

	recent := fixture.newCall(10001, 1514000, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	recent.Audio = testAudio

	savedRecent, err := fixture.calls.SaveCall(ctx, recent)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := files.MoveDaysBefore(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("move: %v", err)
	}

	_, rel := storedAudioOf(t, fixture.db, savedOld.ID)

	if got := bucket.keys(); len(got) != 1 || got[0] != rel {
		t.Fatalf("the bucket holds %v, want only %s", got, rel)
	}

	found, err := fixture.calls.GetCall(ctx, savedOld.ID)
	if err != nil {
		t.Fatalf("GetCall of a moved call: %v", err)
	}

	if !bytes.Equal(found.Audio, testAudio) || found.AudioMime != "audio/mp4" {
		t.Errorf("the moved call played back %d bytes as %q", len(found.Audio), found.AudioMime)
	}

	if found, err := fixture.calls.GetCall(ctx, savedRecent.ID); err != nil || !bytes.Equal(found.Audio, testAudio) {
		t.Errorf("the recent call: %v", err)
	}

	if _, err := fixture.calls.DeleteCallsBefore(ctx, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if got := bucket.keys(); len(got) != 0 {
		t.Errorf("the prune left %v in the bucket", got)
	}

	if _, err := fixture.calls.GetCall(ctx, savedOld.ID); err == nil {
		t.Errorf("the pruned call still answers")
	}
}
