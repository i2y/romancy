package storage

import (
	"net/url"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// Every MySQL session runs at READ COMMITTED in UTC, whichever form the
// connection string takes (SEMANTICS §1).
func TestMySQLDSN_SessionSettings(t *testing.T) {
	for _, in := range []string{
		"mysql://u:p@db:3306/app",
		"u:p@tcp(db:3306)/app",
		"u:p@tcp(db:3306)/app?charset=utf8mb4",
	} {
		dsn, err := convertToMySQLDSN(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", in, dsn, err)
		}
		if got := cfg.Params["transaction_isolation"]; got != "'READ-COMMITTED'" {
			t.Errorf("%s: transaction_isolation = %q", in, got)
		}
		if got := cfg.Params["time_zone"]; got != "'+00:00'" {
			t.Errorf("%s: time_zone = %q", in, got)
		}
		if !cfg.ClientFoundRows || !cfg.ParseTime || cfg.Loc.String() != "UTC" {
			t.Errorf("%s: clientFoundRows=%v parseTime=%v loc=%v", in, cfg.ClientFoundRows, cfg.ParseTime, cfg.Loc)
		}
	}
	// An explicit setting is kept.
	dsn, err := convertToMySQLDSN("mysql://u:p@db/app?transaction_isolation=" + url.QueryEscape("'SERIALIZABLE'"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "SERIALIZABLE") || strings.Contains(dsn, "READ-COMMITTED") {
		t.Errorf("explicit isolation overridden: %s", dsn)
	}
}
