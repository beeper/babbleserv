package media

import (
	"context"
	"crypto/md5"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/xid"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const API_VERSION = 710

type MediaDatabase struct {
	log zerolog.Logger
	db  fdb.Database

	byVersion,
	byServerID subspace.Subspace
}

func NewMediaDatabase(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
) *MediaDatabase {
	log := logger.With().
		Str("database", "media").
		Logger()

	fdb.MustAPIVersion(API_VERSION)
	db := fdb.MustOpenDatabase(cfg.Media.Database.ClusterFilePath)
	log.Info().
		Str("cluster_file", cfg.Media.Database.ClusterFilePath).
		Msg("Connecting to FoundationDB")

	db.Options().SetTransactionTimeout(cfg.Media.Database.TransactionTimeout)
	db.Options().SetTransactionRetryLimit(cfg.Media.Database.TransactionRetryLimit)

	mediaDir, err := directory.CreateOrOpen(db, []string{"media"}, nil)
	if err != nil {
		panic(err)
	}

	log.Debug().
		Bytes("prefix", mediaDir.Bytes()).
		Msg("Init media directory")

	return &MediaDatabase{
		log: log,
		db:  db,

		byVersion:  mediaDir.Sub("ver"),
		byServerID: mediaDir.Sub("sid"),
	}
}

func (m *MediaDatabase) Stop() {
}

func (m *MediaDatabase) GenerateMediaID() string {
	sum := md5.Sum([]byte(xid.New().Bytes()))
	return util.Base64EncodeURLSafe(sum[:])
}

func (m *MediaDatabase) GetMedia(ctx context.Context, serverName, mediaID string) (*types.Media, error) {
	return util.DoReadTransaction(ctx, m.db, func(txn fdb.ReadTransaction) (*types.Media, error) {
		if b, err := txn.Get(m.keyForMedia(serverName, mediaID)).Get(); err != nil {
			return nil, err
		} else if b == nil {
			return nil, nil
		} else {
			return types.NewMediaFromBytes(b, serverName, mediaID)
		}
	})
}

func (m *MediaDatabase) CreateMedia(ctx context.Context, media *types.Media) error {
	_, err := util.DoWriteTransaction(ctx, m.db, func(txn fdb.Transaction) (*struct{}, error) {
		key := m.keyForMedia(media.ServerName, media.MediaID)

		existing, err := txn.Get(key).Get()
		if err != nil {
			return nil, err
		} else if existing != nil {
			// This should never happen!
			panic("media already exists with this ID!")
		}

		txn.Set(key, media.ToMsgpack())

		version := tuple.IncompleteVersionstamp(0)
		kv := m.keyValueForMediaVersion(media.ServerName, media.MediaID, version)
		txn.Set(kv.Key, kv.Value)

		return nil, nil
	})
	return err
}

func (m *MediaDatabase) SetMedia(ctx context.Context, media *types.Media) error {
	_, err := util.DoWriteTransaction(ctx, m.db, func(txn fdb.Transaction) (*struct{}, error) {
		txn.Set(m.keyForMedia(media.ServerName, media.MediaID), media.ToMsgpack())
		return nil, nil
	})
	return err
}

func (m *MediaDatabase) keyForMedia(serverName, mediaID string) fdb.Key {
	return m.byServerID.Pack(tuple.Tuple{serverName, mediaID})
}

func (m *MediaDatabase) keyForVersion(version tuple.Versionstamp) fdb.Key {
	if key, err := m.byVersion.PackWithVersionstamp(tuple.Tuple{version}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (m *MediaDatabase) keyValueForMediaVersion(serverName, mediaID string, version tuple.Versionstamp) fdb.KeyValue {
	return fdb.KeyValue{
		Key:   m.keyForVersion(version),
		Value: tuple.Tuple{serverName, mediaID}.Pack(),
	}
}
