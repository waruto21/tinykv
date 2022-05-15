package server

import (
	"context"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"

	"github.com/pingcap-incubator/tinykv/kv/coprocessor"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/storage/raft_storage"
	"github.com/pingcap-incubator/tinykv/kv/transaction/latches"
	coppb "github.com/pingcap-incubator/tinykv/proto/pkg/coprocessor"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/tinykvpb"
	"github.com/pingcap/tidb/kv"
)

var _ tinykvpb.TinyKvServer = new(Server)

// Server is a TinyKV server, it 'faces outwards', sending and receiving messages from clients such as TinySQL.
type Server struct {
	storage storage.Storage

	// (Used in 4A/4B)
	// latches for single row (key) atomicity, bigtable in percolator provide this
	// prewrite write CFDefault and CFLock
	// commit write CFLock and CFWrite
	Latches *latches.Latches

	// coprocessor API handler, out of course scope
	copHandler *coprocessor.CopHandler
}

func NewServer(storage storage.Storage) *Server {
	return &Server{
		storage: storage,
		Latches: latches.NewLatches(),
	}
}

// The below functions are Server's gRPC API (implements TinyKvServer).

// Raft commands (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Raft(stream tinykvpb.TinyKv_RaftServer) error {
	return server.storage.(*raft_storage.RaftStorage).Raft(stream)
}

// Snapshot stream (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Snapshot(stream tinykvpb.TinyKv_SnapshotServer) error {
	return server.storage.(*raft_storage.RaftStorage).Snapshot(stream)
}

// Transactional API.
func (server *Server) KvGet(_ context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
	// Your Code Here (4B).

	//return nil, nil
	resp := &kvrpcpb.GetResponse{}
	key := req.Key
	ts := req.Version
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, ts)
	// is locked
	lock, err := txn.GetLock(key)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	if lock != nil && ts >= lock.Ts {
		resp.Error = &kvrpcpb.KeyError{
			Locked: &kvrpcpb.LockInfo{
				PrimaryLock: lock.Primary,
				LockVersion: lock.Ts,
				Key:         key,
				LockTtl:     lock.Ttl,
			},
		}
	}
	value, err := txn.GetValue(key)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	if value == nil {
		resp.NotFound = true
	}
	resp.Value = value
	return resp, nil
}

func (server *Server) KvPrewrite(_ context.Context, req *kvrpcpb.PrewriteRequest) (*kvrpcpb.PrewriteResponse, error) {
	// Your Code Here (4B).
	//return nil, nil
	resp := &kvrpcpb.PrewriteResponse{}
	startTs := req.StartVersion
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	defer reader.Close()
	mutationKeys := make([][]byte, 0, len(req.Mutations))
	for _, mutation := range req.Mutations {
		mutationKeys = append(mutationKeys, mutation.Key)
	}
	server.Latches.WaitForLatches(mutationKeys)
	defer server.Latches.ReleaseLatches(mutationKeys)

	txn := mvcc.NewMvccTxn(reader, startTs)
	keyErrors := make([]*kvrpcpb.KeyError, 0)
	// check is w-w conflict and not locked
	for _, key := range mutationKeys {
		write, ts, err := txn.MostRecentWrite(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		if write != nil && ts > startTs {
			keyErrors = append(keyErrors, &kvrpcpb.KeyError{
				Conflict: &kvrpcpb.WriteConflict{
					StartTs:    startTs,
					ConflictTs: ts,
					Key:        key,
					Primary:    req.PrimaryLock,
				},
			})
		}
	}
	if len(keyErrors) > 0 {
		resp.Errors = keyErrors
		return resp, nil
	}

	for _, key := range mutationKeys {
		lock, err := txn.GetLock(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		if lock != nil {
			keyErrors = append(keyErrors, &kvrpcpb.KeyError{
				Locked: &kvrpcpb.LockInfo{
					PrimaryLock: lock.Primary,
					LockVersion: lock.Ts,
					Key:         key,
					LockTtl:     lock.Ttl,
				}})
		}
	}
	if len(keyErrors) > 0 {
		resp.Errors = keyErrors
		return resp, nil
	}
	// now safe to write
	for _, mutation := range req.Mutations {
		switch mutation.Op {
		case kvrpcpb.Op_Put:
			txn.PutValue(mutation.Key, mutation.Value)
			txn.PutLock(mutation.Key, &mvcc.Lock{
				Primary: req.PrimaryLock,
				Ts:      startTs,
				Ttl:     req.LockTtl,
				Kind:    mvcc.WriteKindPut,
			})
		case kvrpcpb.Op_Del:
			txn.DeleteValue(mutation.Key)
			txn.PutLock(mutation.Key, &mvcc.Lock{
				Primary: req.PrimaryLock,
				Ts:      startTs,
				Ttl:     req.LockTtl,
				Kind:    mvcc.WriteKindDelete,
			})
		}
	}
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	return resp, nil
}

func (server *Server) KvCommit(_ context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	// Your Code Here (4B).
	//return nil, nil
	resp := &kvrpcpb.CommitResponse{}
	startTs := req.StartVersion
	commitTs := req.CommitVersion
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, startTs)
	keys := req.Keys
	server.Latches.WaitForLatches(keys)
	defer server.Latches.ReleaseLatches(keys)
	for _, key := range keys {
		lock, err := txn.GetLock(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		if lock == nil {
			write, _, err := txn.CurrentWrite(key)
			if err != nil {
				if regionErr, ok := err.(*raft_storage.RegionError); ok {
					resp.RegionError = regionErr.RequestErr
					return resp, nil
				}
				return nil, err
			}
			// if already rollback
			if write != nil && write.Kind == mvcc.WriteKindRollback {
				resp.Error = &kvrpcpb.KeyError{
					Abort: "true",
				}
			}
			return resp, nil
		}
		if lock.Ts != startTs {
			resp.Error = &kvrpcpb.KeyError{
				Retryable: "true",
			}
			return resp, nil
		}
		txn.PutWrite(key, commitTs, &mvcc.Write{
			StartTS: startTs,
			Kind:    lock.Kind,
		})
		txn.DeleteLock(key)
	}
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	return resp, nil
}

func (server *Server) KvScan(_ context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	// Your Code Here (4C).
	//return nil, nil
	resp := &kvrpcpb.ScanResponse{}
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	defer reader.Close()
	startTs := req.Version
	txn := mvcc.NewMvccTxn(reader, startTs)
	scanner := mvcc.NewScanner(req.StartKey, txn)
	defer scanner.Close()

	kvs := make([]*kvrpcpb.KvPair, 0)
	for i := uint32(0); i < req.Limit; i++ {
		k, v, err := scanner.Next()
		// no k/v remained
		if k == nil && v == nil && err == nil {
			break
		}
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		lock, err := txn.GetLock(k)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		if lock != nil && startTs > lock.Ts {
			kvs = append(kvs, &kvrpcpb.KvPair{
				Error: &kvrpcpb.KeyError{
					Locked: &kvrpcpb.LockInfo{
						PrimaryLock: lock.Primary,
						LockVersion: lock.Ts,
						Key:         k,
						LockTtl:     lock.Ttl,
					},
				},
			})
		} else if v != nil {
			kvs = append(kvs, &kvrpcpb.KvPair{
				Key:   k,
				Value: v,
			})
		}
	}
	resp.Pairs = kvs
	return resp, nil
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	//return nil, nil
	resp := &kvrpcpb.CheckTxnStatusResponse{}
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	defer reader.Close()
	pk := req.PrimaryKey
	server.Latches.WaitForLatches([][]byte{pk})
	defer server.Latches.ReleaseLatches([][]byte{pk})

	txn := mvcc.NewMvccTxn(reader, req.LockTs)

	write, ts, err := txn.CurrentWrite(pk)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}

	if write != nil {
		if write.Kind != mvcc.WriteKindRollback {
			// commit
			resp.CommitVersion = ts
			return resp, nil
		}
		// rollback
		return resp, nil
	}

	lock, err := txn.GetLock(pk)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	// no lock, rollback
	if lock == nil {
		txn.Rollback(pk, false)
		resp.Action = kvrpcpb.Action_LockNotExistRollback
		resp.CommitVersion = 0
		resp.LockTtl = 0
		if err := server.storage.Write(req.Context, txn.Writes()); err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		return resp, nil
	} else {
		// lock expired
		if mvcc.PhysicalTime(lock.Ts)+lock.Ttl < mvcc.PhysicalTime(req.CurrentTs) {
			txn.Rollback(pk, true)
			resp.Action = kvrpcpb.Action_TTLExpireRollback
			if err := server.storage.Write(req.Context, txn.Writes()); err != nil {
				if regionErr, ok := err.(*raft_storage.RegionError); ok {
					resp.RegionError = regionErr.RequestErr
					return resp, nil
				}
				return nil, err
			}
			return resp, nil
		}
	}

	resp.LockTtl = lock.Ttl
	return resp, nil
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	//return nil, nil
	resp := &kvrpcpb.BatchRollbackResponse{}
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	defer reader.Close()
	keys := req.Keys
	server.Latches.WaitForLatches(keys)
	defer server.Latches.ReleaseLatches(keys)
	startTs := req.StartVersion
	txn := mvcc.NewMvccTxn(reader, startTs)
	for _, key := range keys {
		write, _, err := txn.CurrentWrite(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		// already rollback
		if write != nil {
			// already rollback
			if write.Kind != mvcc.WriteKindRollback {
				resp.Error = &kvrpcpb.KeyError{
					Abort: "true",
				}
				return resp, nil
			}
			continue
		}
		// check lock
		lock, err := txn.GetLock(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		if lock != nil && lock.Ts != txn.StartTS {
			// locked by another txn
			txn.Rollback(key, false)
			continue
		}
		txn.Rollback(key, lock != nil)
	}
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	return resp, nil
}

func (server *Server) KvResolveLock(ctx context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	//return nil, nil
	resp := &kvrpcpb.ResolveLockResponse{}
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	defer reader.Close()
	iter := reader.IterCF(engine_util.CfLock)
	defer iter.Close()
	secondaryKeys := make([][]byte, 0)
	for ; iter.Valid(); iter.Next() {
		item := iter.Item()
		value, err := item.Value()
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		lock, err := mvcc.ParseLock(value)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				resp.RegionError = regionErr.RequestErr
				return resp, nil
			}
			return nil, err
		}
		if lock.Ts == req.StartVersion {
			secondaryKeys = append(secondaryKeys, item.Key())
		}
	}
	if len(secondaryKeys) == 0 {
		return resp, nil
	}
	if req.CommitVersion > 0 {
		r, err := server.KvCommit(ctx, &kvrpcpb.CommitRequest{
			Context:       req.Context,
			StartVersion:  req.StartVersion,
			Keys:          secondaryKeys,
			CommitVersion: req.CommitVersion,
		})
		resp.Error = r.Error
		resp.RegionError = r.RegionError
		return resp, err
	} else if req.CommitVersion == 0 {
		r, err := server.KvBatchRollback(ctx, &kvrpcpb.BatchRollbackRequest{
			Context:      req.Context,
			StartVersion: req.StartVersion,
			Keys:         secondaryKeys,
		})
		resp.Error = r.Error
		resp.RegionError = r.RegionError
		return resp, err
	}
	return resp, nil
}

// SQL push down commands.
func (server *Server) Coprocessor(_ context.Context, req *coppb.Request) (*coppb.Response, error) {
	resp := new(coppb.Response)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	switch req.Tp {
	case kv.ReqTypeDAG:
		return server.copHandler.HandleCopDAGRequest(reader, req), nil
	case kv.ReqTypeAnalyze:
		return server.copHandler.HandleCopAnalyzeRequest(reader, req), nil
	}
	return nil, nil
}
