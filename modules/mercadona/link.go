package mercadona

import (
	"context"
	"database/sql"
)

// HasLinkedSession reports whether Mercadona session tokens exist for userID.
func HasLinkedSession(ctx context.Context, db *sql.DB, userID string) bool {
	return HasSession(ctx, db, userID)
}
