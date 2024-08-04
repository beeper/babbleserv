package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

func (s *ServersDirectory) TxnGetCurrentServerPosition(
	txn fdb.ReadTransaction,
	serverName string,
) (string, error) {
	value, err := txn.Get(s.KeyForServerPosition(serverName)).Get()
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func (s *ServersDirectory) TxnSetCurrentServerPosition(
	txn fdb.Transaction,
	serverName, token string,
) {
	txn.Set(s.KeyForServerPosition(serverName), []byte(token))
}
