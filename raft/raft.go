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
	"errors"
	"github.com/pingcap-incubator/tinykv/log"
	"math/rand"
	"sort"
	"strconv"

	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as Id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	Id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes    map[uint64]bool
	Agreed   int
	Rejected int

	// msgs need to send
	msgs []pb.Message

	// the leader Id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout     int
	realElectionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is Id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	// Your Code Here (2A).
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	hardState, confState, err := c.Storage.InitialState()
	if err != nil {
		panic(err.Error())
	}
	raft := &Raft{
		Id:               c.ID,
		Term:             hardState.Term,
		Vote:             hardState.Vote,
		RaftLog:          newLog(c.Storage),
		Prs:              make(map[uint64]*Progress),
		State:            StateFollower,
		votes:            make(map[uint64]bool),
		Rejected:         0,
		Lead:             None,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
	}
	if c.Applied > 0 {
		//log.Infof("%d reset applied from %d to %d\n", raft.Id, raft.RaftLog.applied, c.Applied)
		raft.RaftLog.applied = c.Applied
	}
	if hardState.Vote == c.ID {
		raft.Agreed = 1
	}
	if c.peers == nil {
		c.peers = confState.Nodes
	}
	for _, p := range c.peers {
		raft.Prs[p] = &Progress{
			Match: 0,
			Next:  1,
		}
	}
	//raft.Prs[raft.Id] = &Progress{}
	raft.resetElectionTimeout()
	return raft
}

func (r *Raft) GetLeadTransferee() uint64 {
	return r.leadTransferee
}

func (r *Raft) resetElectionTimeout() {
	r.realElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
}

func (r *Raft) resetTick() {
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.resetElectionTimeout()
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	//return false
	lastIndex := r.RaftLog.LastIndex()
	preLogIndex := r.Prs[to].Next - 1

	preLogTerm, err := r.RaftLog.Term(preLogIndex)
	if err != nil {
		if err == ErrCompacted {
			//log.Infof("%d can't find index %d to %d begin snapshot\n", r.Id, preLogIndex, to)
			r.sendSnapshot(to)
			return true
		}
		//log.Infof("%d sendAppend to %d, preIndex %d error %+v\n", r.Id, to, preLogIndex, err)
		return false
	}
	entries := r.RaftLog.Entries(r.Prs[to].Next, lastIndex+1)

	sendEntries := make([]*pb.Entry, 0, len(entries))
	for _, e := range entries {
		sendEntries = append(sendEntries, &pb.Entry{
			EntryType: e.EntryType,
			Term:      e.Term,
			Index:     e.Index,
			Data:      e.Data,
		})
	}
	//if len(sendEntries) > 0 {
	//log.Infof("%d send index [%d %d] to %d\n", r.Id, sendEntries[0].Index, sendEntries[len(sendEntries)-1].Index, to)
	//}

	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		To:      to,
		From:    r.Id,
		Term:    r.Term,
		LogTerm: preLogTerm,
		Index:   preLogIndex,
		Entries: sendEntries,
		Commit:  r.RaftLog.committed,
	}
	r.msgs = append(r.msgs, msg)
	return true
}

func (r *Raft) sendSnapshot(to uint64) {
	snapshot, err := r.RaftLog.storage.Snapshot()
	if err != nil {
		if err == ErrSnapshotTemporarilyUnavailable {
			return
		}
		panic(err)
	}

	r.msgs = append(r.msgs, pb.Message{
		MsgType:  pb.MessageType_MsgSnapshot,
		To:       to,
		From:     r.Id,
		Term:     r.Term,
		Snapshot: &snapshot,
	})
	//log.Infof("%d success send snapshot to %d, index %d\n", r.Id, to, snapshot.Metadata.Index)
	r.Prs[to].Next = snapshot.Metadata.Index + 1
}

func (r *Raft) sendAppendResp(to uint64, reject bool) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		To:      to,
		From:    r.Id,
		Term:    r.Term,
		Reject:  reject,
		Index:   r.RaftLog.LastIndex(),
	}
	r.msgs = append(r.msgs, msg)
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		From:    r.Id,
		To:      to,
		Term:    r.Term,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendHeartbeatResp(to uint64, reject bool) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		From:    r.Id,
		To:      to,
		Term:    r.Term,
		Index:   r.RaftLog.LastIndex(),
		Reject:  reject,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendVoteResp(to uint64, reject bool) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		From:    r.Id,
		To:      to,
		Term:    r.Term,
		Reject:  reject,
	}
	r.msgs = append(r.msgs, msg)
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	// new added node, term is 0
	if r.Term == 0 {
		return
	}
	// new peer when split should campaign immediately
	if r.Term == 5 && r.Id%2 == 0 {
		r.campaign()
	}
	if r.isLeader() {
		r.tickHeartbeatElapsed()
	} else {
		r.tickElectionElapsed()
	}
}

func (r *Raft) isLeader() bool {
	return r.State == StateLeader
}

func (r *Raft) isFollower() bool {
	return r.State == StateFollower
}

func (r *Raft) tickHeartbeatElapsed() {
	r.heartbeatElapsed++
	if r.heartbeatElapsed == r.heartbeatTimeout {
		r.heartbeatElapsed = 0
		r.bCastHeartbeat()
	}
}

func (r *Raft) tickElectionElapsed() {
	r.electionElapsed++
	//log.Debugf("%d electionElapsed: %d, realElectionTimeout: %d\n", r.Id, r.electionElapsed, r.realElectionTimeout)
	if r.electionElapsed >= r.realElectionTimeout {
		r.campaign()
	}
}

// to be candidate
func (r *Raft) campaign() {
	//fmt.Printf("%d campaign\n", r.Id)
	r.becomeCandidate()
	r.bCastVoteReq()
}

func (r *Raft) bCastAppend() {
	for peer := range r.Prs {
		if peer != r.Id {
			r.sendAppend(peer)
		}
	}
}

func (r *Raft) bCastHeartbeat() {
	for peer := range r.Prs {
		if peer != r.Id {
			r.sendHeartbeat(peer)
		}
	}
}

func (r *Raft) bCastVoteReq() {
	//fmt.Printf("%d vote req\n", r.Id)
	lastIndex := r.RaftLog.LastIndex()
	lastTerm := mustTerm(r.RaftLog.Term(lastIndex))

	for peer := range r.Prs {
		if peer != r.Id {
			msg := pb.Message{
				MsgType: pb.MessageType_MsgRequestVote,
				To:      peer,
				From:    r.Id,
				Term:    r.Term,
				LogTerm: lastTerm,
				Index:   lastIndex,
			}
			r.msgs = append(r.msgs, msg)
		}
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.State = StateFollower
	r.Term = term
	r.Lead = lead
	r.votes = nil
	r.Agreed = 0
	r.Rejected = 0
	r.leadTransferee = None
	// for new added peer to add leader and self, prevent split brain
	if _, ok := r.Prs[lead]; !ok && lead != None {
		//log.Infof("%d first become follower, add leader %d\n", r.Id, lead)
		r.Prs[lead] = &Progress{}
	}
	if _, ok := r.Prs[r.Id]; !ok {
		r.Prs[r.Id] = &Progress{}
	}
	r.resetTick()
	//log.Infof("%d become follower, term: %d, lead: %d, lastIndex: %d\n", r.Id, r.Term, r.Lead, r.RaftLog.LastIndex())
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.State = StateCandidate
	r.Term += 1
	r.Vote = r.Id
	// make sure clean previous votes
	r.votes = make(map[uint64]bool)
	r.Agreed = 1
	r.Rejected = 0
	r.votes[r.Id] = true
	r.resetTick()
	//log.Infof("%d become candidate, term: %d, lastIndex: %d, commit index %d, Prs: %+v\n", r.Id, r.Term, r.RaftLog.LastIndex(), r.RaftLog.committed, r.Prs)
	//fmt.Printf("%d become candidate\n", r.Id)
	if r.Agreed > len(r.Prs)/2 {
		r.becomeLeader()
	}
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	//log.Infof("%d become leader, Term: %d, lastIndex: %d, commit index %d, Prs: %+v\n", r.Id, r.Term, r.RaftLog.LastIndex(), r.RaftLog.committed, r.Prs)
	r.State = StateLeader
	r.Lead = r.Id
	for _, v := range r.Prs {
		v.Match = 0
		v.Next = r.RaftLog.LastIndex() + 1
	}
	r.resetTick()
	// immediately append a noop entry
	r.appendEntries(&pb.Entry{
		EntryType: pb.EntryType_EntryNormal,
		Term:      r.Term,
		Index:     r.RaftLog.LastIndex() + 1,
		Data:      nil,
	})
	r.bCastAppend()
	// for only one node
	r.advanceCommitIndex()
}

func (r *Raft) appendEntries(entries ...*pb.Entry) {
	es := make([]pb.Entry, 0, len(entries))
	for _, e := range entries {
		if e.EntryType == pb.EntryType_EntryConfChange {
			if r.PendingConfIndex < e.Index {
				r.PendingConfIndex = e.Index
				//log.Infof("%d begin conf change pending at index %d, is leader %t \n", r.Id, e.Index, r.isLeader())
			}
		}
		es = append(es, pb.Entry{
			EntryType: e.EntryType,
			Term:      e.Term,
			Index:     e.Index,
			Data:      e.Data,
		})
	}
	r.RaftLog.appendEntries(es...)
	//log.Infof("%d append entries, prs %+v, entries: %+v\n", r.Id, r.Prs, entries)
	r.Prs[r.Id].Match = r.RaftLog.LastIndex()
	r.Prs[r.Id].Next = r.RaftLog.LastIndex() + 1
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	// if r have been removed due to conf change
	// or new added node has no Prs but should step
	if _, ok := r.Prs[r.Id]; !ok && len(r.Prs) != 0 {
		log.Infof("%d do not exist and have other peers return, term %d, Prs %+v\n", r.Id, r.Term, r.Prs)
		return nil
	}
	switch r.State {
	case StateFollower:
		r.stepFollower(m)
	case StateCandidate:
		r.stepCandidate(m)
	case StateLeader:
		r.stepLeader(m)
	}
	return nil
}

func (r *Raft) stepFollower(m pb.Message) {
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		if r.RaftLog.pendingSnapshot != nil {
			return
		}
		r.campaign()
	case pb.MessageType_MsgRequestVote:
		r.handleVote(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgTimeoutNow:
		r.campaign()
	case pb.MessageType_MsgTransferLeader:
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgTransferLeader,
			To:      r.Lead,
			From:    m.From,
		})
	}
}

func (r *Raft) stepCandidate(m pb.Message) {
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.campaign()
	case pb.MessageType_MsgRequestVote:
		r.handleVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handleVoteResp(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgTransferLeader:
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgTransferLeader,
			To:      r.Lead,
			From:    m.From,
		})
	}
}

func (r *Raft) stepLeader(m pb.Message) {
	switch m.MsgType {
	case pb.MessageType_MsgPropose:
		// because the test entries' term and index are not set
		lastIndex := r.RaftLog.LastIndex()
		entities := make([]*pb.Entry, 0, len(m.Entries))
		for _, e := range m.Entries {
			entities = append(entities, &pb.Entry{
				EntryType: e.EntryType,
				Term:      r.Term,
				Index:     lastIndex + 1,
				Data:      e.Data,
			})
			lastIndex += 1
		}
		r.appendEntries(entities...)
		r.bCastAppend()
		// if only on node in raft cluster
		r.advanceCommitIndex()
	case pb.MessageType_MsgRequestVote:
		r.handleVote(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgBeat:
		r.bCastHeartbeat()
	case pb.MessageType_MsgHeartbeatResponse:
		r.handleHeartbeatResp(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendResp(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgTransferLeader:
		r.handleLeaderTransfer(m)
	}
}

func (r *Raft) advanceCommitIndex() {
	// If there exists an N such that N > commitIndex, a majority
	// of matchIndex[i] ≥ N, and log[N].term
	// current Term:
	// set commitIndex = N ($5.3, $5.4).
	matchIndex := make([]uint64, 0, len(r.Prs))
	for _, v := range r.Prs {
		matchIndex = append(matchIndex, v.Match)
	}

	// reverse sort, for even num of node
	sort.Sort(sort.Reverse(uint64Slice(matchIndex)))
	// r only commit own log
	N := matchIndex[len(matchIndex)/2]
	advanced := false
	if N > r.RaftLog.committed {
		//fmt.Println("N is ", N, " last is ", r.RaftLog.LastIndex())
		term := mustTerm(r.RaftLog.Term(N))
		if term == r.Term {
			//log.Infof("%d leader commit to %d\n", r.Id, N)
			r.RaftLog.committed = N
			advanced = true
		}
	}
	if advanced {
		r.bCastAppend()
	}
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if m.Term < r.Term {
		r.sendAppendResp(m.From, true)
		return
	}
	r.becomeFollower(m.Term, m.From)

	// check prevLogIndex and prevLogTerm
	term, err := r.RaftLog.Term(m.Index)
	if err != nil || term != m.LogTerm {
		//log.Infof("%d reject from %d, index %d, log term %d, term %d\n", r.Id, m.From, m.Index, m.LogTerm, term)
		r.sendAppendResp(m.From, true)
		return
	}

	if len(m.Entries) > 0 {
		//r.logLogs("receive logs", m.Entries)
		//log.Infof("%d receive entry %d %v\n", r.Id, m.Entries[0].Index, m.Entries[0].EntryType)
		lastIndex := r.RaftLog.LastIndex()
		// get where the log last matched
		matched := -1
		for i, e := range m.Entries {
			// all the entries that in r.RaftLog matches
			if e.Index > lastIndex {
				break
			}
			term, _ := r.RaftLog.Term(e.Index)
			if term != e.Term {
				// from e.Index, the log do not match
				r.RaftLog.removeEntriesFrom(e.Index)
				break
			}
			matched = i
		}
		// not all matched
		if matched < len(m.Entries)-1 {
			//r.logEntries()
			//r.logLogs("new append", m.Entries[matched+1:])
			r.appendEntries(m.Entries[matched+1:]...)
			//r.logEntries()
		}
	}

	// If leaderCommit > commitIndex, set commitIndex
	// min(leaderCommit, index of last new entry)
	lastNewEntryIndex := m.Index + uint64(len(m.Entries))
	if m.Commit > r.RaftLog.committed {
		r.RaftLog.committed = min(m.Commit, lastNewEntryIndex)
		//log.Infof("%d follower commit to %d\n", r.Id, r.RaftLog.committed)
	}

	r.sendAppendResp(m.From, false)

}

func (r *Raft) logLogs(prefix string, entries []*pb.Entry) {
	s := ""
	for _, e := range entries {
		s += " " + strconv.FormatUint(e.Index, 10)
	}
	log.Infof("%d, %s logs [%s]\n", r.Id, prefix, s)
}

func (r *Raft) logEntries() {
	s := ""
	for _, e := range r.RaftLog.entries {
		s += " " + strconv.FormatUint(e.Index, 10)
	}
	log.Infof("%d, entries [%s]\n", r.Id, s)
}

func (r *Raft) handleAppendResp(m pb.Message) {
	if m.Term > r.Term {
		r.becomeFollower(m.Term, m.From)
		return
	}
	if !m.Reject {
		//fmt.Println("append success, index is ", m.Index)
		r.Prs[m.From].Match = m.Index
		r.Prs[m.From].Next = m.Index + 1

		//log.Infof("%d append to %d success, next %d", r.Id, m.From, m.Index+1)
	} else {
		if m.Index < r.RaftLog.FirstIndex() {
			r.sendSnapshot(m.From)
			return
		}
		if r.Prs[m.From].Next > 1 {
			//log.Infof("%d decrease %d next %d\n", r.Id, m.From, r.Prs[m.From].Next)
			r.Prs[m.From].Next -= 1
		}
		//log.Infof("%d decrease next for %d to %d, first index %d, m.index %d\n", r.Id, m.From, r.Prs[m.From].Next, r.RaftLog.firstIndex, m.Index)
		r.sendAppend(m.From)
		return
	}

	r.advanceCommitIndex()
	if m.From == r.leadTransferee && m.Index == r.RaftLog.LastIndex() {
		r.sendTimeoutNow(m.From)
	}
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if m.Term < r.Term {
		r.sendHeartbeatResp(m.From, true)
		return
	}
	// only leader can send heartbeat
	// if network partition, the old leader's term will less than r.Term, which would be rejected before this
	// so, it's safe to becomeFollower
	//log.Infof("%d handle heartbeat from %d\n", r.Id, m.From)
	r.becomeFollower(m.Term, m.From)
	r.sendHeartbeatResp(m.From, false)

}

func (r *Raft) handleHeartbeatResp(m pb.Message) {
	// r is the old leader, send heartbeat to m.From
	// but m.From is the new Leader
	if m.Term > r.Term {
		r.becomeFollower(m.Term, m.From)
		return
	}
	// because we don't have append timeout
	// so append here
	r.bCastAppend()
}

func (r *Raft) handleVoteResp(m pb.Message) {
	//fmt.Printf("%d handle voteResp: %+v\n", r.Id, m)
	if m.Term > r.Term {
		r.becomeFollower(m.Term, m.From)
		r.Vote = m.From
		return
	}
	if !m.Reject {
		//log.Infof("%d receive agree from %d\n", r.Id, m.From)
		r.votes[m.From] = true
		r.Agreed += 1
	} else {
		r.votes[m.From] = false
		r.Rejected += 1
	}
	if r.Agreed > len(r.Prs)/2 {
		r.becomeLeader()
	} else if r.Rejected > len(r.Prs)/2 {
		r.becomeFollower(r.Term, None)
	}
}

func (r *Raft) handleVote(m pb.Message) {
	//fmt.Printf("%d handle vote %+v\n", r.Id, m)
	// req term is lower
	if r.Term > m.Term {
		r.sendVoteResp(m.From, true)
		//fmt.Println("here1")
		return
	}
	// the req node does not have newer log
	if !r.hasNewerLogs(m.LogTerm, m.Index) {
		if r.Term < m.Term {
			r.becomeFollower(m.Term, None)
		}
		r.sendVoteResp(m.From, true)
		//fmt.Println("here2")
		return
	}
	if r.Term < m.Term {
		r.becomeFollower(m.Term, None)
		r.Vote = m.From
		r.sendVoteResp(m.From, false)
		//fmt.Println("here3")
		return
	}
	// Repeat requests
	if r.Vote == m.From {
		r.sendVoteResp(m.From, false)
		//fmt.Println("here4")
		return
	}

	if r.isFollower() && r.Vote == None {
		r.Vote = m.From
		r.sendVoteResp(m.From, false)
		//fmt.Println("here5")
		return
	}
	//fmt.Println("here6")
	r.sendVoteResp(m.From, true)
}

func (r *Raft) hasNewerLogs(logTerm, index uint64) bool {
	lastIndex := r.RaftLog.LastIndex()
	lastLogTerm := mustTerm(r.RaftLog.Term(lastIndex))
	if logTerm > lastLogTerm || (logTerm == lastLogTerm && index >= lastIndex) {
		return true
	}
	return false
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
	//log.Infof("%d handle snapshot %+v\n", r.Id, m.Snapshot.Metadata)
	meta := m.Snapshot.Metadata
	if meta.Index <= r.RaftLog.committed {
		r.sendAppendResp(m.From, false)
		return
	}
	r.becomeFollower(max(r.Term, meta.Term), m.From)

	// clear log
	r.RaftLog.entries = make([]pb.Entry, 0)
	// install snapshot
	r.RaftLog.applied = meta.Index
	r.RaftLog.committed = meta.Index
	r.RaftLog.stabled = meta.Index
	//r.RaftLog.firstIndex = meta.Index + 1
	r.RaftLog.pendingSnapshot = m.Snapshot

	// update conf
	r.Prs = make(map[uint64]*Progress)
	for _, p := range meta.ConfState.Nodes {
		r.Prs[p] = &Progress{}
	}
	r.sendAppendResp(m.From, false)
}

func (r *Raft) handleLeaderTransfer(m pb.Message) {
	// m.From is transfer target
	if m.From == r.Id {
		return
	}
	if r.leadTransferee == m.From {
		return
	}
	if _, ok := r.Prs[m.From]; !ok {
		return
	}

	r.leadTransferee = m.From
	if r.Prs[m.From].Match != r.RaftLog.LastIndex() {
		r.sendAppend(m.From)
	} else {
		r.sendTimeoutNow(m.From)
	}
}

func (r *Raft) sendTimeoutNow(to uint64) {
	r.msgs = append(r.msgs, pb.Message{
		MsgType: pb.MessageType_MsgTimeoutNow,
		To:      to,
		From:    r.Id,
	})
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
	if _, ok := r.Prs[id]; !ok {
		r.Prs[id] = &Progress{
			Match: 0,
			// new node has no entries
			Next: 1,
		}
	}
	r.PendingConfIndex = None
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
	if _, ok := r.Prs[id]; ok {
		delete(r.Prs, id)
		// because remove a node maybe decrease quorum
		// some log entry maybe commit
		if r.isLeader() {
			r.advanceCommitIndex()
		}
	}
	r.PendingConfIndex = None
}
