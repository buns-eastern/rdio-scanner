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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"rdio-scanner/internal/infrastructure/cloudstore"
)

// moveWorkers is how many recordings a move sends to the bucket at once.
const moveWorkers = 16

// MoveResult is what one move did.
type MoveResult struct {
	Days  int
	Files int
	Bytes int64
}

// MoveError is a day a move could not finish, and why.
type MoveError struct {
	Day string // 2026/10/14
	Err error
}

func (e *MoveError) Error() string { return fmt.Sprintf("could not move %s: %v", e.Day, e.Err) }
func (e *MoveError) Unwrap() error { return e.Err }

// localDay is one day folder on the local disk.
type localDay struct {
	rel   string // 2026/10/14
	start time.Time
}

// localDays lists the day folders on the local disk, oldest first. Folders that are not a
// real date are left alone.
func (files *AudioFiles) localDays() ([]localDay, error) {
	found := []localDay{}

	years, err := numberedDirs(files.dir, 4)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return found, nil
		}

		return nil, err
	}

	for _, year := range years {
		months, err := numberedDirs(filepath.Join(files.dir, year.name), 2)
		if err != nil {
			return nil, err
		}

		for _, month := range months {
			days, err := numberedDirs(filepath.Join(files.dir, year.name, month.name), 2)
			if err != nil {
				return nil, err
			}

			for _, day := range days {
				start, ok := dayStart(year.number, month.number, day.number)
				if !ok {
					continue
				}

				found = append(found, localDay{rel: year.name + "/" + month.name + "/" + day.name, start: start})
			}
		}
	}

	return found, nil
}

// dayStart is the UTC midnight a year, month and day name, or false for a date that does
// not exist, such as 2026/02/31.
func dayStart(year, month, day int) (time.Time, bool) {
	start := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)

	if start.Year() != year || int(start.Month()) != month || start.Day() != day {
		return time.Time{}, false
	}

	return start, true
}

// MoveDaysBefore moves every day folder whose whole day ended at or before the given
// instant from the local disk to the bucket. Each recording is uploaded, checked to be in
// the bucket at its full size, and only then removed from the disk, so a recording is in
// one place or the other at every moment. The move stops at the first day it cannot
// finish, leaving what it did not move on the disk for the next run.
func (files *AudioFiles) MoveDaysBefore(ctx context.Context, before time.Time) (MoveResult, error) {
	result := MoveResult{}

	client := files.cloud.Load()
	if client == nil {
		return result, errors.New("no cloud storage is set")
	}

	days, err := files.localDays()
	if err != nil {
		return result, err
	}

	cutoff := before.UTC()

	for _, day := range days {
		if day.start.AddDate(0, 0, 1).After(cutoff) {
			break
		}

		if err := ctx.Err(); err != nil {
			return result, err
		}

		moved, bytes, err := files.moveDay(ctx, client, day.rel)

		result.Files += moved
		result.Bytes += bytes

		if err != nil {
			return result, &MoveError{Day: day.rel, Err: err}
		}

		result.Days++
	}

	return result, nil
}

// moveDay moves one day folder, then removes it and any month or year folder it leaves empty.
func (files *AudioFiles) moveDay(ctx context.Context, client *cloudstore.Client, dayRel string) (int, int64, error) {
	files.days.Lock()
	defer files.days.Unlock()

	dayDir := filepath.Join(files.dir, filepath.FromSlash(dayRel))

	entries, err := os.ReadDir(dayDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}

		return 0, 0, err
	}

	names := make(chan string)

	var (
		mutex    sync.Mutex
		moved    int
		bytes    int64
		firstErr error
		wg       sync.WaitGroup
	)

	work, cancel := context.WithCancel(ctx)
	defer cancel()

	for range moveWorkers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for name := range names {
				size, err := files.moveFile(work, client, dayRel+"/"+name)

				mutex.Lock()

				if err != nil {
					if firstErr == nil {
						firstErr = err

						cancel()
					}
				} else if size >= 0 {
					moved++
					bytes += size
				}

				mutex.Unlock()
			}
		}()
	}

feed:
	for _, entry := range entries {
		name := entry.Name()

		// A recording still being written is a dot-file; it gets its real name first.
		if !entry.Type().IsRegular() || strings.HasPrefix(name, ".") {
			continue
		}

		select {
		case names <- name:
		case <-work.Done():
			break feed
		}
	}

	close(names)
	wg.Wait()

	if firstErr != nil {
		return moved, bytes, firstErr
	}

	if err := ctx.Err(); err != nil {
		return moved, bytes, err
	}

	removeIfEmpty(dayDir)
	removeIfEmpty(filepath.Dir(dayDir))
	removeIfEmpty(filepath.Dir(filepath.Dir(dayDir)))

	return moved, bytes, nil
}

// moveFile uploads one recording, checks the bucket holds all of it, and removes it from
// the disk. It answers the bytes moved, or -1 for a file that went away before it was read.
func (files *AudioFiles) moveFile(ctx context.Context, client *cloudstore.Client, rel string) (int64, error) {
	full, err := files.fullPath(rel)
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return -1, nil
		}

		return 0, err
	}

	err = retry(ctx, func() error {
		if err := client.Put(ctx, rel, data, contentTypeOf(rel)); err != nil {
			return err
		}

		size, err := client.Head(ctx, rel)
		if err != nil {
			return err
		}

		if size != int64(len(data)) {
			return fmt.Errorf("the bucket holds %d bytes of %s, not %d", size, rel, len(data))
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	return int64(len(data)), nil
}

// retry runs a request up to three times, waiting a little longer each time.
func retry(ctx context.Context, request func() error) error {
	var err error

	for attempt, wait := 0, time.Second; attempt < 3; attempt, wait = attempt+1, wait*2 {
		if err = request(); err == nil {
			return nil
		}

		if attempt == 2 {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	return err
}

// contentTypeOf is the content type a recording is stored with.
func contentTypeOf(rel string) string {
	switch strings.ToLower(path.Ext(rel)) {
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	}

	return "application/octet-stream"
}

// RemoveCloudDaysBefore deletes from the bucket every day folder whose whole day ended at
// or before the given instant, the bucket's side of the retention. It answers how many
// recordings and bytes it removed. Anything in the bucket that is not a day folder is left
// alone.
func (files *AudioFiles) RemoveCloudDaysBefore(ctx context.Context, before time.Time) (int, int64, error) {
	client := files.cloud.Load()
	if client == nil {
		return 0, 0, nil
	}

	cutoff := before.UTC()

	removedFiles := 0
	removedBytes := int64(0)

	years, err := listFolders(ctx, client, "", 4)
	if err != nil {
		return 0, 0, err
	}

	for _, year := range years {
		months, err := listFolders(ctx, client, year.name+"/", 2)
		if err != nil {
			return removedFiles, removedBytes, err
		}

		for _, month := range months {
			days, err := listFolders(ctx, client, year.name+"/"+month.name+"/", 2)
			if err != nil {
				return removedFiles, removedBytes, err
			}

			for _, day := range days {
				start, ok := dayStart(year.number, month.number, day.number)
				if !ok || start.AddDate(0, 0, 1).After(cutoff) {
					continue
				}

				count, bytes, err := files.removeCloudDay(ctx, client, year.name+"/"+month.name+"/"+day.name+"/")

				removedFiles += count
				removedBytes += bytes

				if report := files.onCloudPruned.Load(); report != nil {
					(*report)(count, bytes)
				}

				if err != nil {
					return removedFiles, removedBytes, err
				}
			}
		}
	}

	return removedFiles, removedBytes, nil
}

// removeCloudDay deletes every recording under one day prefix in the bucket.
func (files *AudioFiles) removeCloudDay(ctx context.Context, client *cloudstore.Client, prefix string) (int, int64, error) {
	files.days.Lock()
	defer files.days.Unlock()

	removed := 0
	bytes := int64(0)

	for {
		// Each page is deleted before the next is read, so the listing always starts
		// over from what is left: a token from before a delete is not relied upon.
		page, err := client.List(ctx, prefix, "", "")
		if err != nil {
			return removed, bytes, err
		}

		if len(page.Objects) == 0 {
			return removed, bytes, nil
		}

		keys := make([]string, 0, len(page.Objects))
		size := int64(0)

		for _, object := range page.Objects {
			keys = append(keys, object.Key)
			size += object.Size
		}

		if err := retry(ctx, func() error { return client.Delete(ctx, keys) }); err != nil {
			return removed, bytes, err
		}

		removed += len(keys)
		bytes += size
	}
}

// listFolders lists the numbered folders right under a prefix in the bucket, oldest first.
func listFolders(ctx context.Context, client *cloudstore.Client, prefix string, width int) ([]numberedDir, error) {
	found := []numberedDir{}
	token := ""

	for {
		page, err := client.List(ctx, prefix, "/", token)
		if err != nil {
			return nil, err
		}

		for _, folder := range page.Prefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(folder, prefix), "/")

			if len(name) != width {
				continue
			}

			number, err := strconv.Atoi(name)
			if err != nil || number < 0 {
				continue
			}

			found = append(found, numberedDir{name: name, number: number})
		}

		if page.Next == "" {
			break
		}

		token = page.Next
	}

	sortNumbered(found)

	return found, nil
}

func sortNumbered(dirs []numberedDir) {
	for i := 1; i < len(dirs); i++ {
		for j := i; j > 0 && dirs[j].number < dirs[j-1].number; j-- {
			dirs[j], dirs[j-1] = dirs[j-1], dirs[j]
		}
	}
}

// LocalUsage answers how many recordings, and how many bytes of them, are on the local disk.
func (files *AudioFiles) LocalUsage() (int64, int64, error) {
	count := int64(0)
	bytes := int64(0)

	err := filepath.WalkDir(files.dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return err
		}

		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return nil
		}

		count++
		bytes += info.Size()

		return nil
	})

	return count, bytes, err
}
