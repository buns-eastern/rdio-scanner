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
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AudioFiles keeps each call's recording as a file on disk instead of inside
// the calls row. The row keeps only the file's path relative to the folder, so
// the calls table stays small and the history search never reads past audio.
//
// Files are laid out one folder per day, by the call's own UTC timestamp, and
// named by the call's identity:
//
//	<dir>/2026/11/15/5986008.mp3
//
// A nil *AudioFiles means the recordings stay in the database, as upstream
// stores them.
type AudioFiles struct {
	dir string
}

// NewAudioFiles returns the store for the given folder, or nil for an empty
// folder name, which keeps the recordings in the database.
func NewAudioFiles(dir string) *AudioFiles {
	if dir == "" {
		return nil
	}

	return &AudioFiles{dir: dir}
}

// Dir is the folder the recordings are written under.
func (files *AudioFiles) Dir() string {
	return files.dir
}

// audioPathFor is the relative path, with forward slashes, a call's recording
// is stored at.
func audioPathFor(id uint64, timestamp time.Time, filename, mime string) string {
	day := timestamp.UTC()

	return path.Join(
		fmt.Sprintf("%04d", day.Year()),
		fmt.Sprintf("%02d", int(day.Month())),
		fmt.Sprintf("%02d", day.Day()),
		strconv.FormatUint(id, 10)+audioExtension(filename, mime),
	)
}

// audioExtension picks the file extension from the recording's own name when
// it has a plain one, then from its MIME type, then falls back to .bin.
func audioExtension(filename, mime string) string {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(filename, `\`, "/")))

	if len(ext) >= 2 && len(ext) <= 6 {
		plain := true

		for _, r := range ext[1:] {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				plain = false
				break
			}
		}

		if plain {
			return ext
		}
	}

	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])) {
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/m4a", "audio/x-m4a", "audio/aac":
		return ".m4a"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/ogg", "audio/opus":
		return ".ogg"
	}

	return ".bin"
}

// fullPath turns a stored relative path into a path under the folder, refusing
// anything that would land outside it.
func (files *AudioFiles) fullPath(rel string) (string, error) {
	clean := path.Clean("/" + rel)

	if rel == "" || clean == "/" || strings.Contains(rel, "..") || path.IsAbs(rel) || strings.ContainsRune(rel, '\\') {
		return "", fmt.Errorf("audio path %q is not a path under the audio folder", rel)
	}

	return filepath.Join(files.dir, filepath.FromSlash(clean[1:])), nil
}

// Write stores a recording at its path and returns that path. The bytes go to a
// temporary file in the same folder first and are renamed into place, so a
// reader never sees half a recording.
func (files *AudioFiles) Write(id uint64, timestamp time.Time, filename, mime string, data []byte) (string, error) {
	rel := audioPathFor(id, timestamp, filename, mime)

	full, err := files.fullPath(rel)
	if err != nil {
		return "", err
	}

	folder := filepath.Dir(full)

	if err := os.MkdirAll(folder, 0o750); err != nil {
		return "", fmt.Errorf("cannot create audio folder %s: %w", folder, err)
	}

	tmp, err := os.CreateTemp(folder, ".writing-*")
	if err != nil {
		return "", fmt.Errorf("cannot write audio file in %s: %w", folder, err)
	}

	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)

		return "", fmt.Errorf("cannot write audio file %s: %w", full, err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)

		return "", fmt.Errorf("cannot write audio file %s: %w", full, err)
	}

	if err := os.Chmod(tmpName, 0o640); err != nil {
		os.Remove(tmpName)

		return "", fmt.Errorf("cannot write audio file %s: %w", full, err)
	}

	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)

		return "", fmt.Errorf("cannot write audio file %s: %w", full, err)
	}

	return rel, nil
}

// Read returns the recording stored at a relative path.
func (files *AudioFiles) Read(rel string) ([]byte, error) {
	full, err := files.fullPath(rel)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("cannot read audio file %s: %w", full, err)
	}

	return data, nil
}

// Remove deletes the recording at a relative path. A file that is already gone
// is not an error.
func (files *AudioFiles) Remove(rel string) error {
	full, err := files.fullPath(rel)
	if err != nil {
		return err
	}

	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot remove audio file %s: %w", full, err)
	}

	return nil
}

// RemoveDaysBefore deletes every day folder whose whole day ended at or before
// the given instant, then any month or year folder left empty. Every call
// recorded in such a day is older than the instant, so the retention pass has
// already deleted its row; the folder also takes along any file whose row went
// another way, such as a system deleted from the configuration. It returns how
// many day folders it removed.
func (files *AudioFiles) RemoveDaysBefore(before time.Time) (int, error) {
	cutoff := before.UTC()
	removed := 0

	years, err := numberedDirs(files.dir, 4)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, err
	}

	for _, year := range years {
		yearDir := filepath.Join(files.dir, year.name)

		months, err := numberedDirs(yearDir, 2)
		if err != nil {
			return removed, err
		}

		for _, month := range months {
			monthDir := filepath.Join(yearDir, month.name)

			days, err := numberedDirs(monthDir, 2)
			if err != nil {
				return removed, err
			}

			for _, day := range days {
				start := time.Date(year.number, time.Month(month.number), day.number, 0, 0, 0, 0, time.UTC)

				// Reject names that are not real dates, such as 2026/02/31.
				if start.Year() != year.number || int(start.Month()) != month.number || start.Day() != day.number {
					continue
				}

				if start.AddDate(0, 0, 1).After(cutoff) {
					continue
				}

				if err := os.RemoveAll(filepath.Join(monthDir, day.name)); err != nil {
					return removed, fmt.Errorf("cannot remove audio folder %s: %w", filepath.Join(monthDir, day.name), err)
				}

				removed++
			}

			removeIfEmpty(monthDir)
		}

		removeIfEmpty(yearDir)
	}

	return removed, nil
}

type numberedDir struct {
	name   string
	number int
}

// numberedDirs lists the subfolders of dir whose names are exactly width
// digits, in order. Anything else in the folder is left alone.
func numberedDirs(dir string, width int) ([]numberedDir, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	found := []numberedDir{}

	for _, entry := range entries {
		name := entry.Name()

		if !entry.IsDir() || len(name) != width {
			continue
		}

		number, err := strconv.Atoi(name)
		if err != nil || number < 0 {
			continue
		}

		found = append(found, numberedDir{name: name, number: number})
	}

	sort.Slice(found, func(i, j int) bool { return found[i].number < found[j].number })

	return found, nil
}

func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 0 {
		os.Remove(dir)
	}
}
