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

package app

import (
	"context"

	"rdio-scanner/internal/infrastructure/persistence"
	adminbackend "rdio-scanner/internal/presentation/admin/backend"
)

// adminAudioStorage is the audio storage as the administrator's endpoints use it.
type adminAudioStorage struct {
	storage *persistence.AudioStorage
}

var _ adminbackend.AudioStorage = adminAudioStorage{}

func toPersistence(s adminbackend.AudioStorageSettings) persistence.AudioStorageSettings {
	return persistence.AudioStorageSettings{
		LocalDays:    s.LocalDays,
		CloudEnabled: s.CloudEnabled,
		Bucket:       s.Bucket,
		Endpoint:     s.Endpoint,
		KeyID:        s.KeyID,
		AppKey:       s.AppKey,
	}
}

func (a adminAudioStorage) Settings() adminbackend.AudioStorageSettings {
	s := a.storage.Settings()

	return adminbackend.AudioStorageSettings{
		LocalDays:    s.LocalDays,
		CloudEnabled: s.CloudEnabled,
		Bucket:       s.Bucket,
		Endpoint:     s.Endpoint,
		KeyID:        s.KeyID,
		AppKey:       s.AppKey,
	}
}

func (a adminAudioStorage) Status() adminbackend.AudioStorageStatus {
	s := a.storage.Status()

	return adminbackend.AudioStorageStatus{
		LocalFiles:    s.LocalFiles,
		LocalBytes:    s.LocalBytes,
		CloudFiles:    s.CloudFiles,
		CloudBytes:    s.CloudBytes,
		LastMoveAt:    s.LastMoveAt,
		LastMoveFiles: s.LastMoveFiles,
		LastMoveBytes: s.LastMoveBytes,
		LastError:     s.LastError,
		Moving:        s.Moving,
	}
}

func (a adminAudioStorage) Validate(ctx context.Context, next adminbackend.AudioStorageSettings, localDaysGiven bool) (map[string]string, error) {
	return a.storage.Validate(ctx, toPersistence(next), localDaysGiven)
}

func (a adminAudioStorage) Save(ctx context.Context, next adminbackend.AudioStorageSettings) error {
	return a.storage.Save(ctx, toPersistence(next))
}

func (a adminAudioStorage) Test(ctx context.Context, next adminbackend.AudioStorageSettings) string {
	return a.storage.Test(ctx, toPersistence(next))
}

func (a adminAudioStorage) RefreshLocalUsage() {
	a.storage.RefreshLocalUsage()
}

func (a adminAudioStorage) PruneDays(ctx context.Context) (uint, error) {
	return a.storage.PruneDays(ctx)
}
