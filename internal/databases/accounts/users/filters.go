package users

import (
	"encoding/json"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (u *UsersDirectory) keyForNewUserFilter(username string, version tuple.Versionstamp) fdb.Key {
	key, err := u.userFilters.PackWithVersionstamp(tuple.Tuple{username, version})
	if err != nil {
		panic(err)
	}
	return key
}

func (u *UsersDirectory) keyForUserFilter(username string, version tuple.Versionstamp) fdb.Key {
	return u.userFilters.Pack(tuple.Tuple{username, version})
}

func (u *UsersDirectory) TxnGetUserFilter(txn fdb.ReadTransaction, userID id.UserID, version tuple.Versionstamp) (json.RawMessage, error) {
	if userID.Homeserver() != u.serverName {
		return nil, fmt.Errorf("userid is not local: %s", userID)
	}

	key := u.keyForUserFilter(userID.Localpart(), version)
	return txn.Get(key).Get()
}

func (u *UsersDirectory) TxnStoreUserFilter(txn fdb.Transaction, userID id.UserID, filter json.RawMessage, version tuple.Versionstamp) error {
	if userID.Homeserver() != u.serverName {
		return fmt.Errorf("userid is not local: %s", userID)
	}

	if types.IsIncompleteVersionstamp(version) {
		txn.SetVersionstampedKey(u.keyForNewUserFilter(userID.Localpart(), version), filter)
	} else {
		txn.Set(u.keyForUserFilter(userID.Localpart(), version), filter)
	}
	return nil
}
