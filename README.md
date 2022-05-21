

TinyKV是教学项目，算是PingCAP TiKV的go语言简化版，实现了一个带有调度器的基于multi-raft的分布式K/V存储。

项目源地址：https://github.com/tidb-incubator/tinykv

我的实现：https://github.com/waruto210/tinykv

## Project1 StandaloneKV

基于PingCAP修改的badger实现一个单机的支持column family的K/V存储。这个非常简单，唯一让我觉得不舒服的就是，文档和注释并没有提示应该某些情况是否应该抛出error，比如KeyNotFound，要查看测试才知道。

基于badger实现`StandAloneStorage`，要求实现如下的`Storage`接口，这个接口也是后面真正的分布式`RaftStorage`要实现的接口。另外还有一个`MemStorage`实现了该接口，用于测试。

```go
type Storage interface {
	Start() error
	Stop() error
	Write(ctx *kvrpcpb.Context, batch []Modify) error
	Reader(ctx *kvrpcpb.Context) (StorageReader, error)
}

type StorageReader interface {
	// When the key doesn't exist, return nil for the value
	GetCF(cf string, key []byte) ([]byte, error)
	IterCF(cf string) engine_util.DBIterator
	Close()
}
```

## Project2 RaftKV

这部分要求实现一个单个region的raft kv。

### Part A

在最内部的`Raft`结构中，使用`RaftLog`来管理日志。它维护着各种index：

```shell
snapshot/first.....applied....committed....stabled.....last
```

所有未压缩的log entries都会被放在内存中的`entries`数组（日志压缩后，应该更新），从`first`开始；`stable`表示已经被持久化到`storage`中的日志，last表示当前最新日志。

新建Raft时，注意从`config.storage`回复之前的信息；选举时，要注意处理一些corner case，例如只有一个节点。

当节点成为Leader后，应该先Append一个no-op entry，并广播给其他节点，因为新Leader虽然一定具有最新的日志，但commit index不一定是最新的，而且Raft不允许Leader直接commit不属于自己任期的日志，这样可以尽快更快地更新Leader的commit index到最新。在PingCAP的[TiKV 功能介绍 - Lease Read](https://pingcap.com/zh/blog/lease-read)中也提到了这个问题，etcd和TiKV刚开始都没注意到这个Bug。

然后要实现`RawNode`的两个关键方法:`HasReady()`和`Advance()`。前者返回一个`Ready`结构体，记录了Raft实例的状态，需要被持久化的日志，需要被apply的日志，需要被apply的snapshot，需要发送到其他Raft实例的消息；后者在前者返回的`Ready`被处理后，需要更新`Raft`实例的相关状态。

### Part B

这一部分是驱动Raft KV的核心。

主要步骤为：

1. 对TinyKV的操作被发送给Raft Leader所在节点；
2. Leader节点的`peerMsgHandler.proposeRaftCommand`记录proposal，并将操作转化为Raft log，驱动Raft达成共识；
3. `peerMsgHandler.HandleRaftReady`：每个节点通过`RawNode`获取`Ready`，将需要被持久化的信息持久化，将需要被发送的消息发送出去，然后调用`Advance`，更新Raft实例的状态。
4. Leader节点还需要处理当初留下的proposal，通过callback回复客户端。

对于读操作，可以直接将其转化为一个Log，等到`HandleRaftReady`时回复客户端，这延迟会很高；也可以采用Raft论文 section8的优化措施，PingCAP的[TiKV 功能介绍 - Lease Read](https://pingcap.com/zh/blog/lease-read)中也做了说明。另外，apply log也可以异步执行，提升效率。

### Part C

参照PingCAP的[TiKV 源码解析系列文章（十）Snapshot 的发送和接收](https://pingcap.com/zh/blog/tikv-source-code-reading-10)。

> 在 Raft 中，Snapshot 指的是整个 State Machine 数据的一份快照，大体上有以下这几种情况需要用到 Snapshot：
>
> 1. 正常情况下 leader 与 follower 之间是通过 append log 的方式进行同步的，出于空间和效率的考虑，leader 会定期清理过老的 log。假如 follower/learner 出现宕机或者网络隔离，恢复以后可能所缺的 log 已经在 leader 节点被清理掉了，此时只能通过 Snapshot 的方式进行同步。
> 2. Raft 加入新的节点的，由于新节点没同步过任何日志，只能通过接收 Snapshot 的方式来同步。实际上这也可以认为是 1 的一种特殊情形。
> 3. 出于备份/恢复等需求，应用层需要 dump 一份 State Machine 的完整数据。

实际上主要是情况1和2。

Snapshot不是作为普通的RaftMessage发送的，因为其Size太大。

Raftstore 想要gc时，propose一个AdminCmdType_CompactLog，等到commit后，处理ready时，修改RaftTruncatedState，然后进行实际的gc删除日志。后续Raft Leader向follower发送日志时，如果找不到next指针对应的log，那么该log由于compaction已经被丢弃了，所以只能发送snapshot。Leader调用`Storage.Snapshot()`生成snapshot，就绪后，Leader发出snapshot message，follower 收到snapshot message后，follower调用handleSnapshot处理，在`RaftLog`中记录`pendingSnapshot`，等`handleRaftReady`时，根据snapshot message的内信息，新建task去apply snapshot。snapshot具体的传输及apply细节TinyKV框架已经实现好了，要了解的话，可以查看👆的文章。

## Project3 MultiRaftKV

这一点，要实现多region多Raft Group的机制。

### Part A

实现3A的leader transfer和conf change非常简单，我觉得这里安排不合理，把太多内容安排到3B了，3A的测试也不足，导致很多坑在3B才被发现。

Raft实例使用`PendingConfIndex`来记录最新的conf change entry的index，如果有更新的conf change entry，应该修改为最新的，因为可能有Leader propose新的conf change之后，没有来得及复制到大多数节点，Leader崩溃，重新选举的Leader没有该日志，此时客户端可能会propose新的conf change。

### Part B

#### leader transfer和conf change

这部分要实现对`AdminCmdType_TransferLeader`和`AdminCmdType_ChangePeer`的处理。

当`Raft.leadTransferee`不为None时，为了使leader transfer尽快成功，应该拒绝propose新的command。

对于conf change，有一些坑。

首先，新建peer的Raft实例，其`Raft.Prs`是空的，要等到apply snapshot后，才能获取到当前Group的peers信息，这种情况下，`r.Prs[r.id]`不存在，而另一种情况，r由于conf change被删除，`r.Prs[r.id]`也不存在，如果直接返回，依靠判断`r.Prs[r.id]`来决定是否要处理message，是不行的。所以，作如下的判断，让新peer能够正常接收message。

```go
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
```

此外，考虑到新节点没有数据，为了避免不必要的超时选举（而且由于`Prs`为空，所以选举会直接成功，造成脑裂），当节点的term为0时，不进行tick；收到Leader的心跳后，立即将自己和Leader加入到`r.Prs`中。

解决完以上问题后，跑测试出现超时的概率还是比较大，通过打log发现以下问题：执行完`Raft.addNode`后，Leader向新peer发送snapshot，但是有时会出现发送完snapshot后，新的peer才创建完成，开始接受消息，导致这个snapshot消失了。在我的实现中，发送snapshot后，直接`r.Prs[to].Next = snapshot.Metadata.Index + 1`，因为不这样做，很可能在新peer的response回来之前，又向其发送snapshot，而生成snapshot是极其费时的；但是在前面的问题下，由于snapshot丢了，那么Leader发送后续日志时，新peer会拒绝，Leader将Next -= 1，然后继续，指导Next小于Leader日志的first index，如此来回，耗费了大量时间，自然就超时了。follower会将response message的`m.Index`设置为自己的last index，leader发现其小于自己的first index的话，就立即发送snapshot。

此外，conf change有一个特殊case。考虑：当前Raft group有两个节点Leader A、Follower B，conf change要remove A，那么会出现以下问题，A把conf change的log成功复制给B之后，A apply conf change，把自己删除，没来得及把新的commit index发送给B；此时B的commit index不够新，无法apply这条con change，然后B超时，开启选举，此时B的`Prs`中还有A，B永远无法选举成功。这种问题有一个解决办法，就是remove自己时，计算quorum不要把自己算进去。但是TinyKV的框架不方便实现这个，底层Raft并不知道是remove还是add，更不知道remove谁，要实现的话，需要更改一些代码。所以我选择，在这种情况下，直接return，不予接受。

```go
if req.ChangePeer.ChangeType == eraftpb.ConfChangeType_RemoveNode && d.IsLeader() &&
  	len(d.Region().Peers) == 2 && req.ChangePeer.Peer.Id == d.PeerId() {
    //log.Infof("%s return corner case\n", d.Tag)
    err := fmt.Sprintf("%s return corner case\n", d.Tag)
    cb.Done(ErrResp(errors.New(err)))
    return
}
```

#### split region in raftstore

这里要实现region分裂，实现了这个，就真的实现了multi-raft K/V store了。流程比较简单，按照文档给出的流程就好了。

不过在测试中遇到了`no region`问题，在[asktug](https://asktug.com/t/topic/274159)上，发现这个问题挺普遍的。这是因为：向PD请求region信息时，找不到对应的region信息。

region分裂一般的实现是 [A, B) -> [A, C) + [C, B)，现有region分配为[A, C)，新region分配为[C, B)。旧region是正常的，Leader在持续给PD发送心跳，PD能够及时更新region信息，而新region还需要等待多个peer创建完成，超时，然后选出Leader，发送心跳给PD。因为，向PD查新region信息时，有一段时间查不到[C, B)的信息。

我的解决方案是：首先，对于Term为5的节点（region分裂，新建的正常节点Term是5），立即开始选举，为了防止多个节点同时开始选举，导致多次选举失败，可以仅让Id为偶数的节点开始选举；此外，由于测试中，请求的key在增大，所以为了可以让旧region负责[C, B)，新region负责[A, C)，这样能够split完成后，能够立即响应新的请求，不过这种改进感觉只算是为了通过测试的tricky。

### Part C

这部分是实现一个小型的PD，实现收集心跳与集群平衡，比较简单，按照文档实现即可。

不过，文档中少了一个限制条件，在测试中体现了，被迁移的region，其分布的store数量要满足集群的`MaxReplicas`。这应该是为了防止迁移region导致集群不可用，做的优化。

## Project 4: Transactions

TinyKV采用的Percolator算法，提供了snapshot隔离性，客户端从数据库读到的数据就像从它开始执行事务时数据库被frozen了一样（客户端观察到数据库一个一致的view）。

Percolator算法源自[Large-scale Incremental Processing Using Distributed Transactions and Notifications](https://storage.googleapis.com/pub-tools-public-publication-data/pdf/36726.pdf)，可以参考PingCAP这篇文章[Deep Dive TiKV - Percolator](https://tikv.org/deep-dive/distributed-transaction/percolator/)。

### Part A
这部分就是实现对MVCC基础结构的封装，比较简单，但是代码可能写起来有点烦。

### Part B
这部分实现Percolator事务最关键的三个操作，读，Pewwrite，和commit。

- KvGet：

    - 时间戳ts
    - 查找是否有[0, ts]的锁，如果有，那么我们不能确定该事务是否在ts前被commit（已经commit，锁还没释放完），返回，稍后重试；如果没有，可以读
    - 从write CF读取[0, ts]范围内最新的write记录，从中获取对应事务的start_ts
    - 根据start_ts然后读取default CF

- KvPrewrite

    - 时间戳start_ts
    - 对每个key，加一个lock，然后以start_ts把数据写入default CF，选择一个lock为primary lock，每个lock都包括start_ts；如果key上已经有lock，回滚事务

- Kv Commit

    - 时间戳commit_ts
    - 移除primary lock，同时在write CF写入一个带有start_ts的记录；如果primary lock没有了（超时，被其他事务移除了），事务失败
    - 移除所有secondary lock

    只要primary lock被移除，事务就算成功。

有一个比较关键的地方，原Percolator系统基于BigTable，它是支持单行事务的，lock，write，data只不过是单行的一个列；而TinyKV这里，是3个CF，虽然我们可以原子性的写入3个CF，但是考虑：如果两个事务同时检查Key是否加锁，然后发现没有锁，在同时写入锁，这中间并不会有任何阻碍。所以，框架提供了Latch，注释写道:

> ```
> Only one thread can hold a latch at a time and all keys that a command might write must be locked
> // at once.
> ```

### Part B

这部分比较简单，实现四个操作，主要是用于检查事务状态，决定回滚还是提交。

- `KvScan`：用于按 Key 顺序扫描，类似KvGet一样实现即可；
- `KvCheckTxnStatus`：用于检查事务锁的状态；
- `KvBatchRollback`：用于批量回滚数据；
- `KvResolveLock`：使用`KvCheckTxnStatus`检查锁的状态后，再使用`KvResolveLock`回滚或者提交。
