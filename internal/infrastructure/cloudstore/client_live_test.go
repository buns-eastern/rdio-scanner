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

package cloudstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
)

// The live test talks to a real S3-compatible bucket named by the environment:
// CLOUDSTORE_ENDPOINT, CLOUDSTORE_BUCKET, CLOUDSTORE_KEY_ID, CLOUDSTORE_APP_KEY.
func liveClient(t *testing.T) *Client {
	t.Helper()

	config := Config{
		Endpoint: os.Getenv("CLOUDSTORE_ENDPOINT"),
		Bucket:   os.Getenv("CLOUDSTORE_BUCKET"),
		KeyID:    os.Getenv("CLOUDSTORE_KEY_ID"),
		AppKey:   os.Getenv("CLOUDSTORE_APP_KEY"),
	}

	if config.Endpoint == "" {
		t.Skip("CLOUDSTORE_ENDPOINT is not set")
	}

	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}

	return client
}

func TestLiveRoundTrip(t *testing.T) {
	client := liveClient(t)
	ctx := context.Background()

	if err := client.Probe(ctx); err != nil {
		t.Fatalf("probe: %v (%s)", err, Reason(err))
	}

	prefix := "live-test/2026/09/"

	var keys []string

	for day := 1; day <= 3; day++ {
		for i := 0; i < 700; i++ {
			key := fmt.Sprintf("%s%02d/%d.m4a", prefix, day, i)
			keys = append(keys, key)

			if err := client.Put(ctx, key, []byte(key), "audio/mp4"); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
		}
	}

	got, err := client.Get(ctx, keys[5])
	if err != nil || string(got) != keys[5] {
		t.Fatalf("get: %q, %v", got, err)
	}

	size, err := client.Head(ctx, keys[5])
	if err != nil || size != int64(len(keys[5])) {
		t.Fatalf("head: %d, %v", size, err)
	}

	if _, err := client.Get(ctx, prefix+"missing.m4a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing key answered %v, want ErrNotFound", err)
	}

	if _, err := client.Head(ctx, prefix+"missing.m4a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing key's head answered %v, want ErrNotFound", err)
	}

	days, err := client.List(ctx, prefix, "/", "")
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(days.Prefixes)

	if len(days.Prefixes) != 3 || days.Prefixes[0] != prefix+"01/" {
		t.Fatalf("the day folders listed as %v", days.Prefixes)
	}

	var listed []string
	token := ""

	for {
		page, err := client.List(ctx, prefix, "", token)
		if err != nil {
			t.Fatal(err)
		}

		for _, object := range page.Objects {
			listed = append(listed, object.Key)
		}

		if page.Next == "" {
			break
		}

		token = page.Next
	}

	if len(listed) != len(keys) {
		t.Fatalf("listed %d objects across pages, want %d", len(listed), len(keys))
	}

	for start := 0; start < len(keys); start += 1000 {
		end := min(start+1000, len(keys))

		if err := client.Delete(ctx, keys[start:end]); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}

	if err := client.Delete(ctx, []string{prefix + "never-there.m4a"}); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}

	after, err := client.List(ctx, prefix, "", "")
	if err != nil || len(after.Objects) != 0 {
		t.Fatalf("after the delete the prefix holds %d objects, %v", len(after.Objects), err)
	}
}

func TestLiveReasons(t *testing.T) {
	good := liveClient(t)
	ctx := context.Background()

	wrongBucket, _ := New(Config{Endpoint: os.Getenv("CLOUDSTORE_ENDPOINT"), Bucket: "no-such-bucket-rdio-test", KeyID: good.keyID, AppKey: good.appKey})

	if reason := Reason(wrongBucket.Probe(ctx)); reason != "the bucket wasn't found" {
		t.Errorf("a missing bucket reads %q", reason)
	}

	nowhere, _ := New(Config{Endpoint: "http://127.0.0.1:1", Bucket: "b", KeyID: "k", AppKey: "s"})

	if reason := Reason(nowhere.Probe(ctx)); reason != "the endpoint couldn't be reached" {
		t.Errorf("an unreachable endpoint reads %q", reason)
	}
}
