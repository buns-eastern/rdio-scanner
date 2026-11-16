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

package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type fakeAudioStorage struct {
	settings AudioStorageSettings
	errors   map[string]string
	saved    []AudioStorageSettings
	tested   []AudioStorageSettings
	reason   string
}

func (f *fakeAudioStorage) Settings() AudioStorageSettings { return f.settings }
func (f *fakeAudioStorage) Status() AudioStorageStatus     { return AudioStorageStatus{CloudFiles: 7} }
func (f *fakeAudioStorage) RefreshLocalUsage()             {}

func (f *fakeAudioStorage) PruneDays(context.Context) (uint, error) { return 365, nil }

func (f *fakeAudioStorage) Validate(_ context.Context, next AudioStorageSettings, given bool) (map[string]string, error) {
	errs := map[string]string{}

	for k, v := range f.errors {
		errs[k] = v
	}

	if !given {
		errs["localDays"] = "Keep on local disk is required"
	}

	return errs, nil
}

func (f *fakeAudioStorage) Save(_ context.Context, next AudioStorageSettings) error {
	f.saved = append(f.saved, next)
	f.settings = next

	return nil
}

func (f *fakeAudioStorage) Test(_ context.Context, next AudioStorageSettings) string {
	f.tested = append(f.tested, next)

	return f.reason
}

func audioStorageFixture(storage *fakeAudioStorage) *adminFixture {
	fixture := newAdminFixture()
	fixture.audioStorage = storage
	fixture.routes = fixture.buildRoutes()

	return fixture
}

func TestAudioStorageSettingsNeverSendTheSavedKeyToThePage(t *testing.T) {
	storage := &fakeAudioStorage{settings: AudioStorageSettings{LocalDays: 30, Bucket: "b", AppKey: "the-secret-key"}}
	fixture := audioStorageFixture(storage)

	rec := fixture.serve(adminRequest{method: http.MethodGet, path: "/api/admin/audio-storage", token: captureToken})

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if strings.Contains(rec.Body.String(), "the-secret-key") {
		t.Fatalf("the saved key reached the page: %s", rec.Body.String())
	}

	var reply struct {
		Settings struct {
			LocalDays uint
			AppKeySet bool
		}
		Status struct{ CloudFiles int64 }
	}

	json.Unmarshal(rec.Body.Bytes(), &reply)

	if reply.Settings.LocalDays != 30 || !reply.Settings.AppKeySet || reply.Status.CloudFiles != 7 {
		t.Errorf("the reply read as %+v", reply)
	}
}

func TestAudioStorageSettingsRefuseAnOpenSessionMissing(t *testing.T) {
	fixture := audioStorageFixture(&fakeAudioStorage{})

	for _, path := range []string{"/api/admin/audio-storage", "/api/admin/audio-storage/test"} {
		if rec := fixture.serve(adminRequest{method: http.MethodGet, path: path}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session answered %d", path, rec.Code)
		}
	}
}

func TestAudioStorageSettingsSaveOnlyWithoutFieldErrors(t *testing.T) {
	storage := &fakeAudioStorage{errors: map[string]string{"bucket": "Bucket name is required"}}
	fixture := audioStorageFixture(storage)

	rec := fixture.serve(adminRequest{method: http.MethodPut, path: "/api/admin/audio-storage", token: captureToken,
		body: `{"localDays":30,"cloudEnabled":true,"bucket":"","endpoint":"e","keyId":"k","appKey":""}`})

	if !strings.Contains(rec.Body.String(), `"ok":false`) || !strings.Contains(rec.Body.String(), "Bucket name is required") || len(storage.saved) != 0 {
		t.Fatalf("a submission with a field error: %s, saved %v", rec.Body.String(), storage.saved)
	}

	storage.errors = nil

	rec = fixture.serve(adminRequest{method: http.MethodPut, path: "/api/admin/audio-storage", token: captureToken,
		body: `{"localDays":45,"cloudEnabled":true,"bucket":"b","endpoint":"e","keyId":"k","appKey":""}`})

	if !strings.Contains(rec.Body.String(), `"ok":true`) || len(storage.saved) != 1 || storage.saved[0].LocalDays != 45 || !storage.saved[0].CloudEnabled {
		t.Fatalf("a clean submission: %s, saved %+v", rec.Body.String(), storage.saved)
	}
}

func TestAudioStorageTestAnswersTheReason(t *testing.T) {
	storage := &fakeAudioStorage{reason: "the key was refused"}
	fixture := audioStorageFixture(storage)

	rec := fixture.serve(adminRequest{method: http.MethodPost, path: "/api/admin/audio-storage/test", token: captureToken,
		body: `{"bucket":"b","endpoint":"e","keyId":"k","appKey":"s"}`})

	if !strings.Contains(rec.Body.String(), `"ok":false`) || !strings.Contains(rec.Body.String(), "the key was refused") {
		t.Fatalf("a refused test: %s", rec.Body.String())
	}

	storage.reason = ""

	rec = fixture.serve(adminRequest{method: http.MethodPost, path: "/api/admin/audio-storage/test", token: captureToken,
		body: `{"bucket":"b","endpoint":"e","keyId":"k","appKey":"s"}`})

	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("a good test: %s", rec.Body.String())
	}
}

func TestAudioStorageEndpointsAnswerNotFoundWithRecordingsInTheDatabase(t *testing.T) {
	fixture := newAdminFixture()

	if rec := fixture.serve(adminRequest{method: http.MethodGet, path: "/api/admin/audio-storage", token: captureToken}); rec.Code != http.StatusNotFound {
		t.Errorf("answered %d", rec.Code)
	}
}
