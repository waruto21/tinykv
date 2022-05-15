// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// RaftLog manage the log entries, its struct look like:
//
//  snapshot/first.....applied....committed....stabled.....last
//  --------|------------------------------------------------|
//                            log entries
//
// for simplify the RaftLog implement should manage all log entries
// that not truncated
type RaftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64

	// applied is the highest log position that the application has
	// been instructed to apply to its state machine.
	// Invariant: applied <= committed
	applied uint64

	// log entries with index <= stabled are persisted to storage.
	// It is used to record the logs that are not persisted by storage yet.
	// Everytime handling `Ready`, the unstabled logs will be included.
	stabled uint64

	// all entries that have not yet compact.
	entries []pb.Entry

	// the incoming unstable snapshot, if any.
	// (Used in 2C)
	pendingSnapshot *pb.Snapshot

	// Your Data Here (2A).
	//firstIndex uint64
}

// newLog returns log using the given storage. It recovers the log
// to the state that it just commits and applies the latest snapshot.
func newLog(storage Storage) *RaftLog {
	// Your Code Here (2A).
	//return nil
	hardState, _, _ := storage.InitialState()
	firstIndex, _ := storage.FirstIndex()
	lastIndex, _ := storage.LastIndex()
	entries, _ := storage.Entries(firstIndex, lastIndex+1)
	return &RaftLog{
		storage:   storage,
		committed: hardState.Commit,
		applied:   firstIndex - 1,
		stabled:   lastIndex,
		entries:   entries,
		//firstIndex:      firstIndex,
		pendingSnapshot: nil,
	}
}

// We need to compact the log entries in some point of time like
// storage compact stabled log entries prevent the log entries
// grow unlimitedly in memory
// delete compacted entries in l.entries
func (l *RaftLog) maybeCompact() {
	// Your Code Here (2C).
	if len(l.entries) == 0 {
		return
	}
	truncatedIndex, _ := l.storage.FirstIndex()
	firstIndex := l.FirstIndex()
	if truncatedIndex > firstIndex {
		l.entries = l.entries[truncatedIndex-firstIndex:]
	}
}

// unstableEntries return all the unstable entries
func (l *RaftLog) unstableEntries() []pb.Entry {
	// Your Code Here (2A).
	//return nil
	if len(l.entries) > 0 {
		if (l.stabled-l.FirstIndex()+1 < 0) || (l.stabled-l.FirstIndex()+1 > uint64(len(l.entries))) {
			return nil
		}
		return l.entries[l.stabled-l.FirstIndex()+1:]
	}
	return nil
}

// nextEnts returns all the committed but not applied entries
func (l *RaftLog) nextEnts() (ents []pb.Entry) {
	// Your Code Here (2A).
	if len(l.entries) > 0 {
		if l.committed-l.FirstIndex()+1 < 0 || l.applied-l.FirstIndex()+1 > l.LastIndex() {
			return nil
		}
		if l.applied-l.FirstIndex()+1 >= 0 && l.committed-l.FirstIndex()+1 <= uint64(len(l.entries)) {
			return l.entries[l.applied-l.FirstIndex()+1 : l.committed-l.FirstIndex()+1]
		}
	}
	return nil
}

// LastIndex return the last index of the log entries
func (l *RaftLog) LastIndex() uint64 {
	// Your Code Here (2A).
	if len(l.entries) == 0 {
		return l.stabled
	}
	return l.entries[len(l.entries)-1].Index
}

func (l *RaftLog) FirstIndex() uint64 {
	// Your Code Here (2A).
	if len(l.entries) == 0 {
		i, _ := l.storage.FirstIndex()
		return i - 1
	}
	return l.entries[0].Index
}

// Term return the term of the entry in the given index
func (l *RaftLog) Term(i uint64) (uint64, error) {
	// Your Code Here (2A).
	if len(l.entries) > 0 {
		offset := l.FirstIndex()
		if i >= offset {
			index := i - offset
			if index >= uint64(len(l.entries)) {
				return 0, ErrUnavailable
			}
			return l.entries[index].Term, nil
		}
	}
	term, err := l.storage.Term(i)
	if err == ErrUnavailable && !IsEmptySnap(l.pendingSnapshot) {
		if i == l.pendingSnapshot.Metadata.Index {
			return l.pendingSnapshot.Metadata.Term, nil
		}
		if i < l.pendingSnapshot.Metadata.Index {
			err = ErrCompacted
		}
	}
	return term, err
}

func (l *RaftLog) appendEntries(entries ...pb.Entry) {
	l.entries = append(l.entries, entries...)
}

// Entries returns entries in [lo, hi)
func (l *RaftLog) Entries(lo, hi uint64) []pb.Entry {
	if lo >= l.FirstIndex() && hi-l.FirstIndex() <= uint64(len(l.entries)) {
		return l.entries[lo-l.FirstIndex() : hi-l.FirstIndex()]
	}
	ents, _ := l.storage.Entries(lo, hi)
	return ents
}

func (l *RaftLog) removeEntriesFrom(index uint64) {
	// maybe remove stabled entries
	l.stabled = min(index-1, l.stabled)
	if index-l.FirstIndex() >= uint64(len(l.entries)) {
		return
	}
	l.entries = l.entries[:index-l.FirstIndex()]
}
