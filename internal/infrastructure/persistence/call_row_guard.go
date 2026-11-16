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

import "context"

// callRowGuard gives each legacy call the calls stage carries its own savepoint on PostgreSQL.
//
// The stage runs in one transaction and treats a statement the database refuses as a problem
// with that one call: it reports it and moves on. MariaDB, MySQL and SQLite allow that.
// PostgreSQL does not: after one refused statement it refuses every later statement of the
// transaction, so a single bad call ended the whole pass. With a savepoint per call, a call
// whose statements were refused is rolled back on its own and kept in the migration archive,
// and the pass carries on with the next call.
//
// On every other backend the guard does nothing.
type callRowGuard struct {
	tx   transaction
	arch *migrationArchiver
	on   bool

	open     bool
	id       int64
	named    string
	original map[string]any
}

const callRowSavepoint = "v8_call_row"

// begin closes the previous call's savepoint and opens one for the call about to be written.
func (guard *callRowGuard) begin(id int64, named string, original map[string]any) error {
	if !guard.on {
		return nil
	}

	if err := guard.close(); err != nil {
		return err
	}

	if _, err := guard.tx.exec(context.Background(), `SAVEPOINT `+callRowSavepoint); err != nil {
		return statementError("migration", "migrateCalls", err, `SAVEPOINT `+callRowSavepoint)
	}

	guard.open = true
	guard.id = id
	guard.named = named
	guard.original = original

	return nil
}

// close ends the open savepoint. If the database refused any statement of the call, releasing
// the savepoint is refused too; the call is then rolled back to where it started and archived.
func (guard *callRowGuard) close() error {
	if !guard.on || !guard.open {
		return nil
	}

	ctx := context.Background()

	guard.open = false

	if _, err := guard.tx.exec(ctx, `RELEASE SAVEPOINT `+callRowSavepoint); err == nil {
		return nil
	}

	if _, err := guard.tx.exec(ctx, `ROLLBACK TO SAVEPOINT `+callRowSavepoint); err != nil {
		return statementError("migration", "migrateCalls", err, `ROLLBACK TO SAVEPOINT `+callRowSavepoint)
	}

	if _, err := guard.tx.exec(ctx, `RELEASE SAVEPOINT `+callRowSavepoint); err != nil {
		return statementError("migration", "migrateCalls", err, `RELEASE SAVEPOINT `+callRowSavepoint)
	}

	return guard.arch.skip(guard.id, guard.named, "the database refused part of it", guard.original)
}
