// Copyright (c) 2012-present The upper.io/db authors. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining
// a copy of this software and associated documentation files (the
// "Software"), to deal in the Software without restriction, including
// without limitation the rights to use, copy, modify, merge, publish,
// distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
// NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
// LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
// OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
// WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

// Package postgresql wraps the github.com/lib/pq PostgreSQL driver. See
// https://github.com/frazercomputing/upper-io-db/postgresql for documentation, particularities and
// usage examples.
package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver.
	db "github.com/frazercomputing/upper-io-db"
	"github.com/frazercomputing/upper-io-db/internal/sqladapter"
	"github.com/frazercomputing/upper-io-db/internal/sqladapter/compat"
	"github.com/frazercomputing/upper-io-db/internal/sqladapter/exql"
	"github.com/frazercomputing/upper-io-db/lib/sqlbuilder"
)

// database is the actual implementation of Database
type database struct {
	sqladapter.BaseDatabase

	sqlbuilder.SQLBuilder

	connURL db.ConnectionURL
	mu      sync.Mutex
}

var (
	_ = sqlbuilder.Database(&database{})
	_ = sqladapter.Database(&database{})
)

// newDatabase creates a new *database session for internal use.
func newDatabase(settings db.ConnectionURL) *database {
	return &database{
		connURL: settings,
	}
}

// ConnectionURL returns this database session's connection URL, if any.
func (d *database) ConnectionURL() db.ConnectionURL {
	return d.connURL
}

// Open attempts to open a connection with the database server.
func (d *database) Open(connURL db.ConnectionURL) error {
	if connURL == nil {
		return db.ErrMissingConnURL
	}
	d.connURL = connURL
	return d.open()
}

// NewTx begins a transaction block with the given context.
func (d *database) NewTx(ctx context.Context) (sqlbuilder.Tx, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	nTx, err := d.NewDatabaseTx(ctx)
	if err != nil {
		return nil, err
	}
	return &tx{DatabaseTx: nTx}, nil
}

// Collections returns a list of non-system tables from the database.
func (d *database) Collections() (collections []string, err error) {
	q := d.Select("table_name").
		From("information_schema.tables").
		Where("table_schema = ?", "public")

	iter := q.Iterator()
	defer iter.Close()

	for iter.Next() {
		var tableName string
		if err := iter.Scan(&tableName); err != nil {
			return nil, err
		}
		collections = append(collections, tableName)
	}

	return collections, nil
}

// open attempts to establish a connection with the PostgreSQL server.
func (d *database) open() error {
	// Binding with sqladapter's logic.
	d.BaseDatabase = sqladapter.NewBaseDatabase(d)

	// Binding with sqlbuilder.
	d.SQLBuilder = sqlbuilder.WithSession(d.BaseDatabase, template)

	connFn := func() error {
		sess, err := sql.Open("postgres", d.ConnectionURL().String())
		if err == nil {
			sess.SetConnMaxLifetime(db.DefaultSettings.ConnMaxLifetime())
			sess.SetMaxIdleConns(db.DefaultSettings.MaxIdleConns())
			sess.SetMaxOpenConns(db.DefaultSettings.MaxOpenConns())
			return d.BaseDatabase.BindSession(sess)
		}
		return err
	}

	if err := d.BaseDatabase.WaitForConnection(connFn); err != nil {
		return err
	}

	return nil
}

// Clone creates a copy of the database session on the given context.
func (d *database) clone(ctx context.Context, checkConn bool) (*database, error) {
	clone := newDatabase(d.connURL)

	var err error
	clone.BaseDatabase, err = d.NewClone(clone, checkConn)
	if err != nil {
		return nil, err
	}

	clone.SetContext(ctx)

	clone.SQLBuilder = sqlbuilder.WithSession(clone.BaseDatabase, template)

	return clone, nil
}

func (d *database) ConvertValues(values []interface{}) []interface{} {
	for i := range values {
		switch v := values[i].(type) {
		case *string, *bool, *int, *uint, *int64, *uint64, *int32, *uint32, *int16, *uint16, *int8, *uint8, *float32, *float64, *[]uint8, sql.Scanner, *sql.Scanner, *time.Time:
			// Handled by pq.
		case string, bool, int, uint, int64, uint64, int32, uint32, int16, uint16, int8, uint8, float32, float64, []uint8, driver.Valuer, *driver.Valuer, time.Time:
			// Handled by pq.
		case StringArray, Int64Array, BoolArray, GenericArray, Float64Array, JSONBMap, JSONB:
			// Already with scanner/valuer.
		case *StringArray, *Int64Array, *BoolArray, *GenericArray, *Float64Array, *JSONBMap, *JSONB:
			// Already with scanner/valuer.

		case *[]int64:
			values[i] = (*Int64Array)(v)
		case *[]string:
			values[i] = (*StringArray)(v)
		case *[]float64:
			values[i] = (*Float64Array)(v)
		case *[]bool:
			values[i] = (*BoolArray)(v)
		case *map[string]interface{}:
			values[i] = (*JSONBMap)(v)

		case []int64:
			values[i] = (*Int64Array)(&v)
		case []string:
			values[i] = (*StringArray)(&v)
		case []float64:
			values[i] = (*Float64Array)(&v)
		case []bool:
			values[i] = (*BoolArray)(&v)
		case map[string]interface{}:
			values[i] = (*JSONBMap)(&v)

		case sqlbuilder.ValueWrapper:
			values[i] = v.WrapValue(v)

		default:
			values[i] = autoWrap(reflect.ValueOf(values[i]), values[i])
		}

	}
	return values
}

// CompileStatement compiles a *exql.Statement into arguments that sql/database
// accepts.
func (d *database) CompileStatement(stmt *exql.Statement, args []interface{}) (string, []interface{}) {
	compiled, err := stmt.Compile(template)
	if err != nil {
		panic(err.Error())
	}
	query, args := sqlbuilder.Preprocess(compiled, args)
	return sqladapter.ReplaceWithDollarSign(query), args
}

// Err allows sqladapter to translate specific PostgreSQL string errors into
// custom error values.
func (d *database) Err(err error) error {
	if err != nil {
		s := err.Error()
		// These errors are not exported so we have to check them by they string value.
		if strings.Contains(s, `too many clients`) || strings.Contains(s, `remaining connection slots are reserved`) || strings.Contains(s, `too many open`) {
			return db.ErrTooManyClients
		}
	}
	return err
}

// NewCollection creates a db.Collection by name.
func (d *database) NewCollection(name string) db.Collection {
	return newCollection(d, name)
}

// Tx creates a transaction block on the given context and passes it to the
// function fn. If fn returns no error the transaction is commited, else the
// transaction is rolled back. After being commited or rolled back the
// transaction is closed automatically.
func (d *database) Tx(ctx context.Context, fn func(tx sqlbuilder.Tx) error) error {
	return sqladapter.RunTx(d, ctx, fn)
}

// NewDatabaseTx begins a transaction block.
func (d *database) NewDatabaseTx(ctx context.Context) (sqladapter.DatabaseTx, error) {
	clone, err := d.clone(ctx, true)
	if err != nil {
		return nil, err
	}
	clone.mu.Lock()
	defer clone.mu.Unlock()

	connFn := func() error {
		sqlTx, err := compat.BeginTx(clone.BaseDatabase.Session(), ctx, clone.TxOptions())
		if err == nil {
			return clone.BindTx(ctx, sqlTx)
		}
		return err
	}

	if err := clone.BaseDatabase.WaitForConnection(connFn); err != nil {
		return nil, err
	}

	return sqladapter.NewDatabaseTx(clone), nil
}

// LookupName looks for the name of the database and it's often used as a
// test to determine if the connection settings are valid.
func (d *database) LookupName() (string, error) {
	q := d.Select(db.Raw("CURRENT_DATABASE() AS name"))

	iter := q.Iterator()
	defer iter.Close()

	if iter.Next() {
		var name string
		err := iter.Scan(&name)
		return name, err
	}

	return "", iter.Err()
}

// TableExists returns an error if the given table name does not exist on the
// database.
func (d *database) TableExists(name string) error {
	q := d.Select("table_name").
		From("information_schema.tables").
		Where("table_catalog = ? AND table_name = ?", d.BaseDatabase.Name(), name)

	iter := q.Iterator()
	defer iter.Close()

	if iter.Next() {
		var name string
		if err := iter.Scan(&name); err != nil {
			return err
		}
		return nil
	}
	return db.ErrCollectionDoesNotExist
}

// quotedTableName returns a valid regclass name for both regular tables and
// for schemas.
func quotedTableName(s string) string {
	chunks := strings.Split(s, ".")
	for i := range chunks {
		chunks[i] = fmt.Sprintf("%q", chunks[i])
	}
	return strings.Join(chunks, ".")
}

// PrimaryKeys returns the names of all the primary keys on the table.
func (d *database) PrimaryKeys(tableName string) ([]string, error) {
	q := d.Select("pg_attribute.attname AS pkey").
		From("pg_index", "pg_class", "pg_attribute").
		Where(`
			pg_class.oid = '` + quotedTableName(tableName) + `'::regclass
			AND indrelid = pg_class.oid
			AND pg_attribute.attrelid = pg_class.oid
			AND pg_attribute.attnum = ANY(pg_index.indkey)
			AND indisprimary
		`).OrderBy("pkey")

	iter := q.Iterator()
	defer iter.Close()

	pk := []string{}

	for iter.Next() {
		var k string
		if err := iter.Scan(&k); err != nil {
			return nil, err
		}
		pk = append(pk, k)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}

	return pk, nil
}

// WithContext creates a copy of the session on the given context.
func (d *database) WithContext(ctx context.Context) sqlbuilder.Database {
	newDB, _ := d.clone(ctx, false)
	return newDB
}
