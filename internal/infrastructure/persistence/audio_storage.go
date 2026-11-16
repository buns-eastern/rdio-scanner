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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"rdio-scanner/internal/infrastructure/cloudstore"
)

// AudioStorageSettings is where recordings are kept: the newest days on the local disk,
// and, with cloud storage on, everything older in a bucket.
type AudioStorageSettings struct {
	LocalDays    uint   `json:"localDays"`
	CloudEnabled bool   `json:"cloudEnabled"`
	Bucket       string `json:"bucket"`
	Endpoint     string `json:"endpoint"`
	KeyID        string `json:"keyId"`
	AppKey       string `json:"appKey"`
}

// DefaultLocalDays is how many days of recordings stay on the local disk until an operator
// says otherwise.
const DefaultLocalDays = 30

// complete says the bucket details are all there.
func (settings AudioStorageSettings) complete() bool {
	return strings.TrimSpace(settings.Bucket) != "" && strings.TrimSpace(settings.Endpoint) != "" &&
		strings.TrimSpace(settings.KeyID) != "" && settings.AppKey != ""
}

func (settings AudioStorageSettings) cloudConfig() cloudstore.Config {
	return cloudstore.Config{Endpoint: settings.Endpoint, Bucket: settings.Bucket, KeyID: settings.KeyID, AppKey: settings.AppKey}
}

// AudioStorageStatus is what the audio storage has done and holds.
type AudioStorageStatus struct {
	LocalFiles int64 `json:"localFiles"`
	LocalBytes int64 `json:"localBytes"`
	CloudFiles int64 `json:"cloudFiles"`
	CloudBytes int64 `json:"cloudBytes"`

	// LastMoveAt is when the last move finished, successful or not; zero is never.
	LastMoveAt    int64  `json:"lastMoveAt"`
	LastMoveFiles int    `json:"lastMoveFiles"`
	LastMoveBytes int64  `json:"lastMoveBytes"`
	LastError     string `json:"lastError"`

	Moving bool `json:"moving"`
}

const (
	audioStorageSettingsKey = "audioStorage"
	audioStorageStateKey    = "audioStorageState"
)

// The approved wording of the settings' field errors.
const (
	localDaysRequired   = "Keep on local disk is required"
	localDaysInvalid    = "Keep on local disk is invalid"
	localDaysVsPrune    = "Keep on local disk must be fewer days than Prune Days in Options, so recordings reach cloud storage before they are deleted."
	bucketRequired      = "Bucket name is required"
	endpointRequired    = "Endpoint is required"
	keyIDRequired       = "Key ID is required"
	appKeyRequired      = "Application key is required"
	moveCheckInterval   = 10 * time.Minute
	moveFirstCheckDelay = 2 * time.Minute
	moveNightlyHour     = 3
	moveRetryAfter      = time.Hour
)

// AudioStorage keeps the audio storage settings, gives the audio files the bucket to use,
// and moves older recordings to it once a night.
type AudioStorage struct {
	store store
	audio *AudioFiles

	// pruneDays reads the retention the Prune Days option sets.
	pruneDays func(ctx context.Context) (uint, error)

	// log files what the storage did in the server log.
	log func(warn bool, message string)

	now func() time.Time

	mutex    sync.Mutex
	settings AudioStorageSettings
	status   AudioStorageStatus

	// lastAttempt is when a move last started, which is what spaces the moves a night apart.
	lastAttempt time.Time

	// counting and lastCount space out the walks that count the local disk.
	counting  bool
	lastCount time.Time

	stop chan struct{}
	done chan struct{}
}

// NewAudioStorage builds the audio storage over the audio files it manages.
func NewAudioStorage(db *sql.DB, dialect Dialect, audio *AudioFiles, pruneDays func(ctx context.Context) (uint, error), log func(warn bool, message string)) *AudioStorage {
	storage := &AudioStorage{
		store:     newStore(db, dialect),
		audio:     audio,
		pruneDays: pruneDays,
		log:       log,
		now:       time.Now,
		settings:  AudioStorageSettings{LocalDays: DefaultLocalDays},
	}

	pruned := storage.cloudPruned

	audio.onCloudPruned.Store(&pruned)

	return storage
}

// Load reads the stored settings and state and puts the bucket in use.
func (storage *AudioStorage) Load(ctx context.Context) error {
	settings := AudioStorageSettings{LocalDays: DefaultLocalDays}
	state := AudioStorageStatus{}

	if err := storage.readRow(ctx, audioStorageSettingsKey, &settings); err != nil {
		return err
	}

	if err := storage.readRow(ctx, audioStorageStateKey, &state); err != nil {
		return err
	}

	state.Moving = false

	storage.mutex.Lock()
	storage.settings = settings
	storage.status = state

	if state.LastMoveAt > 0 {
		storage.lastAttempt = time.UnixMilli(state.LastMoveAt)
	}

	storage.mutex.Unlock()

	storage.applyCloud(settings)

	go storage.refreshLocalUsage()

	return nil
}

// applyCloud gives the audio files the bucket the settings name. The bucket stays in use
// for reading while its details are complete, even with the moves turned off: recordings
// already moved there must keep playing.
func (storage *AudioStorage) applyCloud(settings AudioStorageSettings) {
	if !settings.complete() {
		storage.audio.UseCloud(nil)

		return
	}

	client, err := cloudstore.New(settings.cloudConfig())
	if err != nil {
		storage.audio.UseCloud(nil)

		return
	}

	storage.audio.UseCloud(client)
}

// Settings answers the settings in effect.
func (storage *AudioStorage) Settings() AudioStorageSettings {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()

	return storage.settings
}

// PruneDays answers the retention the Prune Days option sets.
func (storage *AudioStorage) PruneDays(ctx context.Context) (uint, error) {
	return storage.pruneDays(ctx)
}

// Status answers what the storage has done and holds.
func (storage *AudioStorage) Status() AudioStorageStatus {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()

	return storage.status
}

// withSavedKey fills an application key left blank with the one already saved: the page
// never sees the saved key, and a blank box means keep it.
func (storage *AudioStorage) withSavedKey(next AudioStorageSettings) AudioStorageSettings {
	if next.AppKey == "" {
		next.AppKey = storage.Settings().AppKey
	}

	next.Bucket = strings.TrimSpace(next.Bucket)
	next.Endpoint = strings.TrimSpace(next.Endpoint)
	next.KeyID = strings.TrimSpace(next.KeyID)

	return next
}

// Validate answers the field errors of a settings submission, keyed by field.
func (storage *AudioStorage) Validate(ctx context.Context, next AudioStorageSettings, localDaysGiven bool) (map[string]string, error) {
	next = storage.withSavedKey(next)
	errors := map[string]string{}

	switch {
	case !localDaysGiven:
		errors["localDays"] = localDaysRequired
	case next.LocalDays < 1:
		errors["localDays"] = localDaysInvalid
	}

	if !next.CloudEnabled {
		return errors, nil
	}

	if _, bad := errors["localDays"]; !bad {
		pruneDays, err := storage.pruneDays(ctx)
		if err != nil {
			return nil, err
		}

		if pruneDays != 0 && next.LocalDays >= pruneDays {
			errors["localDays"] = localDaysVsPrune
		}
	}

	if next.Bucket == "" {
		errors["bucket"] = bucketRequired
	}

	if next.Endpoint == "" {
		errors["endpoint"] = endpointRequired
	}

	if next.KeyID == "" {
		errors["keyId"] = keyIDRequired
	}

	if next.AppKey == "" {
		errors["appKey"] = appKeyRequired
	}

	return errors, nil
}

// Save stores settings that passed Validate and puts them in effect.
func (storage *AudioStorage) Save(ctx context.Context, next AudioStorageSettings) error {
	next = storage.withSavedKey(next)

	if err := storage.writeRow(ctx, audioStorageSettingsKey, next); err != nil {
		return err
	}

	storage.mutex.Lock()
	storage.settings = next
	storage.mutex.Unlock()

	storage.applyCloud(next)

	return nil
}

// Test checks the bucket the given settings name can be written to, read from and deleted
// from. It answers "" when it can, or the reason in the operator's words.
func (storage *AudioStorage) Test(ctx context.Context, next AudioStorageSettings) string {
	next = storage.withSavedKey(next)

	client, err := cloudstore.New(next.cloudConfig())
	if err != nil {
		return "the endpoint couldn't be reached"
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return cloudstore.Reason(client.Probe(ctx))
}

// Start begins checking, every few minutes, whether tonight's move is due.
func (storage *AudioStorage) Start() {
	storage.stop = make(chan struct{})
	storage.done = make(chan struct{})

	go func() {
		defer close(storage.done)

		wait := time.NewTimer(moveFirstCheckDelay)
		defer wait.Stop()

		for {
			select {
			case <-storage.stop:
				return
			case <-wait.C:
			}

			storage.runIfDue()

			wait.Reset(moveCheckInterval)
		}
	}()
}

// Stop ends the checks and waits a little for a move in progress to notice.
func (storage *AudioStorage) Stop() {
	if storage.stop == nil {
		return
	}

	close(storage.stop)

	select {
	case <-storage.done:
	case <-time.After(10 * time.Second):
	}
}

// due says a move should start now: cloud storage is on and usable, it is past the nightly
// hour, and no move has started since then; a move that failed is tried again an hour on.
func (storage *AudioStorage) due(now time.Time) bool {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()

	if !storage.settings.CloudEnabled || storage.audio.Cloud() == nil || storage.status.Moving {
		return false
	}

	local := now.Local()

	tonight := time.Date(local.Year(), local.Month(), local.Day(), moveNightlyHour, 0, 0, 0, local.Location())

	if local.Before(tonight) {
		tonight = tonight.AddDate(0, 0, -1)
	}

	if storage.lastAttempt.Before(tonight) {
		return true
	}

	return storage.status.LastError != "" && now.Sub(storage.lastAttempt) >= moveRetryAfter
}

func (storage *AudioStorage) runIfDue() {
	if !storage.due(storage.now()) {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-storage.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	storage.Move(ctx)
}

// Move moves the recordings older than the days kept on the local disk to the bucket, and
// records what it did.
func (storage *AudioStorage) Move(ctx context.Context) {
	storage.mutex.Lock()

	if storage.status.Moving {
		storage.mutex.Unlock()

		return
	}

	storage.status.Moving = true
	storage.lastAttempt = storage.now()
	localDays := storage.settings.LocalDays

	storage.mutex.Unlock()

	before := storage.now().Add(-time.Duration(localDays) * 24 * time.Hour)

	result, err := storage.audio.MoveDaysBefore(ctx, before)

	storage.mutex.Lock()

	storage.status.Moving = false
	storage.status.LastMoveAt = storage.now().UnixMilli()
	storage.status.LastMoveFiles = result.Files
	storage.status.LastMoveBytes = result.Bytes
	storage.status.CloudFiles += int64(result.Files)
	storage.status.CloudBytes += result.Bytes
	storage.status.LastError = ""

	var moveErr *MoveError

	if err != nil {
		if errors.As(err, &moveErr) {
			storage.status.LastError = cloudstore.Reason(moveErr.Err)
		} else {
			storage.status.LastError = cloudstore.Reason(err)
		}
	}

	state := storage.status

	storage.mutex.Unlock()

	if result.Files > 0 {
		storage.log(false, fmt.Sprintf("audio storage: moved %d recordings (%s) to cloud storage", result.Files, megabytes(result.Bytes)))
	}

	if err != nil {
		if moveErr != nil {
			storage.log(true, fmt.Sprintf("audio storage: could not move %s: %s", moveErr.Day, state.LastError))
		} else {
			storage.log(true, fmt.Sprintf("audio storage: could not move recordings: %s", state.LastError))
		}
	}

	_ = storage.writeRow(context.Background(), audioStorageStateKey, state)

	storage.refreshLocalUsage()
}

// cloudPruned takes off the tally what the retention removed from the bucket.
func (storage *AudioStorage) cloudPruned(files int, bytes int64) {
	if files == 0 {
		return
	}

	storage.mutex.Lock()

	storage.status.CloudFiles = max(0, storage.status.CloudFiles-int64(files))
	storage.status.CloudBytes = max(0, storage.status.CloudBytes-bytes)
	state := storage.status

	storage.mutex.Unlock()

	_ = storage.writeRow(context.Background(), audioStorageStateKey, state)
}

// refreshLocalUsage counts what is on the local disk.
func (storage *AudioStorage) refreshLocalUsage() {
	files, bytes, err := storage.audio.LocalUsage()

	storage.mutex.Lock()
	defer storage.mutex.Unlock()

	storage.counting = false
	storage.lastCount = storage.now()

	if err != nil {
		return
	}

	storage.status.LocalFiles = files
	storage.status.LocalBytes = bytes
}

// localCountInterval is the least time between two counts of the local disk an open admin
// page asks for: a count walks every recording on it.
const localCountInterval = 5 * time.Minute

// RefreshLocalUsage counts what is on the local disk again, in the background, unless a
// count is running or ran a moment ago.
func (storage *AudioStorage) RefreshLocalUsage() {
	storage.mutex.Lock()

	if storage.counting || storage.now().Sub(storage.lastCount) < localCountInterval {
		storage.mutex.Unlock()

		return
	}

	storage.counting = true
	storage.mutex.Unlock()

	go storage.refreshLocalUsage()
}

func megabytes(bytes int64) string {
	if bytes >= 10<<30 {
		return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30))
	}

	return fmt.Sprintf("%d MB", (bytes+(1<<20)/2)>>20)
}

// readRow reads one JSON value stored in the options table; a key never stored leaves the
// destination as it was.
func (storage *AudioStorage) readRow(ctx context.Context, key string, into any) error {
	const query = `SELECT "value" FROM "options" WHERE "key" = ? ORDER BY "optionId" DESC`

	var raw string

	err := storage.store.queryRow(ctx, query, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return statementError("audiostorage", "read", err, query)
	}

	if err := json.Unmarshal([]byte(raw), into); err != nil {
		return operationError("audiostorage", "read", fmt.Errorf("%s: %w", key, err))
	}

	return nil
}

// writeRow stores one JSON value in the options table.
func (storage *AudioStorage) writeRow(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return operationError("audiostorage", "write", err)
	}

	res, err := storage.store.exec(ctx, optionsUpdateQuery, string(b), key)
	if err != nil {
		return statementError("audiostorage", "write", err, optionsUpdateQuery)
	}

	if stored, err := res.RowsAffected(); err == nil && stored > 0 {
		return nil
	}

	if _, err := storage.store.exec(ctx, optionsInsertQuery, key, string(b)); err != nil {
		return statementError("audiostorage", "write", err, optionsInsertQuery)
	}

	return nil
}
