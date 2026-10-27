package postgres

import (
	"context"

	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
)

// ImportProposals atomically inserts new catalog rows. Replays cannot overwrite
// owner edits, targets, stock, or provenance on existing items.
func (s *Store) ImportProposals(ctx context.Context, rows []inventory.Proposal) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var added int64
	for _, row := range rows {
		tag, err := tx.Exec(ctx, `INSERT INTO logistics.items(id,name,category,proposed_desired,proposed_unit,source)
		 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING`, row.ID, row.Name, row.Category, row.Quantity, row.Unit, row.Source)
		if err != nil {
			return 0, err
		}
		added += tag.RowsAffected()
	}
	return added, tx.Commit(ctx)
}
