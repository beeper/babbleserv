package users

import (
	"encoding/json"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix"
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

func (u *UsersDirectory) TxnGetUserFilter(txn fdb.ReadTransaction, userID id.UserID, version tuple.Versionstamp) (*mautrix.Filter, error) {
	if userID.Homeserver() != u.serverName {
		return nil, fmt.Errorf("userid is not local: %s", userID)
	}

	key := u.keyForUserFilter(userID.Localpart(), version)
	b, err := txn.Get(key).Get()
	if err != nil {
		return nil, err
	} else if b == nil {
		return nil, nil
	}

	var filter mautrix.Filter
	if err := json.Unmarshal(b, &filter); err != nil {
		return nil, err
	}

	return &filter, nil
}

func (u *UsersDirectory) TxnStoreUserFilter(txn fdb.Transaction, userID id.UserID, filter mautrix.Filter, version tuple.Versionstamp) error {
	if userID.Homeserver() != u.serverName {
		return fmt.Errorf("userid is not local: %s", userID)
	}

	b, err := json.Marshal(filter)
	if err != nil {
		return err
	}

	var key fdb.Key
	if types.IsIncompleteVersionstamp(version) {
		key = u.keyForNewUserFilter(userID.Localpart(), version)
	} else {
		key = u.keyForUserFilter(userID.Localpart(), version)
	}

	txn.SetVersionstampedKey(key, b)
	return nil
}
