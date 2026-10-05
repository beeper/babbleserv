package users

import (
	"fmt"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (u *UsersDirectory) remoteProfileJobKey(userID id.UserID) fdb.Key {
	return u.remoteProfileJobs.Pack(tuple.Tuple{userID.String()})
}

func (u *UsersDirectory) remoteProfileDueKey(job types.RemoteUserDirectoryProfileJob) fdb.Key {
	return u.remoteProfileJobsByDue.Pack(tuple.Tuple{
		job.DueAt.UnixMilli(), job.UserID.String(), job.SourceEventID.String(),
	})
}

func (u *UsersDirectory) txnGetRemoteProfileJob(txn fdb.ReadTransaction, userID id.UserID) *types.RemoteUserDirectoryProfileJob {
	b := txn.Get(u.remoteProfileJobKey(userID)).MustGet()
	if b == nil {
		return nil
	}
	value, err := tuple.Unpack(b)
	if err != nil {
		panic(err)
	}
	return &types.RemoteUserDirectoryProfileJob{
		UserID: userID, SourceEventID: id.EventID(value[0].(string)),
		DueAt: time.UnixMilli(value[1].(int64)).UTC(), Attempts: int(value[2].(int64)),
	}
}

func (u *UsersDirectory) txnStoreRemoteProfileJob(txn fdb.Transaction, job types.RemoteUserDirectoryProfileJob) {
	txn.Set(u.remoteProfileJobKey(job.UserID), tuple.Tuple{
		job.SourceEventID.String(), job.DueAt.UnixMilli(), int64(job.Attempts),
	}.Pack())
	txn.Set(u.remoteProfileDueKey(job), nil)
}

func (u *UsersDirectory) TxnEnsureRemoteDirectoryUsers(
	txn fdb.Transaction,
	sources []types.RemoteUserDirectorySource,
	now, lookupAt time.Time,
) error {
	for i, source := range sources {
		if source.UserID.Homeserver() == u.serverName || source.SourceEventID == "" {
			return fmt.Errorf("invalid remote directory source for %s", source.UserID)
		}
		if _, _, err := source.UserID.ParseAndValidateRelaxed(); err != nil {
			return err
		}
		current := u.txnGetRemoteProfileJob(txn, source.UserID)
		user, err := u.txnGetUser(txn, source.UserID)
		if err != nil {
			return err
		}
		if user == nil {
			user = &types.User{
				Username: source.UserID.Localpart(), ServerName: source.UserID.Homeserver(),
				CreatedAt: now.UTC(),
			}
			txn.Set(u.keyForUser(source.UserID), user.ToMsgpack())
			txn.SetVersionstampedKey(u.keyForUserVersion(tuple.IncompleteVersionstamp(uint16(i))), []byte(source.UserID))
		}
		// A remote account may already exist from device/key discovery without
		// having appeared in any accepted room membership.
		for gram := range searchGrams(source.UserID.String()) {
			txn.Set(u.searchGrams.Pack(tuple.Tuple{gram, source.UserID.String()}), nil)
		}
		if source.Profile != nil {
			if current != nil {
				txn.Clear(u.remoteProfileDueKey(*current))
				txn.Clear(u.remoteProfileJobKey(source.UserID))
			}
			profile := boundRemoteProfile(source.Profile)
			if err := u.TxnStoreUserProfile(txn, source.UserID, &profile); err != nil {
				return err
			}
			continue
		}
		if current != nil {
			if current.SourceEventID == source.SourceEventID {
				continue
			}
			txn.Clear(u.remoteProfileDueKey(*current))
		}
		u.txnStoreRemoteProfileJob(txn, types.RemoteUserDirectoryProfileJob{
			UserID: source.UserID, SourceEventID: source.SourceEventID, DueAt: lookupAt.UTC(),
		})
	}
	return nil
}

// Call TxnEnsureRemoteDirectoryUsers for users with neither a profile fetch queued nor a profile stored
func (u *UsersDirectory) TxnEnsureUnindexedRemoteDirectoryUsers(
	txn fdb.Transaction,
	sources []types.RemoteUserDirectorySource,
	now, lookupAt time.Time,
) error {
	jobs := make([]fdb.FutureByteSlice, len(sources))
	profiles := make([]fdb.FutureByteSlice, len(sources))
	for i, source := range sources {
		jobs[i] = txn.Get(u.remoteProfileJobKey(source.UserID))
		profiles[i] = txn.Get(u.keyForProfile(source.UserID))
	}
	unindexed := make([]types.RemoteUserDirectorySource, 0, len(sources))
	for i, source := range sources {
		if jobs[i].MustGet() == nil && profiles[i].MustGet() == nil {
			unindexed = append(unindexed, source)
		}
	}
	return u.TxnEnsureRemoteDirectoryUsers(txn, unindexed, now, lookupAt)
}

func (u *UsersDirectory) TxnNextRemoteDirectoryProfileJob(
	txn fdb.ReadTransaction,
	now time.Time,
) (*types.RemoteUserDirectoryProfileJob, error) {
	begin, _ := u.remoteProfileJobsByDue.FDBRangeKeys()
	end := fdb.Key(append(u.remoteProfileJobsByDue.Pack(tuple.Tuple{now.UnixMilli()}), 0xff))
	rows := txn.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{
		Limit: 1, Mode: fdb.StreamingModeExact,
	}).GetSliceOrPanic()
	if len(rows) == 0 {
		return nil, nil
	}
	key, err := u.remoteProfileJobsByDue.Unpack(rows[0].Key)
	if err != nil {
		return nil, err
	}
	return u.txnGetRemoteProfileJob(txn, id.UserID(key[1].(string))), nil
}

// The source and due time distinguish pending work from a superseding event or retry.
// A nil profile and retry time abandons the job without touching the stored profile.
func (u *UsersDirectory) TxnFinishRemoteDirectoryProfileJob(
	txn fdb.Transaction,
	job types.RemoteUserDirectoryProfileJob,
	profile *types.UserProfile,
	retryAt *time.Time,
) (bool, error) {
	current := u.txnGetRemoteProfileJob(txn, job.UserID)
	if current == nil || current.SourceEventID != job.SourceEventID || current.DueAt.UnixMilli() != job.DueAt.UnixMilli() {
		return false, nil
	}
	txn.Clear(u.remoteProfileDueKey(*current))
	if retryAt != nil {
		current.DueAt = retryAt.UTC()
		current.Attempts++
		u.txnStoreRemoteProfileJob(txn, *current)
		return true, nil
	}
	txn.Clear(u.remoteProfileJobKey(job.UserID))
	if profile != nil {
		bounded := boundRemoteProfile(profile)
		if err := u.TxnStoreUserProfile(txn, job.UserID, &bounded); err != nil {
			return false, err
		}
	}
	return true, nil
}

func boundRemoteProfile(profile *types.UserProfile) types.UserProfile {
	bounded := types.UserProfile{}
	if profile != nil {
		bounded.DisplayName, bounded.AvatarURL = profile.DisplayName, profile.AvatarURL
	}
	if validateSearchDisplayName(bounded.DisplayName) != nil {
		bounded.DisplayName = ""
	}
	if len(bounded.AvatarURL) > 4096 {
		bounded.AvatarURL = ""
	}
	return bounded
}
