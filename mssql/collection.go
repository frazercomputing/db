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

package mssql

import (
	db "github.com/frazercomputing/upper-io-db"
	"github.com/frazercomputing/upper-io-db/internal/sqladapter"
	"github.com/frazercomputing/upper-io-db/lib/sqlbuilder"
)

// table is the actual implementation of a collection.
type table struct {
	sqladapter.BaseCollection // Leveraged by sqladapter

	d    *database
	name string

	hasIdentityColumn *bool
}

var (
	_ = sqladapter.Collection(&table{})
	_ = db.Collection(&table{})
)

// newTable binds *table with sqladapter.
func newTable(d *database, name string) *table {
	t := &table{
		name: name,
		d:    d,
	}
	t.BaseCollection = sqladapter.NewBaseCollection(t)
	return t
}

func (t *table) Name() string {
	return t.name
}

func (t *table) Database() sqladapter.Database {
	return t.d
}

// Insert inserts an item (map or struct) into the collection.
func (t *table) Insert(item interface{}) (interface{}, error) {
	columnNames, columnValues, err := sqlbuilder.Map(item, nil)
	if err != nil {
		return nil, err
	}

	pKey := t.BaseCollection.PrimaryKeys()

	var hasKeys bool
	for i := range columnNames {
		for j := 0; j < len(pKey); j++ {
			if pKey[j] == columnNames[i] {
				if columnValues[i] != nil {
					hasKeys = true
					break
				}
			}
		}
	}

	if hasKeys {
		if t.hasIdentityColumn == nil {
			var hasIdentityColumn bool
			var identityColumns int

			row, err := t.d.QueryRow("SELECT COUNT(1) FROM sys.identity_columns WHERE OBJECT_NAME(object_id) = ?", t.Name())
			if err != nil {
				return nil, err
			}

			err = row.Scan(&identityColumns)
			if err != nil {
				return nil, err
			}

			if identityColumns > 0 {
				hasIdentityColumn = true
			}

			t.hasIdentityColumn = &hasIdentityColumn
		}

		if *t.hasIdentityColumn {
			_, err = t.d.Exec("SET IDENTITY_INSERT " + t.Name() + " ON")
			if err != nil {
				return nil, err
			}
			defer t.d.Exec("SET IDENTITY_INSERT " + t.Name() + " OFF")
		}
	}

	q := t.d.InsertInto(t.Name()).
		Columns(columnNames...).
		Values(columnValues...)

	if len(pKey) < 1 {
		_, err = q.Exec()
		if err != nil {
			return nil, err
		}
		return nil, nil
	}

	q = q.Returning(pKey...)

	var keyMap db.Cond
	if err = q.Iterator().One(&keyMap); err != nil {
		return nil, err
	}

	// The IDSetter interface does not match, look for another interface match.
	if len(keyMap) == 1 {
		return keyMap[pKey[0]], nil
	}

	// This was a compound key and no interface matched it, let's return a map.
	return keyMap, nil
}
