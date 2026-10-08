package remotedevices

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func decodeJob(userID id.UserID, b []byte) *types.RemoteDeviceJob {
	if b == nil {
		return nil
	}
	value, err := tuple.Unpack(b)
	if err != nil {
		panic(err)
	}
	return &types.RemoteDeviceJob{
		UserID:   userID,
		DueAt:    time.UnixMilli(value[0].(int64)).UTC(),
		Attempts: int(value[1].(int64)),
	}
}

func (r *RemoteDevicesDirectory) txnStoreJob(txn fdb.Transaction, job types.RemoteDeviceJob) {
	txn.Set(r.keyForJob(job.UserID), tuple.Tuple{job.DueAt.UnixMilli(), int64(job.Attempts)}.Pack())
	txn.Set(r.keyForJobDue(job), nil)
}

func (r *RemoteDevicesDirectory) txnCancelJob(txn fdb.Transaction, job *types.RemoteDeviceJob) {
	if job == nil {
		return
	}
	txn.Clear(r.keyForJob(job.UserID))
	txn.Clear(r.keyForJobDue(*job))
}

// A fetch only ever runs while the user has no snapshot: creating the cache, a wipe and an EDU
// without a snapshot are the only callers, and each leaves StreamID nil, so deltas applied to a
// snapshot never need to move the generation. An existing job keeps its due time and attempts, so
// a fetch that keeps failing keeps backing off.
func (r *RemoteDevicesDirectory) txnEnsureJob(txn fdb.Transaction, userID id.UserID, job *types.RemoteDeviceJob, now time.Time) {
	if job != nil {
		return
	}
	r.txnStoreJob(txn, types.RemoteDeviceJob{UserID: userID, DueAt: now})
}

// TxnNextJobs returns the due jobs in due order, each with the generation its snapshot must be
// published against
func (r *RemoteDevicesDirectory) TxnNextJobs(txn fdb.ReadTransaction, now time.Time, limit int) ([]types.RemoteDeviceJob, error) {
	begin, _ := r.jobsByDue.FDBRangeKeys()
	end := fdb.Key(append(r.jobsByDue.Pack(tuple.Tuple{now.UnixMilli()}), 0xff))
	kvs, err := txn.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{
		Limit: limit,
		Mode:  fdb.StreamingModeWantAll,
	}).GetSliceWithError()
	if err != nil {
		return nil, err
	}

	userIDs := make([]id.UserID, 0, len(kvs))
	jobFutures := make([]fdb.FutureByteSlice, 0, len(kvs))
	metaFutures := make([]fdb.FutureByteSlice, 0, len(kvs))
	for _, kv := range kvs {
		key, err := r.jobsByDue.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		userID := id.UserID(key[1].(string))
		userIDs = append(userIDs, userID)
		jobFutures = append(jobFutures, txn.Get(r.keyForJob(userID)))
		metaFutures = append(metaFutures, txn.Get(r.keyForMeta(userID)))
	}

	jobs := make([]types.RemoteDeviceJob, 0, len(kvs))
	for i, userID := range userIDs {
		job := decodeJob(userID, jobFutures[i].MustGet())
		job.Generation = decodeMeta(metaFutures[i].MustGet()).Generation
		jobs = append(jobs, *job)
	}
	return jobs, nil
}

// TxnRescheduleJob retries a failed fetch at dueAt, returning false when the cache was evicted
func (r *RemoteDevicesDirectory) TxnRescheduleJob(txn fdb.Transaction, userID id.UserID, dueAt time.Time) (bool, error) {
	if err := r.checkRemote(userID); err != nil {
		return false, err
	}
	job := decodeJob(userID, txn.Get(r.keyForJob(userID)).MustGet())
	if job == nil {
		return false, nil
	}
	txn.Clear(r.keyForJobDue(*job))
	job.DueAt = dueAt
	job.Attempts++
	r.txnStoreJob(txn, *job)
	return true, nil
}
