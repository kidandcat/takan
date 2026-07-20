package mercadona

import (
	"context"
	"database/sql"

	"github.com/kidandcat/mercadona-mcp/sdk"
)

// LinkAccount logs into Mercadona and stores session tokens for this Takan user.
func LinkAccount(ctx context.Context, db *sql.DB, box *sdk.Box, userID, email, password, postal string) error {
	return sdk.LinkAccount(ctx, db, box, userID, email, password, postal)
}

// UnlinkAccount removes Mercadona session data for a user.
func UnlinkAccount(ctx context.Context, db *sql.DB, userID string) error {
	return sdk.UnlinkAccount(ctx, db, userID)
}

// HasLinkedSession reports whether tokens exist for userID.
func HasLinkedSession(ctx context.Context, db *sql.DB, userID string) bool {
	return sdk.HasSession(ctx, db, userID)
}
