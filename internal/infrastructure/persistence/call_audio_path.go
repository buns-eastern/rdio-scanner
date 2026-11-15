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
)

// The path of a call's recording file, relative to the audio folder, as a column of the
// calls table. It arrives the way the audio length column does, in the migration pass, and
// only when the server keeps recordings as files. An empty path means the recording is
// still inside the row's audio column, which is how every call written before the switch
// keeps playing.
const (
	callAudioPathColumnSqlite   = `ALTER TABLE "calls" ADD COLUMN "audioPath" text NOT NULL DEFAULT ''`
	callAudioPathColumnServer   = `ALTER TABLE "calls" ADD COLUMN "audioPath" varchar(255) NOT NULL DEFAULT ''`
	callAudioPathColumnPostgres = `ALTER TABLE "calls" ADD COLUMN "audioPath" text NOT NULL DEFAULT ''`
)

// ensureCallAudioPathColumn gives the calls table the audio path column. It asks what the
// table has and adds only what is missing, so a start that finds the column says nothing
// and changes nothing.
func ensureCallAudioPathColumn(ctx context.Context, s store) error {
	columns, err := callTableColumns(ctx, s)
	if err != nil {
		return operationError("migration", "ensureCallAudioPath", err)
	}

	if len(columns) == 0 || hasColumn(columns, "audioPath") {
		return nil
	}

	statement := callAudioPathColumnSqlite

	switch s.dialect {
	case DialectMariadb, DialectMysql:
		statement = callAudioPathColumnServer

	case DialectPostgresql:
		statement = callAudioPathColumnPostgres
	}

	if _, err := s.exec(ctx, statement); err != nil {
		return statementError("migration", "ensureCallAudioPath", err, statement)
	}

	return nil
}
