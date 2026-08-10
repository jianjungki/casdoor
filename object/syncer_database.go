// Copyright 2025 The Casdoor Authors. All Rights Reserved.
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

package object

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/casdoor/casdoor/util"
	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/ssh"
)

// DatabaseSyncerProvider implements SyncerProvider for database-based syncers
type DatabaseSyncerProvider struct {
	Syncer *Syncer
}

// InitAdapter initializes the database adapter
func (p *DatabaseSyncerProvider) InitAdapter() error {
	if p.Syncer.Ormer != nil {
		return nil
	}

	var dataSourceName string
	if p.Syncer.DatabaseType == "mssql" {
		dataSourceName = fmt.Sprintf("sqlserver://%s:%s@%s:%d?database=%s", p.Syncer.User, p.Syncer.Password, p.Syncer.Host, p.Syncer.Port, p.Syncer.Database)
	} else if p.Syncer.DatabaseType == "postgres" {
		sslMode := "disable"
		if p.Syncer.SslMode != "" {
			sslMode = p.Syncer.SslMode
		}
		dataSourceName = fmt.Sprintf("user=%s password=%s host=%s port=%d sslmode=%s dbname=%s", p.Syncer.User, p.Syncer.Password, p.Syncer.Host, p.Syncer.Port, sslMode, p.Syncer.Database)
	} else {
		dataSourceName = fmt.Sprintf("%s:%s@tcp(%s:%d)/", p.Syncer.User, p.Syncer.Password, p.Syncer.Host, p.Syncer.Port)
	}

	var db *sql.DB
	var err error

	if p.Syncer.SshType != "" && (p.Syncer.DatabaseType == "mysql" || p.Syncer.DatabaseType == "postgres" || p.Syncer.DatabaseType == "mssql") {
		var dial *ssh.Client
		if p.Syncer.SshType == "password" {
			dial, err = DialWithPassword(p.Syncer.SshUser, p.Syncer.SshPassword, p.Syncer.SshHost, p.Syncer.SshPort)
		} else {
			dial, err = DialWithCert(p.Syncer.SshUser, p.Syncer.Owner+"/"+p.Syncer.Cert, p.Syncer.SshHost, p.Syncer.SshPort)
		}
		if err != nil {
			return err
		}

		// Store SSH client for proper cleanup
		p.Syncer.SshClient = dial

		if p.Syncer.DatabaseType == "mysql" {
			dataSourceName = fmt.Sprintf("%s:%s@%s(%s:%d)/", p.Syncer.User, p.Syncer.Password, p.Syncer.Owner+p.Syncer.Name, p.Syncer.Host, p.Syncer.Port)
			mysql.RegisterDialContext(p.Syncer.Owner+p.Syncer.Name, (&ViaSSHDialer{Client: dial, Context: nil}).MysqlDial)
		} else if p.Syncer.DatabaseType == "postgres" || p.Syncer.DatabaseType == "mssql" {
			db = sql.OpenDB(dsnConnector{dsn: dataSourceName, driver: &ViaSSHDialer{Client: dial, Context: nil, DatabaseType: p.Syncer.DatabaseType}})
		}
	}

	if !isCloudIntranet {
		dataSourceName = strings.ReplaceAll(dataSourceName, "dbi.", "db.")
	}

	if db != nil {
		p.Syncer.Ormer, err = NewAdapterFromDb(p.Syncer.DatabaseType, dataSourceName, p.Syncer.Database, db)
	} else {
		p.Syncer.Ormer, err = NewAdapter(p.Syncer.DatabaseType, dataSourceName, p.Syncer.Database)
	}

	return err
}

const (
	// dbSyncerQueryTimeoutEnv controls the per-query timeout (in seconds) used
	// when reading the upstream database. This prevents a slow/hung query from
	// blocking the syncer run forever.
	dbSyncerQueryTimeoutEnv = "CASDOOR_SYNCER_DB_QUERY_TIMEOUT"

	// dbSyncerQueryTimeoutDefault is the default per-query timeout (5 minutes).
	dbSyncerQueryTimeoutDefault = 5 * time.Minute

	// dbSyncerPageSize is how many rows are fetched per page when reading a
	// large upstream table, so we never load the whole table into memory at once.
	dbSyncerPageSize = 1000
)

// syncerDBContext returns a context with a per-query timeout for reading the
// upstream database, read once from the environment and cached.
func syncerDBContext(owner, name string) (context.Context, context.CancelFunc) {
	timeout := dbSyncerQueryTimeoutDefault
	if s := os.Getenv(dbSyncerQueryTimeoutEnv); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			timeout = time.Duration(v) * time.Second
		} else {
			fmt.Printf("[syncer: %s/%s][WARN] invalid %s value %q, using default %s\n", owner, name, dbSyncerQueryTimeoutEnv, s, dbSyncerQueryTimeoutDefault)
		}
	}
	return context.WithTimeout(context.Background(), timeout)
}

// GetOriginalUsers retrieves all users from the database, with a per-query
// timeout and paged reads so a huge upstream table does not hang the sync or
// blow up memory.
func (p *DatabaseSyncerProvider) GetOriginalUsers() ([]*OriginalUser, error) {
	table := p.Syncer.getTable()

	ctx, cancel := syncerDBContext(p.Syncer.Owner, p.Syncer.Name)
	defer cancel()

	users := []*OriginalUser{}
	offset := 0
	for {
		var results []map[string]sql.NullString
		err := p.Syncer.Ormer.Engine.Context(ctx).Table(table).Limit(dbSyncerPageSize, offset).Find(&results)
		if err != nil {
			return nil, err
		}

		// Memory leak problem handling
		// https://github.com/casdoor/casdoor/issues/1256
		users = append(users, p.Syncer.getOriginalUsersFromMap(results)...)
		// Clear map contents to help garbage collection
		for i := range results {
			for k := range results[i] {
				delete(results[i], k)
			}
		}
		results = nil

		if len(users)-offset < dbSyncerPageSize {
			break
		}
		offset += dbSyncerPageSize
	}

	return users, nil
}

// AddUser adds a new user to the database
func (p *DatabaseSyncerProvider) AddUser(user *OriginalUser) (bool, error) {
	m := p.Syncer.getMapFromOriginalUser(user)
	affected, err := p.Syncer.Ormer.Engine.Table(p.Syncer.getTable()).Insert(m)
	if err != nil {
		return false, err
	}
	return affected != 0, nil
}

// UpdateUser updates an existing user in the database
func (p *DatabaseSyncerProvider) UpdateUser(user *OriginalUser) (bool, error) {
	key := p.Syncer.getTargetTablePrimaryKey()
	if !util.FilterSQLIdentifier(key) {
		return false, fmt.Errorf("object.UpdateUser: invalid primary key column name: %s", key)
	}

	m := p.Syncer.getMapFromOriginalUser(user)
	pkValue := m[key]
	delete(m, key)

	affected, err := p.Syncer.Ormer.Engine.Table(p.Syncer.getTable()).Where(fmt.Sprintf("%s = ?", key), pkValue).Update(&m)
	if err != nil {
		return false, err
	}
	return affected != 0, nil
}

// TestConnection tests the database connection
func (p *DatabaseSyncerProvider) TestConnection() error {
	err := p.InitAdapter()
	if err != nil {
		return err
	}

	err = p.Syncer.Ormer.Engine.Ping()
	if err != nil {
		return err
	}
	return nil
}

// Close closes the database connection and SSH tunnel
func (p *DatabaseSyncerProvider) Close() error {
	return p.Syncer.Close()
}

type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (t dsnConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return t.driver.Open(t.dsn)
}

func (t dsnConnector) Driver() driver.Driver {
	return t.driver
}

// GetOriginalGroups retrieves all groups from Database (not implemented yet)
func (p *DatabaseSyncerProvider) GetOriginalGroups() ([]*OriginalGroup, error) {
	// TODO: Implement Database group sync
	return []*OriginalGroup{}, nil
}

// GetOriginalUserGroups retrieves the group IDs that a user belongs to (not implemented yet)
func (p *DatabaseSyncerProvider) GetOriginalUserGroups(userId string) ([]string, error) {
	// TODO: Implement Database user group membership sync
	return []string{}, nil
}
