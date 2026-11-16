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

	// onRollback, when set, hears that the open call was rolled back, so the stage can forget
	// what it remembered about that call.
	onRollback func()
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

	if guard.onRollback != nil {
		guard.onRollback()
	}

	return guard.arch.skip(guard.id, guard.named, "the database refused part of it", guard.original)
}

// unitKey names a unit the way a call names it: the system's identity and the radio's number.
type unitKey struct {
	systemID int64
	unitRef  int64
}

// loadCarriedCallIDs reads the identities of the calls already in the version 8 table.
func loadCarriedCallIDs(ctx context.Context, tx transaction) (map[int64]struct{}, error) {
	const query = `SELECT "callId" FROM "calls"`

	rows, err := tx.query(ctx, query)
	if err != nil {
		return nil, statementError("migration", "migrateCalls", err, query)
	}

	defer rows.Close()

	carried := map[int64]struct{}{}

	for rows.Next() {
		var id int64

		if err := rows.Scan(&id); err != nil {
			return nil, statementError("migration", "migrateCalls", err, query)
		}

		carried[id] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, statementError("migration", "migrateCalls", err, query)
	}

	return carried, nil
}

// loadKnownUnits reads the units already in the version 8 table, by system and radio number.
func loadKnownUnits(ctx context.Context, tx transaction) (map[unitKey]struct{}, error) {
	const query = `SELECT "systemId", "unitRef" FROM "units"`

	rows, err := tx.query(ctx, query)
	if err != nil {
		return nil, statementError("migration", "migrateCalls", err, query)
	}

	defer rows.Close()

	known := map[unitKey]struct{}{}

	for rows.Next() {
		var key unitKey

		if err := rows.Scan(&key.systemID, &key.unitRef); err != nil {
			return nil, statementError("migration", "migrateCalls", err, query)
		}

		known[key] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, statementError("migration", "migrateCalls", err, query)
	}

	return known, nil
}
