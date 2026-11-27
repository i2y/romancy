//go:build integration

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Integration builds run every forEachBackend test against PostgreSQL and
// MySQL as well. One container per dialect is shared by the whole package
// run (testcontainers' reaper removes it at exit); each test gets its own
// freshly migrated database.
func init() {
	storageBackends = append(storageBackends,
		storageBackend{name: "postgres", open: openPostgresBackend},
		storageBackend{name: "mysql", open: openMySQLBackend},
	)
}

type sharedServer struct {
	once  sync.Once
	err   error
	admin *sql.DB
	// url returns the connection string production code would be given for
	// the named database.
	url func(dbName string) string
}

var (
	pgServer, mysqlServer sharedServer
	backendDBSeq          atomic.Int64
)

func nextBackendDBName() string {
	return fmt.Sprintf("conf_%d_%d", time.Now().UnixNano()%1_000_000, backendDBSeq.Add(1))
}

func openPostgresBackend(t *testing.T) *testDB {
	t.Helper()
	ctx := context.Background()
	pgServer.once.Do(func() {
		c, err := postgres.Run(ctx, "postgres:16-alpine",
			postgres.WithDatabase("romancy"),
			postgres.WithUsername("romancy"),
			postgres.WithPassword("romancy"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(5*time.Minute)),
		)
		if err != nil {
			pgServer.err = err
			return
		}
		connStr, err := c.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			pgServer.err = err
			return
		}
		pgServer.admin, pgServer.err = sql.Open("pgx", connStr)
		pgServer.url = func(dbName string) string {
			return strings.Replace(connStr, "/romancy?", "/"+dbName+"?", 1)
		}
	})
	if pgServer.err != nil {
		t.Fatalf("postgres container: %v", pgServer.err)
	}

	name := nextBackendDBName()
	if _, err := pgServer.admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	store, err := NewPostgresStorage(pgServer.url(name))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_, _ = pgServer.admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	if err := InitializeTestSchemaPostgres(ctx, store); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}
	return &testDB{Storage: store, db: store.DB(), placeholder: store.driver.Placeholder}
}

func openMySQLBackend(t *testing.T) *testDB {
	t.Helper()
	ctx := context.Background()
	mysqlServer.once.Do(func() {
		c, err := mysql.Run(ctx, "mysql:8.0",
			mysql.WithDatabase("romancy"),
			mysql.WithUsername("root"),
			mysql.WithPassword("romancy"),
		)
		if err != nil {
			mysqlServer.err = err
			return
		}
		host, err := c.Host(ctx)
		if err != nil {
			mysqlServer.err = err
			return
		}
		port, err := c.MappedPort(ctx, "3306/tcp")
		if err != nil {
			mysqlServer.err = err
			return
		}
		mysqlServer.admin, mysqlServer.err = sql.Open("mysql",
			fmt.Sprintf("root:romancy@tcp(%s:%s)/", host, port.Port()))
		// The production URL form, so convertToMySQLDSN supplies the DSN
		// options real deployments get (notably: no clientFoundRows).
		mysqlServer.url = func(dbName string) string {
			return fmt.Sprintf("mysql://root:romancy@%s:%s/%s", host, port.Port(), dbName)
		}
	})
	if mysqlServer.err != nil {
		t.Fatalf("mysql container: %v", mysqlServer.err)
	}

	name := nextBackendDBName()
	if _, err := mysqlServer.admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	store, err := NewMySQLStorage(mysqlServer.url(name))
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_, _ = mysqlServer.admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})
	if err := InitializeTestSchemaMySQL(ctx, store); err != nil {
		t.Fatalf("migrate mysql: %v", err)
	}
	return &testDB{Storage: store, db: store.DB(), placeholder: store.driver.Placeholder}
}
