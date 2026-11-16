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
	"net/http"
	"strings"
)

// AudioStorageSettings is where recordings are kept, as the administrator's page edits it.
type AudioStorageSettings struct {
	LocalDays    uint
	CloudEnabled bool
	Bucket       string
	Endpoint     string
	KeyID        string
	AppKey       string
}

// AudioStorageStatus is what the audio storage has done and holds.
type AudioStorageStatus struct {
	LocalFiles    int64  `json:"localFiles"`
	LocalBytes    int64  `json:"localBytes"`
	CloudFiles    int64  `json:"cloudFiles"`
	CloudBytes    int64  `json:"cloudBytes"`
	LastMoveAt    int64  `json:"lastMoveAt"`
	LastMoveFiles int    `json:"lastMoveFiles"`
	LastMoveBytes int64  `json:"lastMoveBytes"`
	LastError     string `json:"lastError"`
	Moving        bool   `json:"moving"`
}

// AudioStorage is the audio storage as the administrator's page sees it.
type AudioStorage interface {
	Settings() AudioStorageSettings
	Status() AudioStorageStatus
	Validate(ctx context.Context, next AudioStorageSettings, localDaysGiven bool) (map[string]string, error)
	Save(ctx context.Context, next AudioStorageSettings) error
	Test(ctx context.Context, next AudioStorageSettings) string
	RefreshLocalUsage()

	// PruneDays is the retention the Prune Days option sets, which the page names beside
	// the settings: recordings are deleted with their calls wherever they are.
	PruneDays(ctx context.Context) (uint, error)
}

// audioStorageSettingsReply is the settings as the page gets them: the saved application
// key never leaves the server, only whether there is one.
type audioStorageSettingsReply struct {
	LocalDays    uint   `json:"localDays"`
	CloudEnabled bool   `json:"cloudEnabled"`
	Bucket       string `json:"bucket"`
	Endpoint     string `json:"endpoint"`
	KeyID        string `json:"keyId"`
	AppKeySet    bool   `json:"appKeySet"`
}

type audioStorageReply struct {
	OK        bool                      `json:"ok"`
	Errors    map[string]string         `json:"errors,omitempty"`
	Settings  audioStorageSettingsReply `json:"settings"`
	Status    AudioStorageStatus        `json:"status"`
	PruneDays uint                      `json:"pruneDays"`
}

func (admin *Admin) audioStorageReply(ctx context.Context, ok bool, errors map[string]string) audioStorageReply {
	settings := admin.audioStorage.Settings()

	pruneDays, _ := admin.audioStorage.PruneDays(ctx)

	return audioStorageReply{
		PruneDays: pruneDays,
		OK:        ok,
		Errors:    errors,
		Settings: audioStorageSettingsReply{
			LocalDays:    settings.LocalDays,
			CloudEnabled: settings.CloudEnabled,
			Bucket:       settings.Bucket,
			Endpoint:     settings.Endpoint,
			KeyID:        settings.KeyID,
			AppKeySet:    settings.AppKey != "",
		},
		Status: admin.audioStorage.Status(),
	}
}

// submittedAudioStorage reads a settings submission, and whether it named a number of days.
func submittedAudioStorage(submitted map[string]any) (AudioStorageSettings, bool) {
	settings := AudioStorageSettings{}

	text := func(key string) string {
		value, _ := submitted[key].(string)

		return value
	}

	localDaysGiven := false

	if value, ok := submitted["localDays"].(float64); ok {
		localDaysGiven = true

		if value >= 0 && value == float64(uint(value)) {
			settings.LocalDays = uint(value)
		}
	}

	settings.CloudEnabled, _ = submitted["cloudEnabled"].(bool)
	settings.Bucket = text("bucket")
	settings.Endpoint = text("endpoint")
	settings.KeyID = text("keyId")
	settings.AppKey = text("appKey")

	return settings, localDaysGiven
}

// AudioStorageSettingsEndpoint reads and saves where recordings are kept. A submission
// with field errors is answered 200 with the errors, so the page can show them beside
// the fields; nothing is saved then.
func (admin *Admin) AudioStorageSettingsEndpoint(w http.ResponseWriter, r *http.Request) {
	if !admin.sessionIsOpen(r) {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	if admin.audioStorage == nil {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	switch r.Method {
	case http.MethodGet:
		admin.audioStorage.RefreshLocalUsage()

		writeJSON(w, admin.audioStorageReply(r.Context(), true, nil))

	case http.MethodPut:
		submitted, err := decodeObject(r)
		if err != nil {
			admin.reportUnreadableRequest(r, err)

			w.WriteHeader(http.StatusBadRequest)

			return
		}

		next, localDaysGiven := submittedAudioStorage(submitted)

		errors, err := admin.audioStorage.Validate(r.Context(), next, localDaysGiven)
		if err != nil {
			w.WriteHeader(http.StatusExpectationFailed)

			return
		}

		if len(errors) > 0 {
			writeJSON(w, admin.audioStorageReply(r.Context(), false, errors))

			return
		}

		if err := admin.audioStorage.Save(r.Context(), next); err != nil {
			w.WriteHeader(http.StatusExpectationFailed)

			return
		}

		writeJSON(w, admin.audioStorageReply(r.Context(), true, nil))

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// AudioStorageTestEndpoint checks the bucket the page's current fields name, saved or not.
func (admin *Admin) AudioStorageTestEndpoint(w http.ResponseWriter, r *http.Request) {
	if !admin.sessionIsOpen(r) {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	if admin.audioStorage == nil {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	submitted, err := decodeObject(r)
	if err != nil {
		admin.reportUnreadableRequest(r, err)

		w.WriteHeader(http.StatusBadRequest)

		return
	}

	next, _ := submittedAudioStorage(submitted)

	// The bucket details are checked as if the moves were on: the test is of the bucket.
	next.CloudEnabled = true

	errors, err := admin.audioStorage.Validate(r.Context(), next, true)
	if err != nil {
		w.WriteHeader(http.StatusExpectationFailed)

		return
	}

	delete(errors, "localDays")

	if len(errors) > 0 {
		writeJSON(w, map[string]any{"ok": false, "errors": errors})

		return
	}

	reason := admin.audioStorage.Test(r.Context(), next)

	writeJSON(w, map[string]any{"ok": reason == "", "reason": strings.TrimSpace(reason)})
}
