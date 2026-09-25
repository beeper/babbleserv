package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// MaxRoomTagsBytes bounds tag requests and the complete stored account-data
// value, leaving headroom below FoundationDB's value size limit.
const MaxRoomTagsBytes = 64 * 1024

var ErrRoomTagsTooLarge = errors.New("room tags exceed the storage limit")

type RoomTagsContent struct {
	Tags map[string]json.RawMessage `json:"tags"`
}

func (a *AccountsDatabase) SetAccountData(ctx context.Context, ads []*types.AccountData) error {
	_, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		for i, ad := range ads {
			a.txnSetAccountData(txn, ad, tuple.IncompleteVersionstamp(uint16(i)))
		}
		return nil, nil
	})
	return err
}

func (a *AccountsDatabase) txnSetAccountData(txn fdb.Transaction, ad *types.AccountData, version tuple.Versionstamp) {
	versionKey := a.accountdata.KeyForAccountDataVersion(ad.AccountDataTup)
	prevVersion := types.ZeroVersionstamp
	if b := txn.Get(versionKey).MustGet(); b != nil {
		prevVersion = types.MustBytesToVersionstamp(b)
	}

	// Add to user version, remove any old version
	txn.SetVersionstampedKey(a.accountdata.KeyForUserVersion(ad.UserID, version), ad.ToBytes())
	if prevVersion != types.ZeroVersionstamp {
		txn.Clear(a.accountdata.KeyForUserVersion(ad.UserID, prevVersion))
	}

	// Update version key
	txn.SetVersionstampedValue(versionKey, types.MustVersionstampToBytes(version))
}

func (a *AccountsDatabase) GetAccountData(
	ctx context.Context,
	userID id.UserID,
	roomID id.RoomID,
	adType event.Type,
) (*types.AccountData, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.AccountData, error) {
		return a.txnGetAccountData(txn, types.AccountDataTup{
			UserID: userID,
			RoomID: roomID,
			Type:   adType,
		})
	})
}

func (a *AccountsDatabase) txnGetAccountData(txn fdb.ReadTransaction, tup types.AccountDataTup) (*types.AccountData, error) {
	versionBytes := txn.Get(a.accountdata.KeyForAccountDataVersion(tup)).MustGet()
	if versionBytes == nil {
		return nil, nil
	}
	version := types.MustBytesToVersionstamp(versionBytes)

	adBytes := txn.Get(a.accountdata.KeyForUserVersion(tup.UserID, version)).MustGet()
	if adBytes == nil {
		// This should never happen!
		panic("got nil account data but we have a version!")
	}

	return types.BytesToAccountData(adBytes)
}

func (a *AccountsDatabase) UpdateRoomTag(
	ctx context.Context,
	userID id.UserID,
	roomID id.RoomID,
	tag string,
	content json.RawMessage,
	remove bool,
) error {
	tup := types.AccountDataTup{UserID: userID, RoomID: roomID, Type: event.AccountDataRoomTags}
	_, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		fields := make(map[string]json.RawMessage)
		tags := make(map[string]json.RawMessage)
		ad, err := a.txnGetAccountData(txn, tup)
		if err != nil {
			return nil, fmt.Errorf("decode m.tag account data: %w", err)
		}
		if ad != nil {
			if err := json.Unmarshal(ad.Content, &fields); err != nil {
				return nil, fmt.Errorf("decode m.tag content: %w", err)
			}
			if raw, ok := fields["tags"]; ok {
				if err := json.Unmarshal(raw, &tags); err != nil {
					return nil, fmt.Errorf("decode room tags: %w", err)
				}
			}
		}
		if fields == nil {
			fields = make(map[string]json.RawMessage)
		}
		if tags == nil {
			tags = make(map[string]json.RawMessage)
		}
		if remove {
			if _, exists := tags[tag]; !exists {
				return nil, nil
			}
			delete(tags, tag)
		} else {
			if bytes.Equal(tags[tag], content) {
				return nil, nil
			}
			tags[tag] = content
		}
		// Retain extension fields written through the account-data API, as well
		// as unknown fields within each tag. Only this one tag is changed.
		encodedTags, err := json.Marshal(tags)
		if err != nil {
			return nil, err
		}
		fields["tags"] = encodedTags
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		ad = &types.AccountData{AccountDataTup: tup, Content: encoded}
		if !remove && len(ad.ToBytes()) > MaxRoomTagsBytes {
			return nil, ErrRoomTagsTooLarge
		}
		a.txnSetAccountData(txn, ad, tuple.IncompleteVersionstamp(0))
		return nil, nil
	})
	if err == nil {
		// Notify even on an idempotent retry: a previous commit may have
		// succeeded with an unknown result, making the retry a no-op.
		a.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{userID}})
	}
	return err
}
