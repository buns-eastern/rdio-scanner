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
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"rdio-scanner/internal/infrastructure/cloudstore"
)

// fakeBucket is an in-memory S3 bucket that answers the requests the cloud store sends:
// put, get, head, a version 2 listing with a prefix and a delimiter, and a multi-delete.
// It does not check signatures; the signing is checked against botocore separately.
type fakeBucket struct {
	mutex   sync.Mutex
	name    string
	objects map[string][]byte
	down    bool // answers every request with a 503
	gets    int
}

func newFakeBucket(t *testing.T) (*fakeBucket, *cloudstore.Client) {
	t.Helper()

	bucket := &fakeBucket{name: "rdio-test", objects: map[string][]byte{}}

	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)

	client, err := cloudstore.New(cloudstore.Config{Endpoint: server.URL, Bucket: bucket.name, KeyID: "k", AppKey: "s"})
	if err != nil {
		t.Fatal(err)
	}

	return bucket, client
}

func (bucket *fakeBucket) keys() []string {
	bucket.mutex.Lock()
	defer bucket.mutex.Unlock()

	keys := []string{}

	for key := range bucket.objects {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func (bucket *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket.mutex.Lock()
	defer bucket.mutex.Unlock()

	if bucket.down {
		w.WriteHeader(http.StatusServiceUnavailable)

		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	name, key, _ := strings.Cut(path, "/")

	if name != bucket.name {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<Error><Code>NoSuchBucket</Code><Message>no such bucket</Message></Error>`)

		return
	}

	switch {
	case r.Method == http.MethodPut && key != "":
		body, _ := io.ReadAll(r.Body)
		bucket.objects[key] = body

	case r.Method == http.MethodGet && key != "":
		bucket.gets++

		body, ok := bucket.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)

			return
		}

		w.Write(body)

	case r.Method == http.MethodHead && key != "":
		body, ok := bucket.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Length", fmt.Sprint(len(body)))

	case r.Method == http.MethodGet && key == "":
		bucket.list(w, r)

	case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
		var request struct {
			Objects []struct{ Key string } `xml:"Object"`
		}

		body, _ := io.ReadAll(r.Body)
		xml.Unmarshal(body, &request)

		for _, object := range request.Objects {
			delete(bucket.objects, object.Key)
		}

		fmt.Fprint(w, `<DeleteResult></DeleteResult>`)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// list answers a version 2 listing, a page of at most three entries so paging is exercised.
func (bucket *fakeBucket) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	prefix, delimiter, token := query.Get("prefix"), query.Get("delimiter"), query.Get("continuation-token")

	keys := []string{}

	for key := range bucket.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	type entry struct {
		name   string
		folder bool
		size   int
	}

	entries := []entry{}
	seen := map[string]bool{}

	for _, key := range keys {
		rest := strings.TrimPrefix(key, prefix)

		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				folder := prefix + rest[:i+len(delimiter)]

				if !seen[folder] {
					seen[folder] = true
					entries = append(entries, entry{name: folder, folder: true})
				}

				continue
			}
		}

		entries = append(entries, entry{name: key, size: len(bucket.objects[key])})
	}

	start := 0

	if token != "" {
		fmt.Sscan(token, &start)
	}

	end := min(start+3, len(entries))

	var out strings.Builder

	out.WriteString("<ListBucketResult>")

	for _, e := range entries[start:end] {
		if e.folder {
			fmt.Fprintf(&out, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", e.name)
		} else {
			fmt.Fprintf(&out, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", e.name, e.size)
		}
	}

	if end < len(entries) {
		fmt.Fprintf(&out, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
	} else {
		out.WriteString("<IsTruncated>false</IsTruncated>")
	}

	out.WriteString("</ListBucketResult>")

	fmt.Fprint(w, out.String())
}

// shortBucket keeps one byte less of every object than it was sent.
type shortBucket struct{ *fakeBucket }

func (bucket *shortBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket.fakeBucket.ServeHTTP(w, r)

	if r.Method == http.MethodPut {
		bucket.mutex.Lock()
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), bucket.name+"/")
		bucket.objects[key] = bucket.objects[key][:len(bucket.objects[key])-1]
		bucket.mutex.Unlock()
	}
}

func newServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server.URL
}
