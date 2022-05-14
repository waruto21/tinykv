package server

import (
	"context"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"

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

	// avoid locked when get value
	server.Latches.WaitForLatches([][]byte{key})
	defer server.Latches.ReleaseLatches([][]byte{key})

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
	return nil, nil
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvResolveLock(_ context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	return nil, nil
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
