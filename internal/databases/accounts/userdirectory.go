package accounts

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) SearchUserDirectoryCandidates(
	ctx context.Context,
	searchTerm string,
	maxResults int,
	maxIndexScan int,
) ([]*types.UserDirectoryCandidate, bool, error) {
	type result struct {
		candidates []*types.UserDirectoryCandidate
		limited    bool
	}
	res, err := util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (result, error) {
		candidates, limited, err := a.users.TxnSearchUserDirectory(txn, searchTerm, maxResults, maxIndexScan)
		return result{candidates: candidates, limited: limited}, err
	})
	return res.candidates, res.limited, err
}
