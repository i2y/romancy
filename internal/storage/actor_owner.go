package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// deleteOwned deletes the row of table whose keyCol is key, only when its
// ownerCol is owner. A row that belongs to someone else is ErrNotOwner; no
// row at all is a no-op (already revoked or never there).
func deleteOwned(ctx context.Context, conn Executor, ph func(int) string, table, keyCol, ownerCol, key, owner string) error {
	res, err := conn.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE %s = %s AND %s = %s", table, keyCol, ph(1), ownerCol, ph(2)), key, owner)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var one int
	err = conn.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE %s = %s", table, keyCol, ph(1)), key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrNotOwner
}
