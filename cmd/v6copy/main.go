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

// Command v6copy copies a version 6.6.3 deployment from MariaDB or MySQL into PostgreSQL,
// into the same tables version 6.6.3 itself creates on PostgreSQL. The version 8 server then
// upgrades that PostgreSQL database the way it upgrades any 6.6.3 database.
//
// The source is only read, in batches by primary key, with no long transaction, so it can be
// a deployment that is still running. A second run carries on where the first stopped: every
// table is read from just past the highest key the destination already holds, and for the
// calls also past the highest call the upgraded version 8 table holds, so the run made at
// switchover time copies only the calls recorded since.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// v6Table is one table of the 6.6.3 schema, with the key the copy walks it by.
type v6Table struct {
	name   string
	key    string
	create []string
}

// v6Tables is the 6.6.3 PostgreSQL schema, in the order the tables are copied: the calls last,
// because they are the bulk of the work and the rest is the configuration.
var v6Tables = []v6Table{
	{"rdioScannerMeta", "name", []string{
		`create table "rdioScannerMeta" ("name" varchar(255) not null primary key unique)`,
	}},
	{"rdioScannerAccesses", "_id", []string{
		`create table "rdioScannerAccesses" ("_id" serial not null primary key, "code" varchar(255) not null unique, "expiration" timestamp with time zone, "ident" varchar(255), "limit" integer, "order" integer, "systems" text not null)`,
	}},
	{"rdioScannerApiKeys", "_id", []string{
		`create table "rdioScannerApiKeys" ("_id" serial not null primary key, "disabled" boolean not null default false, "ident" varchar(255), "key" varchar(255) not null unique, "order" integer, "systems" text not null)`,
	}},
	{"rdioScannerConfigs", "_id", []string{
		`create table "rdioScannerConfigs" ("_id" serial not null primary key, "key" varchar(255) not null unique, "val" text not null)`,
		`create index "rdio_scanner_configs_key" on "rdioScannerConfigs" ("key")`,
	}},
	{"rdioScannerDirWatches", "_id", []string{
		`create table "rdioScannerDirWatches" ("_id" serial not null primary key, "delay" integer default 0, "deleteAfter" boolean not null default false, "directory" varchar(255) not null unique, "disabled" boolean not null default false, "extension" varchar(255), "frequency" integer, "mask" varchar(255), "order" integer, "systemId" integer, "talkgroupId" integer, "type" varchar(255), "usePolling" boolean not null default false)`,
	}},
	{"rdioScannerDownstreams", "_id", []string{
		`create table "rdioScannerDownstreams" ("_id" serial not null primary key, "apiKey" varchar(255) not null, "disabled" boolean not null default false, "order" integer, "systems" text not null, "url" varchar(255) not null)`,
	}},
	{"rdioScannerGroups", "_id", []string{
		`create table "rdioScannerGroups" ("_id" serial not null primary key, "label" varchar(255) not null)`,
	}},
	{"rdioScannerLogs", "_id", []string{
		`create table "rdioScannerLogs" ("_id" serial not null primary key, "dateTime" timestamp with time zone not null, "level" varchar(255) not null, "message" varchar(255) not null)`,
		`create index "rdio_scanner_logs_date_time_level" on "rdioScannerLogs" ("dateTime", "level")`,
	}},
	{"rdioScannerSystems", "_id", []string{
		`create table "rdioScannerSystems" ("_id" serial not null primary key, "autoPopulate" boolean not null default false, "blacklists" text not null, "id" integer not null unique, "label" varchar(255) not null, "led" varchar(255), "order" integer)`,
	}},
	{"rdioScannerTags", "_id", []string{
		`create table "rdioScannerTags" ("_id" serial not null primary key, "label" varchar(255) not null)`,
	}},
	{"rdioScannerTalkgroups", "_id", []string{
		`create table "rdioScannerTalkgroups" ("_id" serial not null primary key, "frequency" integer, "groupId" integer not null, "id" integer not null, "label" varchar(255) not null, "led" varchar(255), "name" varchar(255) not null, "order" integer, "systemId" integer not null, "tagId" integer not null)`,
		`create unique index "rdio_scanner_talkgroups_system_id_id" on "rdioScannerTalkgroups" ("systemId", "id")`,
	}},
	{"rdioScannerUnits", "_id", []string{
		`create table "rdioScannerUnits" ("_id" serial not null primary key, "id" integer not null, "label" varchar(255) not null, "order" integer, "systemId" integer not null)`,
		`create unique index "rdio_scanner_units_system_id_id" on "rdioScannerUnits" ("systemId", "id")`,
	}},
	{"rdioScannerCalls", "id", []string{
		`create table "rdioScannerCalls" ("id" serial not null primary key, "audio" bytea not null, "audioName" varchar(255), "audioType" varchar(255), "dateTime" timestamp with time zone not null, "frequencies" text not null, "frequency" integer, "patches" text not null, "source" integer, "sources" text not null, "system" integer not null, "talkgroup" integer not null)`,
		`create index "rdio_scanner_calls_date_time_system_talkgroup" on "rdioScannerCalls" ("dateTime", "system", "talkgroup")`,
	}},
}

type options struct {
	from      string
	to        string
	callsOnly bool
	batch     int
	dryRun    bool
}

func main() {
	opts := options{}

	flag.StringVar(&opts.from, "from", "", `MariaDB/MySQL source, e.g. "user:pass@tcp(127.0.0.1:3307)/rdio_scanner"`)
	flag.StringVar(&opts.to, "to", "", `PostgreSQL destination, e.g. "postgres://user:pass@127.0.0.1:5432/rdio?sslmode=disable"`)
	flag.BoolVar(&opts.callsOnly, "calls-only", false, "copy only the calls recorded since the last run (the switchover run)")
	flag.IntVar(&opts.batch, "batch", 500, "rows per batch")
	flag.BoolVar(&opts.dryRun, "dry-run", false, "count what would be copied and copy nothing")
	flag.Parse()

	if opts.from == "" || opts.to == "" || opts.batch < 1 {
		flag.Usage()
		os.Exit(2)
	}

	if err := run(context.Background(), opts, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "v6copy: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, opts options, out *os.File) error {
	src, err := sql.Open("mysql", sourceDSN(opts.from))
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	defer src.Close()

	if err := src.PingContext(ctx); err != nil {
		return fmt.Errorf("source: %w", err)
	}

	dst, err := sql.Open("pgx", opts.to)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}

	defer dst.Close()

	if err := dst.PingContext(ctx); err != nil {
		return fmt.Errorf("destination: %w", err)
	}

	started := time.Now()

	for _, table := range v6Tables {
		if opts.callsOnly && table.name != "rdioScannerCalls" {
			continue
		}

		if err := copyTable(ctx, src, dst, table, opts, out); err != nil {
			return fmt.Errorf("%s: %w", table.name, err)
		}
	}

	fmt.Fprintf(out, "done in %s\n", time.Since(started).Round(time.Second))

	return nil
}

// sourceDSN adds what the copy needs to the source connection: datetimes read as UTC instants,
// which is how the version 8 upgrade reads a 6.6.3 datetime that carries no zone.
func sourceDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	return dsn + sep + "parseTime=true&loc=UTC&maxAllowedPacket=0"
}

func copyTable(ctx context.Context, src, dst *sql.DB, table v6Table, opts options, out *os.File) error {
	srcColumns, err := sourceColumns(ctx, src, table.name)
	if err != nil {
		return err
	}

	if len(srcColumns) == 0 {
		fmt.Fprintf(out, "%-24s not in the source, skipped\n", table.name)
		return nil
	}

	if opts.dryRun {
		after := int64(0)

		if table.key != "name" {
			if after, err = startingKey(ctx, dst, table, opts); err != nil {
				return err
			}
		}

		var total int64

		query := "SELECT COUNT(*) FROM `" + table.name + "`"
		if table.key != "name" {
			query += " WHERE `" + table.key + "` > " + strconv.FormatInt(after, 10)
		}

		if err := src.QueryRowContext(ctx, query).Scan(&total); err != nil {
			return fmt.Errorf("counting: %w", err)
		}

		fmt.Fprintf(out, "%-24s %d rows to copy\n", table.name, total)

		return nil
	}

	if err := ensureTable(ctx, dst, table); err != nil {
		return err
	}

	columns, types, err := destinationColumns(ctx, dst, table)
	if err != nil {
		return err
	}

	// Only the columns both sides have are copied; a destination column the source lacks keeps
	// its default.
	use := []string{}
	useTypes := []string{}

	for i, column := range columns {
		if srcColumns[column] {
			use = append(use, column)
			useTypes = append(useTypes, types[i])
		} else {
			fmt.Fprintf(out, "%-24s column %q is not in the source, left to its default\n", table.name, column)
		}
	}

	if table.key == "name" {
		return copySmallTable(ctx, src, dst, table, use, useTypes, opts, out)
	}

	after, err := startingKey(ctx, dst, table, opts)
	if err != nil {
		return err
	}

	var total int64

	if err := src.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table.name+"` WHERE `"+table.key+"` > ?", after).Scan(&total); err != nil {
		return fmt.Errorf("counting: %w", err)
	}

	selectQuery := "SELECT " + sourceList(use) + " FROM `" + table.name + "` WHERE `" + table.key + "` > ? ORDER BY `" + table.key + "` LIMIT " + strconv.Itoa(opts.batch)
	keyIndex := indexOf(use, table.key)

	if keyIndex < 0 {
		return fmt.Errorf("the key %q is not among the copied columns", table.key)
	}

	var (
		copied   int64
		bytes    int64
		lastSaid = time.Now()
		began    = time.Now()
	)

	for {
		batch, size, err := readBatch(ctx, src, selectQuery, after, len(use))
		if err != nil {
			return err
		}

		if len(batch) == 0 {
			break
		}

		if err := writeBatch(ctx, dst, table.name, use, useTypes, batch); err != nil {
			return fmt.Errorf("writing the batch after key %d: %w", after, err)
		}

		next, err := toInt64(batch[len(batch)-1][keyIndex])
		if err != nil {
			return fmt.Errorf("reading the key: %w", err)
		}

		after = next
		copied += int64(len(batch))
		bytes += size

		if time.Since(lastSaid) >= 10*time.Second {
			sayProgress(out, table.name, copied, total, bytes, began, after)
			lastSaid = time.Now()
		}
	}

	sayProgress(out, table.name, copied, total, bytes, began, after)

	return nil
}

func sayProgress(out *os.File, name string, copied, total, bytes int64, began time.Time, after int64) {
	elapsed := time.Since(began)
	rate := float64(bytes) / 1e6 / elapsed.Seconds()

	eta := "-"
	if copied > 0 && total > copied {
		eta = (time.Duration(float64(elapsed) / float64(copied) * float64(total-copied))).Round(time.Second).String()
	}

	fmt.Fprintf(out, "%-24s %d / %d rows, %.1f MB/s, last key %d, %s elapsed, ETA %s\n",
		name, copied, total, rate, after, elapsed.Round(time.Second), eta)
}

// copySmallTable copies a table keyed by text in one pass; only the meta table is.
func copySmallTable(ctx context.Context, src, dst *sql.DB, table v6Table, use, useTypes []string, opts options, out *os.File) error {
	batch, _, err := readBatch(ctx, src, "SELECT "+sourceList(use)+" FROM `"+table.name+"`", nil, len(use))
	if err != nil {
		return err
	}

	if len(batch) > 0 {
		if err := writeBatch(ctx, dst, table.name, use, useTypes, batch); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "%-24s %d rows\n", table.name, len(batch))

	return nil
}

// startingKey is the highest key already copied: the destination table's own highest key, and
// for the calls also the highest call the version 8 upgrade has already carried into "calls".
func startingKey(ctx context.Context, dst *sql.DB, table v6Table, opts options) (int64, error) {
	var after int64

	if exists, err := tableExists(ctx, dst, table.name); err != nil {
		return 0, err
	} else if exists {
		if err := dst.QueryRowContext(ctx, `SELECT COALESCE(MAX("`+table.key+`"), 0) FROM "`+table.name+`"`).Scan(&after); err != nil {
			return 0, err
		}
	}

	if table.name == "rdioScannerCalls" {
		if exists, err := tableExists(ctx, dst, "calls"); err != nil {
			return 0, err
		} else if exists {
			var carried int64

			if err := dst.QueryRowContext(ctx, `SELECT COALESCE(MAX("callId"), 0) FROM "calls"`).Scan(&carried); err != nil {
				return 0, err
			}

			if carried > after {
				after = carried
			}
		}
	}

	return after, nil
}

func readBatch(ctx context.Context, src *sql.DB, query string, after any, width int) ([][]any, int64, error) {
	var (
		rows *sql.Rows
		err  error
	)

	if after == nil {
		rows, err = src.QueryContext(ctx, query)
	} else {
		rows, err = src.QueryContext(ctx, query, after)
	}

	if err != nil {
		return nil, 0, fmt.Errorf("reading: %w", err)
	}

	defer rows.Close()

	batch := [][]any{}

	var size int64

	for rows.Next() {
		values := make([]any, width)
		pointers := make([]any, width)

		for i := range values {
			pointers[i] = &values[i]
		}

		if err := rows.Scan(pointers...); err != nil {
			return nil, 0, fmt.Errorf("reading: %w", err)
		}

		for _, value := range values {
			if b, ok := value.([]byte); ok {
				size += int64(len(b))
			}
		}

		batch = append(batch, values)
	}

	return batch, size, rows.Err()
}

func writeBatch(ctx context.Context, dst *sql.DB, name string, columns, types []string, batch [][]any) error {
	quoted := make([]string, len(columns))

	for i, column := range columns {
		quoted[i] = `"` + column + `"`
	}

	args := make([]any, 0, len(batch)*len(columns))
	tuples := make([]string, 0, len(batch))

	for _, row := range batch {
		holders := make([]string, len(columns))

		for i := range columns {
			value, err := convert(row[i], types[i])
			if err != nil {
				return fmt.Errorf("column %q: %w", columns[i], err)
			}

			args = append(args, value)
			holders[i] = "$" + strconv.Itoa(len(args))
		}

		tuples = append(tuples, "("+strings.Join(holders, ", ")+")")
	}

	query := `INSERT INTO "` + name + `" (` + strings.Join(quoted, ", ") + `) VALUES ` + strings.Join(tuples, ", ") + ` ON CONFLICT DO NOTHING`

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}

// convert turns a MariaDB value into the value the PostgreSQL column takes.
func convert(value any, pgType string) (any, error) {
	if value == nil {
		return nil, nil
	}

	switch pgType {
	case "boolean":
		switch v := value.(type) {
		case int64:
			return v != 0, nil
		case []byte:
			return string(v) != "0" && string(v) != "", nil
		case bool:
			return v, nil
		}

	case "integer", "bigint", "smallint":
		return toInt64(value)

	case "timestamp with time zone", "timestamp without time zone":
		switch v := value.(type) {
		case time.Time:
			return v.UTC(), nil
		case []byte:
			when, err := time.Parse("2006-01-02 15:04:05", string(v))
			if err != nil {
				return nil, err
			}

			return when, nil
		}

	case "bytea":
		switch v := value.(type) {
		case []byte:
			return v, nil
		case string:
			return []byte(v), nil
		}

	default:
		switch v := value.(type) {
		case []byte:
			return string(v), nil
		case string:
			return v, nil
		case int64:
			return strconv.FormatInt(v, 10), nil
		case time.Time:
			return v.UTC().Format("2006-01-02 15:04:05"), nil
		}
	}

	return nil, fmt.Errorf("a %T cannot go into a %s column", value, pgType)
}

func toInt64(value any) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case int32:
		return int64(v), nil
	case uint64:
		return int64(v), nil
	case []byte:
		return strconv.ParseInt(string(v), 10, 64)
	case string:
		return strconv.ParseInt(v, 10, 64)
	}

	return 0, fmt.Errorf("%v (%T) is not a whole number", value, value)
}

func sourceList(columns []string) string {
	quoted := make([]string, len(columns))

	for i, column := range columns {
		quoted[i] = "`" + column + "`"
	}

	return strings.Join(quoted, ", ")
}

func indexOf(list []string, item string) int {
	for i, v := range list {
		if v == item {
			return i
		}
	}

	return -1
}

func sourceColumns(ctx context.Context, src *sql.DB, name string) (map[string]bool, error) {
	rows, err := src.QueryContext(ctx, "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", name)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	columns := map[string]bool{}

	for rows.Next() {
		var column string

		if err := rows.Scan(&column); err != nil {
			return nil, err
		}

		columns[column] = true
	}

	return columns, rows.Err()
}

func destinationColumns(ctx context.Context, dst *sql.DB, table v6Table) ([]string, []string, error) {
	rows, err := dst.QueryContext(ctx, `SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 ORDER BY ordinal_position`, table.name)
	if err != nil {
		return nil, nil, err
	}

	defer rows.Close()

	columns, types := []string{}, []string{}

	for rows.Next() {
		var column, dataType string

		if err := rows.Scan(&column, &dataType); err != nil {
			return nil, nil, err
		}

		columns = append(columns, column)
		types = append(types, dataType)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if len(columns) == 0 {
		return nil, nil, errors.New("the destination table has no columns")
	}

	return columns, types, nil
}

func tableExists(ctx context.Context, dst *sql.DB, name string) (bool, error) {
	var found sql.NullString

	if err := dst.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, `"`+name+`"`).Scan(&found); err != nil {
		return false, err
	}

	return found.Valid, nil
}

func ensureTable(ctx context.Context, dst *sql.DB, table v6Table) error {
	exists, err := tableExists(ctx, dst, table.name)
	if err != nil || exists {
		return err
	}

	for _, statement := range table.create {
		if _, err := dst.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("creating: %w", err)
		}
	}

	return nil
}
