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

//go:build live

package persistence

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"rdio-scanner/internal/infrastructure/cloudstore"
)

// The move, read-back and prune against a real S3-compatible bucket named by the
// environment: CLOUDSTORE_ENDPOINT, CLOUDSTORE_BUCKET, CLOUDSTORE_KEY_ID, CLOUDSTORE_APP_KEY.
func TestLiveTiersAgainstARealBucket(t *testing.T) {
	if os.Getenv("CLOUDSTORE_ENDPOINT") == "" {
		t.Skip("CLOUDSTORE_ENDPOINT is not set")
	}

	client, err := cloudstore.New(cloudstore.Config{
		Endpoint: os.Getenv("CLOUDSTORE_ENDPOINT"),
		Bucket:   os.Getenv("CLOUDSTORE_BUCKET"),
		KeyID:    os.Getenv("CLOUDSTORE_KEY_ID"),
		AppKey:   os.Getenv("CLOUDSTORE_APP_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	files := NewAudioFiles(t.TempDir())
	files.UseCloud(client)

	// Two old days well past the tens of a listing page, and one recent day.
	var oldRels []string

	for day := 1; day <= 2; day++ {
		ids := []uint64{}
		for i := 0; i < 1200; i++ {
			ids = append(ids, uint64(day*10000+i))
		}

		oldRels = append(oldRels, writeDay(t, files, time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC), ids...)...)
	}

	recent := writeDay(t, files, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), 99999)

	result, err := files.MoveDaysBefore(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("move: %v", err)
	}

	if result.Files != len(oldRels) || result.Days != 2 {
		t.Fatalf("moved %+v, want %d files over 2 days", result, len(oldRels))
	}

	usage, _, _ := files.LocalUsage()
	if usage != 1 {
		t.Fatalf("%d recordings left on the local disk, want the recent one", usage)
	}

	for _, rel := range []string{oldRels[0], oldRels[1199], oldRels[2399]} {
		data, err := files.ReadContext(ctx, rel)
		if err != nil || !strings.Contains(string(data), "2026-09-0") {
			t.Fatalf("%s from the bucket: %q, %v", rel, data, err)
		}
	}

	if _, err := files.ReadContext(ctx, recent[0]); err != nil {
		t.Fatal(err)
	}

	removed, _, err := files.RemoveCloudDaysBefore(ctx, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || removed != 1200 {
		t.Fatalf("pruned %d, %v; want the 1200 of September 1st", removed, err)
	}

	if _, err := files.ReadContext(ctx, oldRels[0]); err == nil {
		t.Fatal("a pruned recording still reads")
	}

	if _, err := files.ReadContext(ctx, oldRels[1200]); err != nil {
		t.Fatalf("a recording of the kept day: %v", err)
	}

	removed, _, err = files.RemoveCloudDaysBefore(ctx, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || removed != 1200 {
		t.Fatalf("pruned %d, %v", removed, err)
	}
}
