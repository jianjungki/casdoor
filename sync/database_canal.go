// Copyright 2023 The Casdoor Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sync

import (
	"fmt"
	"strings"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/siddontang/go-log/log"
	"github.com/xorm-io/xorm/core"
)

func (db *Database) OnGTID(header *replication.EventHeader, gtid mysql.GTIDSet) error {
	db.Gtid = gtid.String()
	return nil
}

func (db *Database) onDDL(header *replication.EventHeader, nextPos mysql.Position, queryEvent *replication.QueryEvent) error {
	return nil
}

// setGtidNext pins GTID_NEXT on the given tx so the writes applied on the
// target library carry the source GTID, which prevents loopback (the same
// event being re-applied back-and-forth between the two instances).
func (db *Database) setGtidNext(tx *core.Tx) error {
	if db.Gtid == "" {
		return nil
	}
	_, err := tx.Exec(fmt.Sprintf("SET GTID_NEXT= '%s'", db.Gtid))
	return err
}

func (db *Database) resetGtidNext(tx *core.Tx) error {
	_, err := tx.Exec("SET GTID_NEXT='automatic'")
	return err
}

// runInTx executes fn inside a single transaction pinned to one connection.
// This is critical because SET GTID_NEXT is connection-scoped: running it and
// the writes on different pooled connections would silently drop the
// loopback-protection pinning.
//
// If fn returns an error, the transaction is rolled back so a failed batch
// cannot leak an open transaction (which previously stalled later events and
// looked like a "sync timeout").
func (db *Database) runInTx(setGtid bool, fn func(tx *core.Tx) error) error {
	tx, err := db.engine.DB().Begin()
	if err != nil {
		return err
	}

	if setGtid {
		if err := db.setGtidNext(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	if setGtid {
		if err := db.resetGtidNext(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// fmtValue renders a binlog cell value into the SQL argument representation.
// It preserves NULL properly and avoids the previous "%!d(<nil>)" corruption.
func fmtValue(item interface{}, isChar bool) interface{} {
	if item == nil {
		return nil
	}
	if isChar {
		return fmt.Sprintf("%v", item)
	}
	switch v := item.(type) {
	case []byte:
		// NUM columns from go-mysql are already decoded, but guard anyway.
		return string(v)
	default:
		return v
	}
}

func (db *Database) OnRow(e *canal.RowsEvent) error {
	// Loopback protection: if the current GTID already originates from this
	// instance, the corresponding change was written by us and must NOT be
	// re-applied, otherwise the two instances would ping-pong forever.
	if strings.Contains(db.Gtid, db.serverUuid) {
		return nil
	}

	length := len(e.Table.Columns)
	columnNames := make([]string, length)
	oldColumnValue := make([]interface{}, length)
	newColumnValue := make([]interface{}, length)
	isChar := make([]bool, len(e.Table.Columns))

	for i, col := range e.Table.Columns {
		columnNames[i] = col.Name
		isChar[i] = col.Type > 2
	}

	// get pk column name
	pkColumnNames := getPkColumnNames(columnNames, e.Table.PKColumns)

	// We are applying an event that did not originate from this instance, so
	// pin GTID_NEXT on the very same transaction connection (loopback protection).
	setGtid := true

	switch e.Action {
	case canal.UpdateAction:
		return db.runInTx(setGtid, func(tx *core.Tx) error {
			for i, row := range e.Rows {
				for j, item := range row {
					if i%2 == 0 {
						oldColumnValue[j] = fmtValue(item, isChar[j])
					} else {
						newColumnValue[j] = fmtValue(item, isChar[j])
					}
				}

				if i%2 == 1 {
					pkColumnValue := getPkColumnValues(oldColumnValue, e.Table.PKColumns)
					updateSql, args, err := getUpdateSql(e.Table.Schema, e.Table.Name, columnNames, newColumnValue, pkColumnNames, pkColumnValue)
					if err != nil {
						return err
					}

					if _, err := tx.Exec(updateSql, args...); err != nil {
						return err
					}
				}
			}
			return nil
		})
	case canal.DeleteAction:
		return db.runInTx(setGtid, func(tx *core.Tx) error {
			for _, row := range e.Rows {
				for j, item := range row {
					oldColumnValue[j] = fmtValue(item, isChar[j])
				}

				pkColumnValue := getPkColumnValues(oldColumnValue, e.Table.PKColumns)
				deleteSql, args, err := getDeleteSql(e.Table.Schema, e.Table.Name, pkColumnNames, pkColumnValue)
				if err != nil {
					return err
				}

				if _, err := tx.Exec(deleteSql, args...); err != nil {
					return err
				}
			}
			return nil
		})
	case canal.InsertAction:
		return db.runInTx(setGtid, func(tx *core.Tx) error {
			for _, row := range e.Rows {
				for j, item := range row {
					newColumnValue[j] = fmtValue(item, isChar[j])
				}

				insertSql, args, err := getInsertSql(e.Table.Schema, e.Table.Name, columnNames, newColumnValue)
				if err != nil {
					return err
				}

				if _, err := tx.Exec(insertSql, args...); err != nil {
					return err
				}
			}
			return nil
		})
	default:
		log.Infof("%v", e.String())
		return nil
	}
}

func (db *Database) String() string {
	return "Database"
}
