package media

import (
	"context"
	"crypto/md5"
	"errors"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/xid"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const API_VERSION = 710

var (
	ErrMediaNotFound        = errors.New("media not found")
	ErrMediaForbidden       = errors.New("media belongs to another user")
	ErrMediaAlreadyExists   = errors.New("media ID already exists")
	ErrMediaAlreadyUploaded = errors.New("media already uploaded")
)

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
			return nil, ErrMediaAlreadyExists
		}

		txn.Set(key, media.ToMsgpack())

		version := tuple.IncompleteVersionstamp(0)
		kv := m.keyValueForMediaVersion(media.ServerName, media.MediaID, version)
		txn.SetVersionstampedKey(kv.Key, kv.Value)

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

// CompleteMediaUpload atomically publishes an object which has already been
// durably written to a datastore. The candidate uses a unique object path, so
// concurrent uploads cannot overwrite the object selected by this transaction.
func (m *MediaDatabase) CompleteMediaUpload(ctx context.Context, candidate *types.Media, sender id.UserID) error {
	_, err := util.DoWriteTransaction(ctx, m.db, func(txn fdb.Transaction) (*struct{}, error) {
		key := m.keyForMedia(candidate.ServerName, candidate.MediaID)
		b, err := txn.Get(key).Get()
		if err != nil {
			return nil, err
		} else if b == nil {
			return nil, ErrMediaNotFound
		}

		current, err := types.NewMediaFromBytes(b, candidate.ServerName, candidate.MediaID)
		if err != nil {
			return nil, err
		} else if current.Sender != sender {
			return nil, ErrMediaForbidden
		} else if !current.UploadedAt.IsZero() {
			return nil, ErrMediaAlreadyUploaded
		} else if !current.ExpiresAt.IsZero() && !time.Now().Before(current.ExpiresAt) {
			return nil, ErrMediaNotFound
		}

		current.StoreKey = candidate.StoreKey
		current.StorePath = candidate.StorePath
		current.Size = candidate.Size
		current.ContentType = candidate.ContentType
		current.FileName = candidate.FileName
		current.UploadedAt = candidate.UploadedAt
		txn.Set(key, current.ToMsgpack())
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
