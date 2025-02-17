// Copyright 2021 TiKV Authors
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

// NOTE: The code in this file is based on code from the
// TiDB project, licensed under the Apache License v 2.0
//
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/locate/region_request.go
//

// Copyright 2016 PingCAP, Inc.
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

package locate

import (
	"context"
	"fmt"
	"maps"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pkg/errors"
	"github.com/tikv/client-go/v2/config/retry"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/internal/client"
	"github.com/tikv/client-go/v2/internal/logutil"
	"github.com/tikv/client-go/v2/kv"
	"github.com/tikv/client-go/v2/metrics"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/util"
	pderr "github.com/tikv/pd/client/errs"
)

// shuttingDown is a flag to indicate tidb-server is exiting (Ctrl+C signal
// receved for example). If this flag is set, tikv client should not retry on
// network error because tidb-server expect tikv client to exit as soon as possible.
var shuttingDown uint32

// randIntn is only use for testing.
var randIntn = rand.Intn

// StoreShuttingDown atomically stores ShuttingDown into v.
func StoreShuttingDown(v uint32) {
	atomic.StoreUint32(&shuttingDown, v)
}

// LoadShuttingDown atomically loads ShuttingDown.
func LoadShuttingDown() uint32 {
	return atomic.LoadUint32(&shuttingDown)
}

// RegionRequestSender sends KV/Cop requests to tikv server. It handles network
// errors and some region errors internally.
//
// Typically, a KV/Cop request is bind to a region, all keys that are involved
// in the request should be located in the region.
// The sending process begins with looking for the address of leader store's
// address of the target region from cache, and the request is then sent to the
// destination tikv server over TCP connection.
// If region is updated, can be caused by leader transfer, region split, region
// merge, or region balance, tikv server may not able to process request and
// send back a RegionError.
// RegionRequestSender takes care of errors that does not relevant to region
// range, such as 'I/O timeout', 'NotLeader', and 'ServerIsBusy'. If fails to
// send the request to all replicas, a fake rregion error may be returned.
// Caller which receives the error should retry the request.
//
// For other region errors, since region range have changed, the request may need to
// split, so we simply return the error to caller.
type RegionRequestSender struct {
	regionCache       *RegionCache
	apiVersion        kvrpcpb.APIVersion
	client            client.Client
	storeAddr         string
	rpcError          error
	replicaSelector   ReplicaSelector
	failStoreIDs      map[uint64]struct{}
	failProxyStoreIDs map[uint64]struct{}
	Stats             *RegionRequestRuntimeStats
	AccessStats       *ReplicaAccessStats
}

func (s *RegionRequestSender) String() string {
	if s.replicaSelector == nil {
		return fmt.Sprintf("{rpcError:%v, replicaSelector: %v}", s.rpcError, s.replicaSelector)
	}
	return fmt.Sprintf("{rpcError:%v, replicaSelector: %v}", s.rpcError, s.replicaSelector.String())
}

// RegionRequestRuntimeStats records the runtime stats of send region requests.
type RegionRequestRuntimeStats struct {
	RPCStats map[tikvrpc.CmdType]*RPCRuntimeStats
	RequestErrorStats
}

// RequestErrorStats records the request error(region error and rpc error) count.
type RequestErrorStats struct {
	// ErrStats record the region error and rpc error, and their count.
	// Attention: avoid too many error types, ErrStats only record the first 16 different errors.
	ErrStats    map[string]int
	OtherErrCnt int
}

// NewRegionRequestRuntimeStats returns a new RegionRequestRuntimeStats.
func NewRegionRequestRuntimeStats() *RegionRequestRuntimeStats {
	return &RegionRequestRuntimeStats{
		RPCStats: make(map[tikvrpc.CmdType]*RPCRuntimeStats),
	}
}

// RPCRuntimeStats indicates the RPC request count and consume time.
type RPCRuntimeStats struct {
	Count int64
	// Send region request consume time.
	Consume int64
}

// RecordRPCRuntimeStats uses to record the rpc count and duration stats.
func (r *RegionRequestRuntimeStats) RecordRPCRuntimeStats(cmd tikvrpc.CmdType, d time.Duration) {
	stat, ok := r.RPCStats[cmd]
	if !ok {
		r.RPCStats[cmd] = &RPCRuntimeStats{
			Count:   1,
			Consume: int64(d),
		}
		return
	}
	stat.Count++
	stat.Consume += int64(d)
}

// RecordRPCErrorStats uses to record the request error(region error label and rpc error) info and count.
func (r *RequestErrorStats) RecordRPCErrorStats(errLabel string) {
	if r.ErrStats == nil {
		// lazy init to avoid unnecessary allocation.
		r.ErrStats = make(map[string]int)
	}
	if len(r.ErrStats) < 16 {
		// Avoid too many error.
		r.ErrStats[errLabel]++
	} else {
		r.OtherErrCnt++
	}
}

// getErrMsg returns error message. if the error has cause error, then return cause error message.
func getErrMsg(err error) string {
	if err == nil {
		return ""
	}
	if causeErr := errors.Cause(err); causeErr != nil {
		return causeErr.Error()
	}
	return err.Error()
}

// String implements fmt.Stringer interface.
func (r *RegionRequestRuntimeStats) String() string {
	if r == nil {
		return ""
	}
	var builder strings.Builder
	for k, v := range r.RPCStats {
		if builder.Len() > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(k.String())
		builder.WriteString(":{num_rpc:")
		builder.WriteString(strconv.FormatInt(v.Count, 10))
		builder.WriteString(", total_time:")
		builder.WriteString(util.FormatDuration(time.Duration(v.Consume)))
		builder.WriteString("}")
	}
	if errStatsStr := r.RequestErrorStats.String(); errStatsStr != "" {
		builder.WriteString(", rpc_errors:")
		builder.WriteString(errStatsStr)
	}
	return builder.String()
}

// String implements fmt.Stringer interface.
func (r *RequestErrorStats) String() string {
	if len(r.ErrStats) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("{")
	for err, cnt := range r.ErrStats {
		if builder.Len() > 2 {
			builder.WriteString(", ")
		}
		builder.WriteString(err)
		builder.WriteString(":")
		builder.WriteString(strconv.Itoa(cnt))
	}
	if r.OtherErrCnt > 0 {
		builder.WriteString(", other_error:")
		builder.WriteString(strconv.Itoa(r.OtherErrCnt))
	}
	builder.WriteString("}")
	return builder.String()
}

// Clone returns a copy of itself.
func (r *RegionRequestRuntimeStats) Clone() *RegionRequestRuntimeStats {
	newRs := NewRegionRequestRuntimeStats()
	maps.Copy(newRs.RPCStats, r.RPCStats)
	if len(r.ErrStats) > 0 {
		newRs.ErrStats = make(map[string]int)
		maps.Copy(newRs.ErrStats, r.ErrStats)
		newRs.OtherErrCnt = r.OtherErrCnt
	}
	return newRs
}

// Merge merges other RegionRequestRuntimeStats.
func (r *RegionRequestRuntimeStats) Merge(rs *RegionRequestRuntimeStats) {
	if rs == nil {
		return
	}
	for cmd, v := range rs.RPCStats {
		stat, ok := r.RPCStats[cmd]
		if !ok {
			r.RPCStats[cmd] = &RPCRuntimeStats{
				Count:   v.Count,
				Consume: v.Consume,
			}
			continue
		}
		stat.Count += v.Count
		stat.Consume += v.Consume
	}
	if len(rs.ErrStats) > 0 {
		if r.ErrStats == nil {
			r.ErrStats = make(map[string]int)
		}
		for err, cnt := range rs.ErrStats {
			r.ErrStats[err] += cnt
		}
		r.OtherErrCnt += rs.OtherErrCnt
	}
}

// ReplicaAccessStats records the replica access info.
type ReplicaAccessStats struct {
	// AccessInfos records the access info
	AccessInfos []ReplicaAccessInfo
	// avoid to consume too much memory, after more than 5 records, count them by peerID in `OverflowAccessStat` map.
	OverflowAccessStat map[uint64]*RequestErrorStats
}

// ReplicaAccessInfo indicates the access path detail info of a request.
type ReplicaAccessInfo struct {
	Peer      uint64
	Store     uint64
	ReqReadTp ReqReadType
	Err       string
}

type ReqReadType byte

const (
	ReqLeader ReqReadType = iota
	ReqReplicaRead
	ReqStaleRead
)

func (s *ReplicaAccessStats) recordReplicaAccessInfo(staleRead, replicaRead bool, peerID, storeID uint64, err string) {
	if len(s.AccessInfos) < 5 {
		tp := ReqLeader
		if replicaRead {
			tp = ReqReplicaRead
		} else if staleRead {
			tp = ReqStaleRead
		}
		s.AccessInfos = append(s.AccessInfos, ReplicaAccessInfo{
			Peer:      peerID,
			Store:     storeID,
			ReqReadTp: tp,
			Err:       err,
		})
		return
	}
	if s.OverflowAccessStat == nil {
		s.OverflowAccessStat = make(map[uint64]*RequestErrorStats)
	}
	stat, ok := s.OverflowAccessStat[peerID]
	if !ok {
		stat = &RequestErrorStats{}
		s.OverflowAccessStat[peerID] = stat
	}
	stat.RecordRPCErrorStats(err)
}

// String implements fmt.Stringer interface.
func (s *ReplicaAccessStats) String() string {
	if s == nil {
		return ""
	}
	var builder strings.Builder
	for i, info := range s.AccessInfos {
		if i > 0 {
			builder.WriteString(", ")
		}
		switch info.ReqReadTp {
		case ReqLeader:
			builder.WriteString("{")
		case ReqReplicaRead:
			builder.WriteString("{replica_read, ")
		case ReqStaleRead:
			builder.WriteString("{stale_read, ")
		}
		builder.WriteString("peer:")
		builder.WriteString(strconv.FormatUint(info.Peer, 10))
		builder.WriteString(", store:")
		builder.WriteString(strconv.FormatUint(info.Store, 10))
		builder.WriteString(", err:")
		builder.WriteString(info.Err)
		builder.WriteString("}")
	}
	if len(s.OverflowAccessStat) > 0 {
		builder.WriteString(", overflow_count:{")
		cnt := 0
		for peerID, stat := range s.OverflowAccessStat {
			if stat == nil {
				continue
			}
			if cnt > 0 {
				builder.WriteString(", ")
			}
			builder.WriteString("{peer:")
			builder.WriteString(strconv.FormatUint(peerID, 10))
			builder.WriteString(", error_stats:")
			builder.WriteString(stat.String())
			builder.WriteString("}")
			cnt++
		}
		builder.WriteString("}")
	}
	return builder.String()
}

// NewRegionRequestSender creates a new sender.
func NewRegionRequestSender(regionCache *RegionCache, client client.Client) *RegionRequestSender {
	return &RegionRequestSender{
		regionCache: regionCache,
		apiVersion:  regionCache.codec.GetAPIVersion(),
		client:      client,
	}
}

// GetRegionCache returns the region cache.
func (s *RegionRequestSender) GetRegionCache() *RegionCache {
	return s.regionCache
}

// GetClient returns the RPC client.
func (s *RegionRequestSender) GetClient() client.Client {
	return s.client
}

// getClientExt returns the client with ClientExt interface.
// Return nil if the client does not implement ClientExt.
// Don't use in critical path.
func (s *RegionRequestSender) getClientExt() client.ClientExt {
	ext, _ := s.client.(client.ClientExt)
	return ext
}

// SetStoreAddr specifies the dest store address.
func (s *RegionRequestSender) SetStoreAddr(addr string) {
	s.storeAddr = addr
}

// GetStoreAddr returns the dest store address.
func (s *RegionRequestSender) GetStoreAddr() string {
	return s.storeAddr
}

// GetRPCError returns the RPC error.
func (s *RegionRequestSender) GetRPCError() error {
	return s.rpcError
}

// SetRPCError rewrite the rpc error.
func (s *RegionRequestSender) SetRPCError(err error) {
	s.rpcError = err
}

// SendReq sends a request to tikv server. If fails to send the request to all replicas,
// a fake region error may be returned. Caller which receives the error should retry the request.
// It also returns the times of retries in RPC layer. A positive retryTimes indicates a possible undetermined error.
func (s *RegionRequestSender) SendReq(
	bo *retry.Backoffer, req *tikvrpc.Request, regionID RegionVerID, timeout time.Duration,
) (*tikvrpc.Response, int, error) {
	resp, _, retryTimes, err := s.SendReqCtx(bo, req, regionID, timeout, tikvrpc.TiKV)
	return resp, retryTimes, err
}

func (s *RegionRequestSender) recordRPCAccessInfo(req *tikvrpc.Request, rpcCtx *RPCContext, err string) {
	if req == nil || rpcCtx == nil || rpcCtx.Peer == nil || rpcCtx.Store == nil {
		return
	}
	if s.AccessStats == nil {
		s.AccessStats = &ReplicaAccessStats{}
	}
	s.AccessStats.recordReplicaAccessInfo(req.StaleRead, req.ReplicaRead, rpcCtx.Peer.GetId(), rpcCtx.Store.storeID, err)
}

type replica struct {
	store         *Store
	peer          *metapb.Peer
	epoch         uint32
	attempts      int
	attemptedTime time.Duration
	// deadlineErrUsingConfTimeout indicates the replica is already tried, but the received deadline exceeded error.
	deadlineErrUsingConfTimeout bool
	dataIsNotReady              bool
	notLeader                   bool
	serverIsBusy                bool
}

func (r *replica) getEpoch() uint32 {
	return atomic.LoadUint32(&r.epoch)
}

func (r *replica) isEpochStale() bool {
	return r.epoch != atomic.LoadUint32(&r.store.epoch)
}

func (r *replica) isExhausted(maxAttempt int, maxAttemptTime time.Duration) bool {
	return r.attempts >= maxAttempt || (maxAttemptTime > 0 && r.attemptedTime >= maxAttemptTime)
}

func (r *replica) onUpdateLeader() {
	if r.isExhausted(maxReplicaAttempt, maxReplicaAttemptTime) {
		// Give the replica one more chance and because each follower is tried only once,
		// it won't result in infinite retry.
		r.attempts = maxReplicaAttempt - 1
		r.attemptedTime = 0
	}
	r.notLeader = false
}

type baseReplicaSelector struct {
	regionCache *RegionCache
	region      *Region
	replicas    []*replica
	// TiKV can reject the request when its estimated wait duration exceeds busyThreshold.
	// Then, the client will receive a ServerIsBusy error and choose another replica to retry.
	busyThreshold time.Duration
	// pendingBackoffs records the pending backoff by store_id for fast retry. Here are some examples to show how it works:
	// Example-1, fast retry and success:
	//      1. send req to store 1, got ServerIsBusy region error, record `store1 -> BoTiKVServerBusy` backoff in pendingBackoffs and fast retry next replica.
	//      2. retry in store2, and success.
	//         Since the request is success, we can skip the backoff and fast return result to user.
	// Example-2: fast retry different replicas but all failed:
	//      1. send req to store 1, got ServerIsBusy region error, record `store1 -> BoTiKVServerBusy` backoff in pendingBackoffs and fast retry next replica.
	//      2. send req to store 2, got ServerIsBusy region error, record `store2 -> BoTiKVServerBusy` backoff in pendingBackoffs and fast retry next replica.
	//      3. send req to store 3, got ServerIsBusy region error, record `store3 -> BoTiKVServerBusy` backoff in pendingBackoffs and fast retry next replica.
	//      4. no candidate since all stores are busy. But before return no candidate error to up layer, we need to call backoffOnNoCandidate function
	//         to apply a max pending backoff, the backoff is to avoid frequent access and increase the pressure on the cluster.
	// Example-3: fast retry same replica:
	//      1. send req to store 1, got ServerIsBusy region error, record `store1 -> BoTiKVServerBusy` backoff in pendingBackoffs and fast retry next replica.
	//      2. assume store 2 and store 3 are unreachable.
	//      3. re-send req to store 1 with replica-read. But before re-send to store1, we need to call backoffOnRetry function
	//         to apply pending BoTiKVServerBusy backoff, the backoff is to avoid frequent access and increase the pressure on the cluster.
	pendingBackoffs map[uint64]*backoffArgs
}

// TODO(crazycs520): remove this after replicaSelectorV2 stable.
type replicaSelector struct {
	baseReplicaSelector
	regionStore *regionStore
	labels      []*metapb.StoreLabel
	state       selectorState
	// replicas[targetIdx] is the replica handling the request this time
	targetIdx AccessIndex
	// replicas[proxyIdx] is the store used to redirect requests this time
	proxyIdx AccessIndex
}

func selectorStateToString(state selectorState) string {
	replicaSelectorState := "nil"
	if state != nil {
		switch state.(type) {
		case *accessKnownLeader:
			replicaSelectorState = "accessKnownLeader"
		case *accessFollower:
			replicaSelectorState = "accessFollower"
		case *accessByKnownProxy:
			replicaSelectorState = "accessByKnownProxy"
		case *tryFollower:
			replicaSelectorState = "tryFollower"
		case *tryNewProxy:
			replicaSelectorState = "tryNewProxy"
		case *tryIdleReplica:
			replicaSelectorState = "tryIdleReplica"
		case *invalidLeader:
			replicaSelectorState = "invalidLeader"
		case *invalidStore:
			replicaSelectorState = "invalidStore"
		case *stateBase:
			replicaSelectorState = "stateBase"
		case nil:
			replicaSelectorState = "nil"
		}
	}
	return replicaSelectorState
}

func (s *replicaSelector) String() string {
	selectorStateStr := "nil"
	if s != nil {
		selectorStateStr = selectorStateToString(s.state)
	}

	return fmt.Sprintf("replicaSelector{state: %v, %v}", selectorStateStr, s.baseReplicaSelector.String())
}

func (s *baseReplicaSelector) String() string {
	var replicaStatus []string
	cacheRegionIsValid := "unknown"
	if s != nil {
		if s.region != nil {
			if s.region.isValid() {
				cacheRegionIsValid = "true"
			} else {
				cacheRegionIsValid = "false"
			}
		}
		for _, replica := range s.replicas {
			replicaStatus = append(replicaStatus, fmt.Sprintf("peer: %v, store: %v, isEpochStale: %v, "+
				"attempts: %v, attempts_time: %v, replica-epoch: %v, store-epoch: %v, store-state: %v, store-liveness-state: %v",
				replica.peer.GetId(),
				replica.store.storeID,
				replica.isEpochStale(),
				replica.attempts,
				util.FormatDuration(replica.attemptedTime),
				replica.getEpoch(),
				atomic.LoadUint32(&replica.store.epoch),
				replica.store.getResolveState(),
				replica.store.getLivenessState(),
			))
		}
	}
	return fmt.Sprintf("cacheRegionIsValid: %v, replicaStatus: %v", cacheRegionIsValid, replicaStatus)
}

// selectorState is the interface of states of the replicaSelector.
// Here is the main state transition diagram:
//
//                                    exceeding maxReplicaAttempt
//           +-------------------+   || RPC failure && unreachable && no forwarding
// +-------->+ accessKnownLeader +----------------+
// |         +------+------------+                |
// |                |                             |
// |                | RPC failure                 v
// |                | && unreachable        +-----+-----+
// |                | && enable forwarding  |tryFollower+------+
// |                |                       +-----------+      |
// | leader becomes v                                          | all followers
// | reachable +----+-------------+                            | are tried
// +-----------+accessByKnownProxy|                            |
// ^           +------+-----------+                            |
// |                  |                           +-------+    |
// |                  | RPC failure               |backoff+<---+
// | leader becomes   v                           +---+---+
// | reachable  +-----+-----+ all proxies are tried   ^
// +------------+tryNewProxy+-------------------------+
//              +-----------+

type selectorState interface {
	next(*retry.Backoffer, *replicaSelector) (*RPCContext, error)
	onSendSuccess(*replicaSelector)
	onSendFailure(*retry.Backoffer, *replicaSelector, error)
	onNoLeader(*replicaSelector)
}

type stateChanged struct{}

func (c stateChanged) Error() string {
	return "replicaSelector state changed"
}

type stateBase struct{}

func (s stateBase) next(bo *retry.Backoffer, selector *replicaSelector) (*RPCContext, error) {
	return nil, nil
}

func (s stateBase) onSendSuccess(selector *replicaSelector) {
}

func (s stateBase) onSendFailure(backoffer *retry.Backoffer, selector *replicaSelector, err error) {
}

func (s stateBase) onNoLeader(selector *replicaSelector) {
}

// accessKnownLeader is the state where we are sending requests
// to the leader we suppose to be.
//
// After attempting maxReplicaAttempt times without success
// and without receiving new leader from the responses error,
// we should switch to tryFollower state.
type accessKnownLeader struct {
	stateBase
	leaderIdx AccessIndex
}

func (state *accessKnownLeader) next(bo *retry.Backoffer, selector *replicaSelector) (*RPCContext, error) {
	leader := selector.replicas[state.leaderIdx]
	liveness := leader.store.getLivenessState()
	if liveness == unreachable && selector.regionCache.enableForwarding {
		selector.state = &tryNewProxy{leaderIdx: state.leaderIdx}
		return nil, stateChanged{}
	}
	if !isLeaderCandidate(leader) {
		selector.state = &tryFollower{leaderIdx: state.leaderIdx, lastIdx: state.leaderIdx, fromAccessKnownLeader: true}
		return nil, stateChanged{}
	}
	if selector.busyThreshold > 0 {
		// If the leader is busy in our estimation, change to tryIdleReplica state to try other replicas.
		// If other replicas are all busy, tryIdleReplica will try the leader again without busy threshold.
		leaderEstimated := selector.replicas[state.leaderIdx].store.EstimatedWaitTime()
		if leaderEstimated > selector.busyThreshold {
			selector.state = &tryIdleReplica{leaderIdx: state.leaderIdx}
			return nil, stateChanged{}
		}
	}
	selector.targetIdx = state.leaderIdx
	return selector.buildRPCContext(bo)
}

// check leader is candidate or not.
func isLeaderCandidate(leader *replica) bool {
	// If hibernate region is enabled and the leader is not reachable, the raft group
	// will not be wakened up and re-elect the leader until the follower receives
	// a request. So, before the new leader is elected, we should not send requests
	// to the unreachable old leader to avoid unnecessary timeout.
	// If leader.deadlineErrUsingConfTimeout is true, it means the leader is already tried and received deadline exceeded error, then don't retry it.
	// If leader.notLeader is true, it means the leader is already tried and received not leader error, then don't retry it.
	if leader.store.getLivenessState() != reachable ||
		leader.isExhausted(maxReplicaAttempt, maxReplicaAttemptTime) ||
		leader.deadlineErrUsingConfTimeout ||
		leader.notLeader ||
		leader.isEpochStale() { // check leader epoch here, if leader.epoch staled, we can try other replicas. instead of buildRPCContext failed and invalidate region then retry.
		return false
	}
	return true
}

func (state *accessKnownLeader) onSendFailure(bo *retry.Backoffer, selector *replicaSelector, cause error) {
	liveness := selector.checkLiveness(bo, selector.targetReplica())
	// Only enable forwarding when unreachable to avoid using proxy to access a TiKV that cannot serve.
	if liveness == unreachable && len(selector.replicas) > 1 && selector.regionCache.enableForwarding {
		selector.state = &accessByKnownProxy{leaderIdx: state.leaderIdx}
		return
	}
	if liveness != reachable || selector.targetReplica().isExhausted(maxReplicaAttempt, maxReplicaAttemptTime) {
		selector.state = &tryFollower{leaderIdx: state.leaderIdx, lastIdx: state.leaderIdx, fromAccessKnownLeader: true}
	}
	if liveness != reachable {
		selector.invalidateReplicaStore(selector.targetReplica(), cause)
	}
}

func (state *accessKnownLeader) onNoLeader(selector *replicaSelector) {
	selector.state = &tryFollower{leaderIdx: state.leaderIdx, lastIdx: state.leaderIdx, fromAccessKnownLeader: true}
}

// tryFollower is the state where we cannot access the known leader
// but still try other replicas in case they have become the leader.
//
// In this state, a follower that is not tried will be used. If all
// followers are tried, we think we have exhausted the replicas.
// On sending failure in this state, if leader info is returned,
// the leader will be updated to replicas[0] and give it another chance.
type tryFollower struct {
	stateBase
	leaderIdx AccessIndex
	lastIdx   AccessIndex
	// fromAccessKnownLeader indicates whether the state is changed from `accessKnownLeader`.
	fromAccessKnownLeader bool
	labels                []*metapb.StoreLabel
}

func (state *tryFollower) next(bo *retry.Backoffer, selector *replicaSelector) (*RPCContext, error) {
	filterReplicas := func(fn func(*replica) bool) (AccessIndex, *replica) {
		for i := 0; i < len(selector.replicas); i++ {
			idx := AccessIndex((int(state.lastIdx) + i) % len(selector.replicas))
			if idx == state.leaderIdx {
				continue
			}
			selectReplica := selector.replicas[idx]
			if selectReplica.store.getLivenessState() != unreachable && !selectReplica.deadlineErrUsingConfTimeout &&
				fn(selectReplica) {
				return idx, selectReplica
			}
		}
		return -1, nil
	}

	if len(state.labels) > 0 {
		idx, selectReplica := filterReplicas(func(selectReplica *replica) bool {
			return selectReplica.store.IsLabelsMatch(state.labels) && !state.isExhausted(selectReplica)
		})
		if selectReplica != nil && idx >= 0 {
			state.lastIdx = idx
			selector.targetIdx = idx
		}
		// labels only take effect for first try.
		state.labels = nil
	}

	if selector.targetIdx < 0 {
		// Search replica that is not attempted from the last accessed replica
		idx, selectReplica := filterReplicas(func(selectReplica *replica) bool {
			return !state.isExhausted(selectReplica)
		})
		if selectReplica != nil && idx >= 0 {
			state.lastIdx = idx
			selector.targetIdx = idx
		}
	}

	// If all followers are tried and fail, backoff and retry.
	if selector.targetIdx < 0 {
		// when meet deadline exceeded error, do fast retry without invalidate region cache.
		if !hasDeadlineExceededError(selector.replicas) {
			selector.invalidateRegion()
		}
		metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("exhausted").Inc()
		return nil, nil
	}
	rpcCtx, err := selector.buildRPCContext(bo)
	if err != nil || rpcCtx == nil {
		return rpcCtx, err
	}
	if !state.fromAccessKnownLeader {
		replicaRead := true
		rpcCtx.contextPatcher.replicaRead = &replicaRead
	}
	staleRead := false
	rpcCtx.contextPatcher.staleRead = &staleRead
	return rpcCtx, nil
}

func (state *tryFollower) isExhausted(replica *replica) bool {
	if replica.dataIsNotReady {
		// we can retry DataIsNotReady replica by replica-read.
		return replica.isExhausted(2, 0)
	}
	return replica.isExhausted(1, 0)
}

func (state *tryFollower) onSendSuccess(selector *replicaSelector) {
	if state.fromAccessKnownLeader {
		peer := selector.targetReplica().peer
		if !selector.region.switchWorkLeaderToPeer(peer) {
			logutil.BgLogger().Warn("the store must exist",
				zap.Uint64("store", peer.StoreId),
				zap.Uint64("peer", peer.Id))
		}
	}
}

func (state *tryFollower) onSendFailure(bo *retry.Backoffer, selector *replicaSelector, cause error) {
	if selector.checkLiveness(bo, selector.targetReplica()) != reachable {
		selector.invalidateReplicaStore(selector.targetReplica(), cause)
	}
}

// accessByKnownProxy is the state where we are sending requests through
// regionStore.proxyTiKVIdx as a proxy.
type accessByKnownProxy struct {
	stateBase
	leaderIdx AccessIndex
}

func (state *accessByKnownProxy) next(bo *retry.Backoffer, selector *replicaSelector) (*RPCContext, error) {
	leader := selector.replicas[state.leaderIdx]
	if leader.store.getLivenessState() == reachable {
		selector.regionStore.unsetProxyStoreIfNeeded(selector.region)
		selector.state = &accessKnownLeader{leaderIdx: state.leaderIdx}
		return nil, stateChanged{}
	}

	if selector.regionStore.proxyTiKVIdx >= 0 {
		selector.targetIdx = state.leaderIdx
		selector.proxyIdx = selector.regionStore.proxyTiKVIdx
		return selector.buildRPCContext(bo)
	}

	selector.state = &tryNewProxy{leaderIdx: state.leaderIdx}
	return nil, stateChanged{}
}

func (state *accessByKnownProxy) onSendFailure(bo *retry.Backoffer, selector *replicaSelector, cause error) {
	selector.state = &tryNewProxy{leaderIdx: state.leaderIdx}
	if selector.checkLiveness(bo, selector.proxyReplica()) != reachable {
		selector.invalidateReplicaStore(selector.proxyReplica(), cause)
	}
}

func (state *accessByKnownProxy) onNoLeader(selector *replicaSelector) {
	selector.state = &tryFollower{leaderIdx: state.leaderIdx, lastIdx: state.leaderIdx, fromAccessKnownLeader: true}
}

// tryNewProxy is the state where we try to find a node from followers as proxy.
type tryNewProxy struct {
	leaderIdx AccessIndex
}

func (state *tryNewProxy) next(bo *retry.Backoffer, selector *replicaSelector) (*RPCContext, error) {
	leader := selector.replicas[state.leaderIdx]
	if leader.store.getLivenessState() == reachable {
		selector.regionStore.unsetProxyStoreIfNeeded(selector.region)
		selector.state = &accessKnownLeader{leaderIdx: state.leaderIdx}
		return nil, stateChanged{}
	}

	candidateNum := 0
	for idx, replica := range selector.replicas {
		if state.isCandidate(AccessIndex(idx), replica) {
			candidateNum++
		}
	}

	// If all followers are tried as a proxy and fail, mark the leader store invalid, then backoff and retry.
	if candidateNum == 0 {
		metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("exhausted").Inc()
		selector.invalidateReplicaStore(leader, errors.Errorf("all followers are tried as proxy but fail"))
		selector.region.setSyncFlags(needReloadOnAccess)
		return nil, nil
	}

	// Skip advanceCnt valid candidates to find a proxy peer randomly
	advanceCnt := randIntn(candidateNum)
	for idx, replica := range selector.replicas {
		if !state.isCandidate(AccessIndex(idx), replica) {
			continue
		}
		if advanceCnt == 0 {
			selector.targetIdx = state.leaderIdx
			selector.proxyIdx = AccessIndex(idx)
			break
		}
		advanceCnt--
	}
	return selector.buildRPCContext(bo)
}

func (state *tryNewProxy) isCandidate(idx AccessIndex, replica *replica) bool {
	// Try each peer only once
	return idx != state.leaderIdx && !replica.isExhausted(1, 0)
}

func (state *tryNewProxy) onSendSuccess(selector *replicaSelector) {
	selector.regionStore.setProxyStoreIdx(selector.region, selector.proxyIdx)
}

func (state *tryNewProxy) onSendFailure(bo *retry.Backoffer, selector *replicaSelector, cause error) {
	if selector.checkLiveness(bo, selector.proxyReplica()) != reachable {
		selector.invalidateReplicaStore(selector.proxyReplica(), cause)
	}
}

func (state *tryNewProxy) onNoLeader(selector *replicaSelector) {
	selector.state = &tryFollower{leaderIdx: state.leaderIdx, lastIdx: state.leaderIdx, fromAccessKnownLeader: true}
}

// accessFollower is the state where we are sending requests to TiKV followers.
// If there is no suitable follower, requests will be sent to the leader as a fallback.
type accessFollower struct {
	stateBase
	// If tryLeader is true, the request can also be sent to the leader when !leader.isSlow()
	tryLeader   bool
	isStaleRead bool
	option      storeSelectorOp
	leaderIdx   AccessIndex
	lastIdx     AccessIndex
	learnerOnly bool
}

// Follower read will try followers first, if no follower is available, it will fallback to leader.
// Specially, for stale read, it tries local peer(can be either leader or follower), then use snapshot read in the leader,
// if the leader read receive server-is-busy and connection errors, the region cache is still valid,
// and the state will be changed to tryFollower, which will read by replica read.
func (state *accessFollower) next(bo *retry.Backoffer, selector *replicaSelector) (*RPCContext, error) {
	replicaSize := len(selector.replicas)
	resetStaleRead := false
	if state.lastIdx < 0 {
		if state.tryLeader {
			state.lastIdx = AccessIndex(randIntn(replicaSize))
		} else {
			if replicaSize <= 1 {
				state.lastIdx = state.leaderIdx
			} else {
				// Randomly select a non-leader peer
				state.lastIdx = AccessIndex(randIntn(replicaSize - 1))
				if state.lastIdx >= state.leaderIdx {
					state.lastIdx++
				}
			}
		}
	} else {
		// Stale Read request will retry the leader only by using the WithLeaderOnly option.
		if state.isStaleRead {
			WithLeaderOnly()(&state.option)
			// retry on the leader should not use stale read flag to avoid possible DataIsNotReady error as it always can serve any read.
			resetStaleRead = true
		}
		state.lastIdx++
	}

	// If selector is under `ReplicaReadPreferLeader` mode, we should choose leader as high priority.
	if state.option.preferLeader {
		state.lastIdx = state.leaderIdx
	}
	var offset int
	if state.lastIdx >= 0 {
		offset = randIntn(replicaSize)
	}
	reloadRegion := false
	for i := 0; i < replicaSize && !state.option.leaderOnly; i++ {
		var idx AccessIndex
		if state.option.preferLeader {
			if i == 0 {
				idx = state.lastIdx
			} else {
				// randomly select next replica, but skip state.lastIdx
				// since i must be greater than or equal to 1, so use i-1 to try from the first replica to make test stable.
				if (i-1+offset)%replicaSize == int(state.leaderIdx) {
					offset++
				}
				idx = AccessIndex((i - 1 + offset) % replicaSize)
			}
		} else {
			idx = AccessIndex((offset + i) % replicaSize)
		}
		selectReplica := selector.replicas[idx]
		if state.isCandidate(idx, selectReplica) {
			state.lastIdx = idx
			selector.targetIdx = idx
			break
		}
		if selectReplica.isEpochStale() &&
			selectReplica.store.getResolveState() == resolved &&
			selectReplica.store.getLivenessState() == reachable {
			reloadRegion = true
		}
	}
	if reloadRegion {
		selector.region.setSyncFlags(needDelayedReloadPending)
	}
	// If there is no candidate, fallback to the leader.
	if selector.targetIdx < 0 {
		leader := selector.replicas[state.leaderIdx]
		leaderEpochStale := leader.isEpochStale()
		leaderUnreachable := leader.store.getLivenessState() != reachable
		leaderExhausted := state.IsLeaderExhausted(leader)
		leaderInvalid := leaderEpochStale || leaderUnreachable || leaderExhausted
		if len(state.option.labels) > 0 && !state.option.leaderOnly {
			logutil.Logger(bo.GetCtx()).Warn("unable to find a store with given labels",
				zap.Uint64("region", selector.region.GetID()),
				zap.Any("labels", state.option.labels))
		}
		if leaderInvalid || leader.deadlineErrUsingConfTimeout {
			logutil.Logger(bo.GetCtx()).Warn("unable to find valid leader",
				zap.Uint64("region", selector.region.GetID()),
				zap.Bool("epoch-stale", leaderEpochStale),
				zap.Bool("unreachable", leaderUnreachable),
				zap.Bool("exhausted", leaderExhausted),
				zap.Bool("kv-timeout", leader.deadlineErrUsingConfTimeout),
				zap.Bool("stale-read", state.isStaleRead))
			// In stale-read, the request will fallback to leader after the local follower failure.
			// If the leader is also unavailable, we can fallback to the follower and use replica-read flag again,
			// The remote follower not tried yet, and the local follower can retry without stale-read flag.
			// If leader tried and received deadline exceeded error, try follower.
			// If labels are used, some followers would be filtered by the labels and can't be candidates, they still need to be retried.
			if state.isStaleRead || leader.deadlineErrUsingConfTimeout || len(state.option.labels) > 0 {
				selector.state = &tryFollower{
					leaderIdx: state.leaderIdx,
					lastIdx:   state.leaderIdx,
					labels:    state.option.labels,
				}
				if leaderEpochStale {
					selector.region.setSyncFlags(needDelayedReloadPending)
				}
				return nil, stateChanged{}
			}
			// when meet deadline exceeded error, do fast retry without invalidate region cache.
			if !hasDeadlineExceededError(selector.replicas) {
				selector.invalidateRegion()
			}
			metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("exhausted").Inc()
			return nil, nil
		}
		state.lastIdx = state.leaderIdx
		selector.targetIdx = state.leaderIdx
	}
	// Monitor the flows destination if selector is under `ReplicaReadPreferLeader` mode.
	if state.option.preferLeader {
		if selector.targetIdx != state.leaderIdx {
			selector.replicas[selector.targetIdx].store.recordReplicaFlowsStats(toFollower)
		} else {
			selector.replicas[selector.targetIdx].store.recordReplicaFlowsStats(toLeader)
		}
	}
	rpcCtx, err := selector.buildRPCContext(bo)
	if err != nil || rpcCtx == nil {
		return nil, err
	}
	if resetStaleRead {
		staleRead := false
		rpcCtx.contextPatcher.staleRead = &staleRead
	}
	return rpcCtx, nil
}

func (state *accessFollower) IsLeaderExhausted(leader *replica) bool {
	return leader.isExhausted(1, 0)
}

func (state *accessFollower) onSendFailure(bo *retry.Backoffer, selector *replicaSelector, cause error) {
	if selector.checkLiveness(bo, selector.targetReplica()) != reachable {
		selector.invalidateReplicaStore(selector.targetReplica(), cause)
	}
}

func (state *accessFollower) isCandidate(idx AccessIndex, replica *replica) bool {
	// the epoch is staled or retry exhausted, or the store is unreachable.
	if replica.isEpochStale() || replica.isExhausted(1, 0) || replica.store.getLivenessState() == unreachable || replica.deadlineErrUsingConfTimeout {
		return false
	}
	if state.option.leaderOnly {
		// The request can only be sent to the leader.
		return idx == state.leaderIdx
	}
	if !state.tryLeader && idx == state.leaderIdx {
		// The request cannot be sent to leader.
		return false
	}
	if state.learnerOnly {
		// The request can only be sent to the learner.
		return replica.peer.Role == metapb.PeerRole_Learner
	}
	// And If the leader store is abnormal to be accessed under `ReplicaReadPreferLeader` mode, we should choose other valid followers
	// as candidates to serve the Read request.
	if state.option.preferLeader && replica.store.healthStatus.IsSlow() {
		return false
	}
	// Choose a replica with matched labels.
	return replica.store.IsStoreMatch(state.option.stores) && replica.store.IsLabelsMatch(state.option.labels)
}

func hasDeadlineExceededError(replicas []*replica) bool {
	for _, replica := range replicas {
		if replica.deadlineErrUsingConfTimeout {
			// when meet deadline exceeded error, do fast retry without invalidate region cache.
			return true
		}
	}
	return false
}

// tryIdleReplica is the state where we find the leader is busy and retry the request using replica read.
type tryIdleReplica struct {
	stateBase
	leaderIdx AccessIndex
}

func (state *tryIdleReplica) next(bo *retry.Backoffer, selector *replicaSelector) (*RPCContext, error) {
	// Select a follower replica that has the lowest estimated wait duration
	minWait := time.Duration(math.MaxInt64)
	targetIdx := state.leaderIdx
	startIdx := randIntn(len(selector.replicas))
	for i := 0; i < len(selector.replicas); i++ {
		idx := (i + startIdx) % len(selector.replicas)
		r := selector.replicas[idx]
		// Don't choose leader again by default.
		if idx == int(state.leaderIdx) {
			continue
		}
		if !state.isCandidate(r) {
			continue
		}
		estimated := r.store.EstimatedWaitTime()
		if estimated > selector.busyThreshold {
			continue
		}
		if estimated < minWait {
			minWait = estimated
			targetIdx = AccessIndex(idx)
		}
		if minWait == 0 {
			break
		}
	}
	if targetIdx == state.leaderIdx && !isLeaderCandidate(selector.replicas[targetIdx]) {
		// when meet deadline exceeded error, do fast retry without invalidate region cache.
		if !hasDeadlineExceededError(selector.replicas) {
			selector.invalidateRegion()
		}
		metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("exhausted").Inc()
		return nil, nil
	}
	selector.targetIdx = targetIdx
	rpcCtx, err := selector.buildRPCContext(bo)
	if err != nil || rpcCtx == nil {
		return nil, err
	}
	replicaRead := targetIdx != state.leaderIdx
	rpcCtx.contextPatcher.replicaRead = &replicaRead
	if targetIdx == state.leaderIdx {
		// No threshold if all peers are too busy.
		selector.busyThreshold = 0
		rpcCtx.contextPatcher.busyThreshold = &selector.busyThreshold
	}
	return rpcCtx, nil
}

func (state *tryIdleReplica) isCandidate(replica *replica) bool {
	if replica.isEpochStale() ||
		replica.isExhausted(1, 0) ||
		replica.store.getLivenessState() != reachable ||
		replica.deadlineErrUsingConfTimeout {
		return false
	}
	return true
}

func (state *tryIdleReplica) onSendFailure(bo *retry.Backoffer, selector *replicaSelector, cause error) {
	if selector.checkLiveness(bo, selector.targetReplica()) != reachable {
		selector.invalidateReplicaStore(selector.targetReplica(), cause)
	}
}

type invalidStore struct {
	stateBase
}

func (state *invalidStore) next(_ *retry.Backoffer, _ *replicaSelector) (*RPCContext, error) {
	metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("invalidStore").Inc()
	return nil, nil
}

// TODO(sticnarf): If using request forwarding and the leader is unknown, try other followers
// instead of just switching to this state to backoff and retry.
type invalidLeader struct {
	stateBase
}

func (state *invalidLeader) next(_ *retry.Backoffer, _ *replicaSelector) (*RPCContext, error) {
	metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("invalidLeader").Inc()
	return nil, nil
}

// newReplicaSelector creates a replicaSelector which selects replicas according to reqType and opts.
// opts is currently only effective for follower read.
func newReplicaSelector(
	regionCache *RegionCache, regionID RegionVerID, req *tikvrpc.Request, opts ...StoreSelectorOption,
) (*replicaSelector, error) {
	cachedRegion := regionCache.GetCachedRegionWithRLock(regionID)
	if cachedRegion == nil || !cachedRegion.isValid() {
		return nil, nil
	}
	replicas := buildTiKVReplicas(cachedRegion)
	regionStore := cachedRegion.getStore()
	option := storeSelectorOp{}
	for _, op := range opts {
		op(&option)
	}
	var state selectorState
	if !req.ReplicaReadType.IsFollowerRead() {
		if regionCache.enableForwarding && regionStore.proxyTiKVIdx >= 0 {
			state = &accessByKnownProxy{leaderIdx: regionStore.workTiKVIdx}
		} else {
			state = &accessKnownLeader{leaderIdx: regionStore.workTiKVIdx}
		}
	} else {
		if req.ReplicaReadType == kv.ReplicaReadPreferLeader {
			WithPerferLeader()(&option)
		}
		tryLeader := req.ReplicaReadType == kv.ReplicaReadMixed || req.ReplicaReadType == kv.ReplicaReadPreferLeader
		state = &accessFollower{
			tryLeader:   tryLeader,
			isStaleRead: req.StaleRead,
			option:      option,
			leaderIdx:   regionStore.workTiKVIdx,
			lastIdx:     -1,
			learnerOnly: req.ReplicaReadType == kv.ReplicaReadLearner,
		}
	}

	return &replicaSelector{
		baseReplicaSelector: baseReplicaSelector{
			regionCache:   regionCache,
			region:        cachedRegion,
			replicas:      replicas,
			busyThreshold: time.Duration(req.BusyThresholdMs) * time.Millisecond,
		},
		regionStore: regionStore,
		labels:      option.labels,
		state:       state,
		targetIdx:   -1,
		proxyIdx:    -1,
	}, nil
}

func (s *replicaSelector) isValid() bool {
	return s != nil
}

func buildTiKVReplicas(region *Region) []*replica {
	regionStore := region.getStore()
	replicas := make([]*replica, 0, regionStore.accessStoreNum(tiKVOnly))
	for _, storeIdx := range regionStore.accessIndex[tiKVOnly] {
		replicas = append(
			replicas, &replica{
				store:    regionStore.stores[storeIdx],
				peer:     region.meta.Peers[storeIdx],
				epoch:    regionStore.storeEpochs[storeIdx],
				attempts: 0,
			},
		)
	}

	if val, err := util.EvalFailpoint("newReplicaSelectorInitialAttemptedTime"); err == nil {
		attemptedTime, err := time.ParseDuration(val.(string))
		if err != nil {
			panic(err)
		}
		for _, r := range replicas {
			r.attemptedTime = attemptedTime
		}
	}
	return replicas
}

const (
	maxReplicaAttempt = 10
	// The maximum time to allow retrying sending requests after RPC failure. In case an RPC request fails after
	// timeout (there might be network issue or the TiKV node stuck), we use this to avoid retrying 10 times which may cost too much time.
	// For request using `client.ReadTimeoutShort` which is 30s, it might retry twice which costs 1min.
	maxReplicaAttemptTime = time.Second * 50
)

// next creates the RPCContext of the current candidate replica.
// It returns a SendError if runs out of all replicas or the cached region is invalidated.
func (s *replicaSelector) next(bo *retry.Backoffer, _ *tikvrpc.Request) (rpcCtx *RPCContext, err error) {
	if !s.region.isValid() {
		metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("invalid").Inc()
		return nil, nil
	}

	s.targetIdx = -1
	s.proxyIdx = -1
	s.refreshRegionStore()
	for {
		rpcCtx, err = s.state.next(bo, s)
		if _, isStateChanged := err.(stateChanged); !isStateChanged {
			return
		}
	}
}

func (s *replicaSelector) targetReplica() *replica {
	if s.targetIdx >= 0 && int(s.targetIdx) < len(s.replicas) {
		return s.replicas[s.targetIdx]
	}
	return nil
}

func (s *replicaSelector) proxyReplica() *replica {
	if s.proxyIdx >= 0 && int(s.proxyIdx) < len(s.replicas) {
		return s.replicas[s.proxyIdx]
	}
	return nil
}

func (s *replicaSelector) getLabels() []*metapb.StoreLabel {
	return s.labels
}

// sliceIdentical checks whether two slices are referencing the same block of memory. Two `nil`s are also considered
// the same.
func sliceIdentical[T any](a, b []T) bool {
	return len(a) == len(b) && unsafe.SliceData(a) == unsafe.SliceData(b)
}

func (s *replicaSelector) refreshRegionStore() {
	oldRegionStore := s.regionStore
	newRegionStore := s.region.getStore()
	if oldRegionStore == newRegionStore {
		return
	}
	s.regionStore = newRegionStore

	// In the current implementation, if stores change, the address of it must change.
	// So we just compare the address here.
	// When stores change, we mark this replicaSelector as invalid to let the caller
	// recreate a new replicaSelector.
	if !sliceIdentical(oldRegionStore.stores, newRegionStore.stores) {
		s.state = &invalidStore{}
		return
	}

	// If leader has changed, it means a recent request succeeds an RPC
	// on the new leader.
	if oldRegionStore.workTiKVIdx != newRegionStore.workTiKVIdx {
		switch state := s.state.(type) {
		case *accessFollower:
			state.leaderIdx = newRegionStore.workTiKVIdx
		default:
			// Try the new leader and give it an addition chance if the
			// request is sent to the leader.
			newLeaderIdx := newRegionStore.workTiKVIdx
			s.state = &accessKnownLeader{leaderIdx: newLeaderIdx}
			if s.replicas[newLeaderIdx].isExhausted(maxReplicaAttempt, maxReplicaAttemptTime) {
				s.replicas[newLeaderIdx].attempts = maxReplicaAttempt - 1
				s.replicas[newLeaderIdx].attemptedTime = 0
			}
		}
	}
}

func (s *replicaSelector) buildRPCContext(bo *retry.Backoffer) (*RPCContext, error) {
	return s.baseReplicaSelector.buildRPCContext(bo, s.targetReplica(), s.proxyReplica())
}

func (s *baseReplicaSelector) buildRPCContext(bo *retry.Backoffer, targetReplica, proxyReplica *replica) (*RPCContext, error) {
	// Backoff and retry if no replica is selected or the selected replica is stale
	if targetReplica == nil || targetReplica.isEpochStale() ||
		(proxyReplica != nil && proxyReplica.isEpochStale()) {
		// TODO(youjiali1995): Is it necessary to invalidate the region?
		metrics.TiKVReplicaSelectorFailureCounter.WithLabelValues("stale_store").Inc()
		s.invalidateRegion()
		return nil, nil
	}

	rpcCtx := &RPCContext{
		ClusterID:  s.regionCache.clusterID,
		Region:     s.region.VerID(),
		Meta:       s.region.meta,
		Peer:       targetReplica.peer,
		Store:      targetReplica.store,
		AccessMode: tiKVOnly,
		TiKVNum:    len(s.replicas),
	}

	// Set leader addr
	addr, err := s.regionCache.getStoreAddr(bo, s.region, targetReplica.store)
	if err != nil {
		return nil, err
	}
	if len(addr) == 0 {
		return nil, nil
	}
	rpcCtx.Addr = addr
	targetReplica.attempts++

	// Set proxy addr
	if proxyReplica != nil {
		addr, err = s.regionCache.getStoreAddr(bo, s.region, proxyReplica.store)
		if err != nil {
			return nil, err
		}
		if len(addr) == 0 {
			return nil, nil
		}
		rpcCtx.ProxyStore = proxyReplica.store
		rpcCtx.ProxyAddr = addr
		proxyReplica.attempts++
	}

	return rpcCtx, nil
}

func (s *replicaSelector) onSendFailure(bo *retry.Backoffer, err error) {
	metrics.RegionCacheCounterWithSendFail.Inc()
	s.state.onSendFailure(bo, s, err)
}

func (s *replicaSelector) onReadReqConfigurableTimeout(req *tikvrpc.Request) bool {
	if isReadReqConfigurableTimeout(req) {
		if target := s.targetReplica(); target != nil {
			target.deadlineErrUsingConfTimeout = true
		}
		if accessLeader, ok := s.state.(*accessKnownLeader); ok {
			// If leader return deadline exceeded error, we should try to access follower next time.
			s.state = &tryFollower{leaderIdx: accessLeader.leaderIdx, lastIdx: accessLeader.leaderIdx}
		}
		return true
	}
	return false
}

func isReadReqConfigurableTimeout(req *tikvrpc.Request) bool {
	if req.MaxExecutionDurationMs >= uint64(client.ReadTimeoutShort.Milliseconds()) {
		// Configurable timeout should less than `ReadTimeoutShort`.
		return false
	}
	// Only work for read requests, return false for non-read requests.
	return isReadReq(req.Type)
}

func isReadReq(tp tikvrpc.CmdType) bool {
	switch tp {
	case tikvrpc.CmdGet, tikvrpc.CmdBatchGet, tikvrpc.CmdScan,
		tikvrpc.CmdCop, tikvrpc.CmdBatchCop, tikvrpc.CmdCopStream:
		return true
	default:
		return false
	}
}

func (s *baseReplicaSelector) getBaseReplicaSelector() *baseReplicaSelector {
	return s
}

func (s *baseReplicaSelector) checkLiveness(bo *retry.Backoffer, accessReplica *replica) livenessState {
	return accessReplica.store.requestLivenessAndStartHealthCheckLoopIfNeeded(bo, s.regionCache.bg, s.regionCache.stores)
}

func (s *baseReplicaSelector) invalidateReplicaStore(replica *replica, cause error) {
	store := replica.store
	if atomic.CompareAndSwapUint32(&store.epoch, replica.epoch, replica.epoch+1) {
		logutil.BgLogger().Info(
			"mark store's regions need be refill",
			zap.Uint64("id", store.storeID),
			zap.String("addr", store.addr),
			zap.Error(cause),
		)
		metrics.RegionCacheCounterWithInvalidateStoreRegionsOK.Inc()
		// schedule a store addr resolve.
		s.regionCache.stores.markStoreNeedCheck(store)
		store.healthStatus.markAlreadySlow()
	}
}

func (s *replicaSelector) onSendSuccess(_ *tikvrpc.Request) {
	s.state.onSendSuccess(s)
}

func (s *replicaSelector) onNotLeader(
	bo *retry.Backoffer, ctx *RPCContext, notLeader *errorpb.NotLeader,
) (shouldRetry bool, err error) {
	if target := s.targetReplica(); target != nil {
		target.notLeader = true
	}
	leaderIdx, err := s.baseReplicaSelector.onNotLeader(bo, ctx, notLeader)
	if err != nil {
		return false, err
	}
	if leaderIdx >= 0 {
		if isLeaderCandidate(s.replicas[leaderIdx]) {
			s.state = &accessKnownLeader{leaderIdx: AccessIndex(leaderIdx)}
		}
	} else {
		s.state.onNoLeader(s)
	}
	return true, nil
}

func (s *baseReplicaSelector) onNotLeader(
	bo *retry.Backoffer, ctx *RPCContext, notLeader *errorpb.NotLeader,
) (leaderIdx int, err error) {
	leader := notLeader.GetLeader()
	if leader == nil {
		// The region may be during transferring leader.
		err = bo.Backoff(retry.BoRegionScheduling, errors.Errorf("no leader, ctx: %v", ctx))
		return -1, err
	}
	return s.updateLeader(leader), nil
}

// updateLeader updates the leader of the cached region.
// If the leader peer isn't found in the region, the region will be invalidated.
// If switch to new leader successfully, returns the AccessIndex of the new leader in the replicas.
func (s *baseReplicaSelector) updateLeader(leader *metapb.Peer) int {
	if leader == nil {
		return -1
	}
	for i, replica := range s.replicas {
		if isSamePeer(replica.peer, leader) {
			// If hibernate region is enabled and the leader is not reachable, the raft group
			// will not be wakened up and re-elect the leader until the follower receives
			// a request. So, before the new leader is elected, we should not send requests
			// to the unreachable old leader to avoid unnecessary timeout.
			if replica.store.getLivenessState() != reachable {
				return -1
			}
			replica.onUpdateLeader()
			// Update the workTiKVIdx so that following requests can be sent to the leader immediately.
			if !s.region.switchWorkLeaderToPeer(leader) {
				panic("the store must exist")
			}
			logutil.BgLogger().Debug(
				"switch region leader to specific leader due to kv return NotLeader",
				zap.Uint64("regionID", s.region.GetID()),
				zap.Uint64("leaderStoreID", leader.GetStoreId()),
			)
			return i
		}
	}
	// Invalidate the region since the new leader is not in the cached version.
	s.region.invalidate(StoreNotFound)
	return -1
}

func (s *replicaSelector) onServerIsBusy(
	bo *retry.Backoffer, ctx *RPCContext, req *tikvrpc.Request, serverIsBusy *errorpb.ServerIsBusy,
) (shouldRetry bool, err error) {
	var store *Store
	if ctx != nil && ctx.Store != nil {
		store = ctx.Store
		if serverIsBusy.EstimatedWaitMs != 0 {
			ctx.Store.updateServerLoadStats(serverIsBusy.EstimatedWaitMs)
			if s.busyThreshold != 0 && isReadReq(req.Type) {
				// do not retry with batched coprocessor requests.
				// it'll be region misses if we send the tasks to replica.
				if req.Type == tikvrpc.CmdCop && len(req.Cop().Tasks) > 0 {
					return false, nil
				}
				switch state := s.state.(type) {
				case *accessKnownLeader:
					// Clear attempt history of the leader, so the leader can be accessed again.
					s.replicas[state.leaderIdx].attempts = 0
					s.state = &tryIdleReplica{leaderIdx: state.leaderIdx}
				}
			}
		} else {
			// Mark the server is busy (the next incoming READs could be redirected to expected followers.)
			ctx.Store.healthStatus.markAlreadySlow()
		}
	}
	backoffErr := errors.Errorf("server is busy, ctx: %v", ctx)
	if s.canFastRetry() {
		s.addPendingBackoff(store, retry.BoTiKVServerBusy, backoffErr)
		return true, nil
	}
	err = bo.Backoff(retry.BoTiKVServerBusy, backoffErr)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *replicaSelector) onDataIsNotReady() {
	if target := s.targetReplica(); target != nil {
		target.dataIsNotReady = true
	}
}

func (s *baseReplicaSelector) invalidateRegion() {
	if s.region != nil {
		s.region.invalidate(Other)
	}
}

func (s *RegionRequestSender) getRPCContext(
	bo *retry.Backoffer,
	req *tikvrpc.Request,
	regionID RegionVerID,
	et tikvrpc.EndpointType,
	opts ...StoreSelectorOption,
) (*RPCContext, error) {
	switch et {
	case tikvrpc.TiKV:
		if s.replicaSelector == nil {
			selector, err := NewReplicaSelector(s.regionCache, regionID, req, opts...) //nolint:staticcheck // ignore SA4023, never returns a nil interface value
			if selector == nil || !selector.isValid() || err != nil {                  //nolint:staticcheck // ignore SA4023, never returns a nil interface value
				return nil, err
			}
			s.replicaSelector = selector
		}
		return s.replicaSelector.next(bo, req)
	case tikvrpc.TiFlash:
		// Should ignore WN, because in disaggregated tiflash mode, TiDB will build rpcCtx itself.
		return s.regionCache.GetTiFlashRPCContext(bo, regionID, true, LabelFilterNoTiFlashWriteNode)
	case tikvrpc.TiDB:
		return &RPCContext{Addr: s.storeAddr}, nil
	case tikvrpc.TiFlashCompute:
		// In disaggregated tiflash mode, TiDB will build rpcCtx itself, so cannot reach here.
		return nil, errors.Errorf("should not reach here for disaggregated tiflash mode")
	default:
		return nil, errors.Errorf("unsupported storage type: %v", et)
	}
}

func (s *RegionRequestSender) reset() {
	s.replicaSelector = nil
	s.failStoreIDs = nil
	s.failProxyStoreIDs = nil
}

// IsFakeRegionError returns true if err is fake region error.
func IsFakeRegionError(err *errorpb.Error) bool {
	return err != nil && err.GetEpochNotMatch() != nil && len(err.GetEpochNotMatch().CurrentRegions) == 0
}

const slowLogSendReqTime = 100 * time.Millisecond

// SendReqCtx sends a request to tikv server and return response and RPCCtx of this RPC.
func (s *RegionRequestSender) SendReqCtx(
	bo *retry.Backoffer,
	req *tikvrpc.Request,
	regionID RegionVerID,
	timeout time.Duration,
	et tikvrpc.EndpointType,
	opts ...StoreSelectorOption,
) (
	resp *tikvrpc.Response,
	rpcCtx *RPCContext,
	retryTimes int,
	err error,
) {
	if span := opentracing.SpanFromContext(bo.GetCtx()); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("regionRequest.SendReqCtx", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		bo.SetCtx(opentracing.ContextWithSpan(bo.GetCtx(), span1))
	}

	if val, err := util.EvalFailpoint("tikvStoreSendReqResult"); err == nil {
		if s, ok := val.(string); ok {
			switch s {
			case "timeout":
				return nil, nil, 0, errors.New("timeout")
			case "GCNotLeader":
				if req.Type == tikvrpc.CmdGC {
					return &tikvrpc.Response{
						Resp: &kvrpcpb.GCResponse{RegionError: &errorpb.Error{NotLeader: &errorpb.NotLeader{}}},
					}, nil, 0, nil
				}
			case "PessimisticLockNotLeader":
				if req.Type == tikvrpc.CmdPessimisticLock {
					return &tikvrpc.Response{
						Resp: &kvrpcpb.PessimisticLockResponse{RegionError: &errorpb.Error{NotLeader: &errorpb.NotLeader{}}},
					}, nil, 0, nil
				}
			case "GCServerIsBusy":
				if req.Type == tikvrpc.CmdGC {
					return &tikvrpc.Response{
						Resp: &kvrpcpb.GCResponse{RegionError: &errorpb.Error{ServerIsBusy: &errorpb.ServerIsBusy{}}},
					}, nil, 0, nil
				}
			case "busy":
				return &tikvrpc.Response{
					Resp: &kvrpcpb.GCResponse{RegionError: &errorpb.Error{ServerIsBusy: &errorpb.ServerIsBusy{}}},
				}, nil, 0, nil
			case "requestTiDBStoreError":
				if et == tikvrpc.TiDB {
					return nil, nil, 0, errors.WithStack(tikverr.ErrTiKVServerTimeout)
				}
			case "requestTiFlashError":
				if et == tikvrpc.TiFlash {
					return nil, nil, 0, errors.WithStack(tikverr.ErrTiFlashServerTimeout)
				}
			}
		}
	}

	// If the MaxExecutionDurationMs is not set yet, we set it to be the RPC timeout duration
	// so TiKV can give up the requests whose response TiDB cannot receive due to timeout.
	if req.Context.MaxExecutionDurationMs == 0 {
		req.Context.MaxExecutionDurationMs = uint64(timeout.Milliseconds())
	}

	s.reset()
	startTime := time.Now()
	startBackOff := bo.GetTotalSleep()
	retryTimes = 0
	defer func() {
		if retryTimes > 0 {
			metrics.TiKVRequestRetryTimesHistogram.Observe(float64(retryTimes))
		}
	}()

	var staleReadCollector *staleReadMetricsCollector
	if req.StaleRead {
		staleReadCollector = &staleReadMetricsCollector{}
		defer func() {
			if retryTimes == 0 {
				metrics.StaleReadHitCounter.Add(1)
			} else {
				metrics.StaleReadMissCounter.Add(1)
			}
		}()
	}

	for {
		if retryTimes > 0 {
			if retryTimes%100 == 0 {
				logutil.Logger(bo.GetCtx()).Warn(
					"retry",
					zap.Uint64("region", regionID.GetID()),
					zap.Int("times", retryTimes),
				)
			}
		}

		rpcCtx, err = s.getRPCContext(bo, req, regionID, et, opts...)
		if err != nil {
			return nil, nil, retryTimes, err
		}

		if _, err := util.EvalFailpoint("invalidCacheAndRetry"); err == nil {
			// cooperate with tikvclient/setGcResolveMaxBackoff
			if c := bo.GetCtx().Value("injectedBackoff"); c != nil {
				resp, err = tikvrpc.GenRegionErrorResp(req, &errorpb.Error{EpochNotMatch: &errorpb.EpochNotMatch{}})
				return resp, nil, retryTimes, err
			}
		}
		if rpcCtx == nil {
			// TODO(youjiali1995): remove it when using the replica selector for all requests.
			// If the region is not found in cache, it must be out
			// of date and already be cleaned up. We can skip the
			// RPC by returning RegionError directly.

			// TODO: Change the returned error to something like "region missing in cache",
			// and handle this error like EpochNotMatch, which means to re-split the request and retry.
			if s.replicaSelector != nil {
				if err := s.replicaSelector.getBaseReplicaSelector().backoffOnNoCandidate(bo); err != nil {
					return nil, nil, retryTimes, err
				}
				if cost := time.Since(startTime); cost > slowLogSendReqTime || cost > timeout {
					s.logSendReqError(bo, "throwing pseudo region error due to no replica available", regionID, retryTimes, req, cost, bo.GetTotalSleep()-startBackOff, timeout)
				}
			}
			resp, err = tikvrpc.GenRegionErrorResp(req, &errorpb.Error{EpochNotMatch: &errorpb.EpochNotMatch{}})
			return resp, nil, retryTimes, err
		}

		var isLocalTraffic bool
		if staleReadCollector != nil && s.replicaSelector != nil {
			if target := s.replicaSelector.targetReplica(); target != nil {
				isLocalTraffic = target.store.IsLabelsMatch(s.replicaSelector.getLabels())
				staleReadCollector.onReq(req, isLocalTraffic)
			}
		}

		logutil.Eventf(bo.GetCtx(), "send %s request to region %d at %s", req.Type, regionID.id, rpcCtx.Addr)
		s.storeAddr = rpcCtx.Addr

		if _, err := util.EvalFailpoint("beforeSendReqToRegion"); err == nil {
			if hook := bo.GetCtx().Value("sendReqToRegionHook"); hook != nil {
				h := hook.(func(*tikvrpc.Request))
				h(req)
			}
		}

		req.Context.ClusterId = rpcCtx.ClusterID
		rpcCtx.contextPatcher.applyTo(&req.Context)
		if req.InputRequestSource != "" && s.replicaSelector != nil {
			patchRequestSource(req, s.replicaSelector.replicaType(rpcCtx))
		}
		if e := tikvrpc.SetContext(req, rpcCtx.Meta, rpcCtx.Peer); e != nil {
			return nil, nil, retryTimes, err
		}
		if s.replicaSelector != nil {
			if err := s.replicaSelector.getBaseReplicaSelector().backoffOnRetry(rpcCtx.Store, bo); err != nil {
				return nil, nil, retryTimes, err
			}
		}

		var retry bool
		resp, retry, err = s.sendReqToRegion(bo, rpcCtx, req, timeout)
		req.IsRetryRequest = true
		if err != nil {
			if cost := time.Since(startTime); cost > slowLogSendReqTime || cost > timeout {
				msg := fmt.Sprintf("send request failed, err: %v", err.Error())
				s.logSendReqError(bo, msg, regionID, retryTimes, req, cost, bo.GetTotalSleep()-startBackOff, timeout)
			}
			return nil, nil, retryTimes, err
		}

		if _, err1 := util.EvalFailpoint("afterSendReqToRegion"); err1 == nil {
			if hook := bo.GetCtx().Value("sendReqToRegionFinishHook"); hook != nil {
				h := hook.(func(*tikvrpc.Request, *tikvrpc.Response, error))
				h(req, resp, err)
			}
		}

		// recheck whether the session/query is killed during the Next()
		if err2 := bo.CheckKilled(); err2 != nil {
			return nil, nil, retryTimes, err2
		}
		if val, err := util.EvalFailpoint("mockRetrySendReqToRegion"); err == nil {
			if val.(bool) {
				retry = true
			}
		}
		if retry {
			retryTimes++
			continue
		}

		var regionErr *errorpb.Error
		regionErr, err = resp.GetRegionError()
		if err != nil {
			return nil, nil, retryTimes, err
		}
		if regionErr != nil {
			retry, err = s.onRegionError(bo, rpcCtx, req, regionErr)
			if err != nil {
				if cost := time.Since(startTime); cost > slowLogSendReqTime || cost > timeout {
					msg := fmt.Sprintf("send request on region error failed, err: %v", err.Error())
					s.logSendReqError(bo, msg, regionID, retryTimes, req, cost, bo.GetTotalSleep()-startBackOff, timeout)
				}
				return nil, nil, retryTimes, err
			}
			if retry {
				retryTimes++
				continue
			}
			if cost := time.Since(startTime); cost > slowLogSendReqTime || cost > timeout {
				s.logSendReqError(bo, "send request meet region error without retry", regionID, retryTimes, req, cost, bo.GetTotalSleep()-startBackOff, timeout)
			}
		} else {
			if s.replicaSelector != nil {
				s.replicaSelector.onSendSuccess(req)
			}
		}
		if staleReadCollector != nil {
			staleReadCollector.onResp(req.Type, resp, isLocalTraffic)
		}
		return resp, rpcCtx, retryTimes, nil
	}
}

func (s *RegionRequestSender) logSendReqError(bo *retry.Backoffer, msg string, regionID RegionVerID, retryTimes int, req *tikvrpc.Request, cost time.Duration, currentBackoffMs int, timeout time.Duration) {
	var builder strings.Builder
	// build the total round stats string.
	builder.WriteString("{total-backoff: ")
	builder.WriteString(util.FormatDuration(time.Duration(bo.GetTotalSleep() * int(time.Millisecond))))
	builder.WriteString(", total-backoff-times: ")
	builder.WriteString(strconv.Itoa(bo.GetTotalBackoffTimes()))
	if s.Stats != nil {
		builder.WriteString(", total-rpc: {")
		builder.WriteString(s.Stats.String())
		builder.WriteString("}")
	}
	builder.WriteString("}")
	totalRoundStats := builder.String()

	// build the current round stats string.
	builder.Reset()
	builder.WriteString("{time: ")
	builder.WriteString(util.FormatDuration(cost))
	builder.WriteString(", backoff: ")
	builder.WriteString(util.FormatDuration(time.Duration(currentBackoffMs * int(time.Millisecond))))
	builder.WriteString(", timeout: ")
	builder.WriteString(util.FormatDuration(timeout))
	builder.WriteString(", req-max-exec-timeout: ")
	builder.WriteString(util.FormatDuration(time.Duration(int64(req.Context.MaxExecutionDurationMs) * int64(time.Millisecond))))
	builder.WriteString(", retry-times: ")
	builder.WriteString(strconv.Itoa(retryTimes))
	if s.AccessStats != nil {
		builder.WriteString(", replica-access: {")
		builder.WriteString(s.AccessStats.String())
		builder.WriteString("}")
	}
	builder.WriteString("}")
	currentRoundStats := builder.String()
	logutil.Logger(bo.GetCtx()).Info(msg,
		zap.Uint64("req-ts", req.GetStartTS()),
		zap.String("req-type", req.Type.String()),
		zap.String("region", regionID.String()),
		zap.String("replica-read-type", req.ReplicaReadType.String()),
		zap.Bool("stale-read", req.StaleRead),
		zap.Stringer("request-sender", s),
		zap.String("total-round-stats", totalRoundStats),
		zap.String("current-round-stats", currentRoundStats))
}

// RPCCancellerCtxKey is context key attach rpc send cancelFunc collector to ctx.
type RPCCancellerCtxKey struct{}

// RPCCanceller is rpc send cancelFunc collector.
type RPCCanceller struct {
	sync.Mutex
	allocID   int
	cancels   map[int]func()
	cancelled bool
}

// NewRPCanceller creates RPCCanceller with init state.
func NewRPCanceller() *RPCCanceller {
	return &RPCCanceller{cancels: make(map[int]func())}
}

// WithCancel generates new context with cancel func.
func (h *RPCCanceller) WithCancel(ctx context.Context) (context.Context, func()) {
	nctx, cancel := context.WithCancel(ctx)
	h.Lock()
	if h.cancelled {
		h.Unlock()
		cancel()
		return nctx, func() {}
	}
	id := h.allocID
	h.allocID++
	h.cancels[id] = cancel
	h.Unlock()
	return nctx, func() {
		cancel()
		h.Lock()
		delete(h.cancels, id)
		h.Unlock()
	}
}

// CancelAll cancels all inflight rpc context.
func (h *RPCCanceller) CancelAll() {
	h.Lock()
	for _, c := range h.cancels {
		c()
	}
	h.cancelled = true
	h.Unlock()
}

func fetchRespInfo(resp *tikvrpc.Response) string {
	var extraInfo string
	if resp == nil || resp.Resp == nil {
		extraInfo = "nil response"
	} else {
		regionErr, e := resp.GetRegionError()
		if e != nil {
			extraInfo = e.Error()
		} else if regionErr != nil {
			extraInfo = regionErr.String()
		} else if prewriteResp, ok := resp.Resp.(*kvrpcpb.PrewriteResponse); ok {
			extraInfo = prewriteResp.String()
		}
	}
	return extraInfo
}

func (s *RegionRequestSender) sendReqToRegion(
	bo *retry.Backoffer, rpcCtx *RPCContext, req *tikvrpc.Request, timeout time.Duration,
) (resp *tikvrpc.Response, retry bool, err error) {
	// judge the store limit switch.
	if limit := kv.StoreLimit.Load(); limit > 0 {
		if err := s.getStoreToken(rpcCtx.Store, limit); err != nil {
			return nil, false, err
		}
		defer s.releaseStoreToken(rpcCtx.Store)
	}

	ctx := bo.GetCtx()
	if rawHook := ctx.Value(RPCCancellerCtxKey{}); rawHook != nil {
		var cancel context.CancelFunc
		ctx, cancel = rawHook.(*RPCCanceller).WithCancel(ctx)
		defer cancel()
	}

	// sendToAddr is the first target address that will receive the request. If proxy is used, sendToAddr will point to
	// the proxy that will forward the request to the final target.
	sendToAddr := rpcCtx.Addr
	if rpcCtx.ProxyStore == nil {
		req.ForwardedHost = ""
	} else {
		req.ForwardedHost = rpcCtx.Addr
		sendToAddr = rpcCtx.ProxyAddr
	}

	// Count the replica number as the RU cost factor.
	req.ReplicaNumber = 1
	if rpcCtx.Meta != nil && len(rpcCtx.Meta.GetPeers()) > 0 {
		req.ReplicaNumber = 0
		for _, peer := range rpcCtx.Meta.GetPeers() {
			role := peer.GetRole()
			if role == metapb.PeerRole_Voter || role == metapb.PeerRole_Learner {
				req.ReplicaNumber++
			}
		}
	}

	var sessionID uint64
	if v := bo.GetCtx().Value(util.SessionID); v != nil {
		sessionID = v.(uint64)
	}

	injectFailOnSend := false
	if val, e := util.EvalFailpoint("rpcFailOnSend"); e == nil {
		inject := true
		// Optional filters
		if s, ok := val.(string); ok {
			if s == "greengc" && !req.IsGreenGCRequest() {
				inject = false
			} else if s == "write" && !req.IsTxnWriteRequest() {
				inject = false
			}
		} else if sessionID == 0 {
			inject = false
		}

		if inject {
			logutil.Logger(ctx).Info(
				"[failpoint] injected RPC error on send", zap.Stringer("type", req.Type),
				zap.Stringer("req", req.Req.(fmt.Stringer)), zap.Stringer("ctx", &req.Context),
			)
			injectFailOnSend = true
			err = errors.New("injected RPC error on send")
		}
	}

	if !injectFailOnSend {
		start := time.Now()
		resp, err = s.client.SendRequest(ctx, sendToAddr, req, timeout)
		rpcDuration := time.Since(start)
		if s.replicaSelector != nil {
			recordAttemptedTime(s.replicaSelector, rpcDuration)
		}
		// Record timecost of external requests on related Store when `ReplicaReadMode == "PreferLeader"`.
		if rpcCtx.Store != nil && req.ReplicaReadType == kv.ReplicaReadPreferLeader && !util.IsInternalRequest(req.RequestSource) {
			rpcCtx.Store.healthStatus.recordClientSideSlowScoreStat(rpcDuration)
		}
		if s.Stats != nil {
			s.Stats.RecordRPCRuntimeStats(req.Type, rpcDuration)
			if val, fpErr := util.EvalFailpoint("tikvStoreRespResult"); fpErr == nil {
				if val.(bool) {
					if req.Type == tikvrpc.CmdCop && bo.GetTotalSleep() == 0 {
						return &tikvrpc.Response{
							Resp: &coprocessor.Response{RegionError: &errorpb.Error{EpochNotMatch: &errorpb.EpochNotMatch{}}},
						}, false, nil
					}
				}
			}
		}

		if val, e := util.EvalFailpoint("rpcFailOnRecv"); e == nil {
			inject := true
			// Optional filters
			if s, ok := val.(string); ok {
				if s == "greengc" && !req.IsGreenGCRequest() {
					inject = false
				} else if s == "write" && !req.IsTxnWriteRequest() {
					inject = false
				}
			} else if sessionID == 0 {
				inject = false
			}

			if inject {
				logutil.Logger(ctx).Info(
					"[failpoint] injected RPC error on recv", zap.Stringer("type", req.Type),
					zap.Stringer("req", req.Req.(fmt.Stringer)), zap.Stringer("ctx", &req.Context),
					zap.Error(err), zap.String("extra response info", fetchRespInfo(resp)),
				)
				err = errors.New("injected RPC error on recv")
				resp = nil
			}
		}

		if val, e := util.EvalFailpoint("rpcContextCancelErr"); e == nil {
			if val.(bool) {
				ctx1, cancel := context.WithCancel(context.Background())
				cancel()
				<-ctx1.Done()
				ctx = ctx1
				err = ctx.Err()
				resp = nil
			}
		}

		if _, e := util.EvalFailpoint("onRPCFinishedHook"); e == nil {
			if hook := bo.GetCtx().Value("onRPCFinishedHook"); hook != nil {
				h := hook.(func(*tikvrpc.Request, *tikvrpc.Response, error) (*tikvrpc.Response, error))
				resp, err = h(req, resp, err)
			}
		}
	}

	if rpcCtx.ProxyStore != nil {
		fromStore := strconv.FormatUint(rpcCtx.ProxyStore.storeID, 10)
		toStore := strconv.FormatUint(rpcCtx.Store.storeID, 10)
		result := "ok"
		if err != nil {
			result = "fail"
		}
		metrics.TiKVForwardRequestCounter.WithLabelValues(fromStore, toStore, req.Type.String(), result).Inc()
	}

	if err != nil {
		s.rpcError = err
		if s.Stats != nil {
			errStr := getErrMsg(err)
			s.Stats.RecordRPCErrorStats(errStr)
			s.recordRPCAccessInfo(req, rpcCtx, errStr)
		}
		// Because in rpc logic, context.Cancel() will be transferred to rpcContext.Cancel error. For rpcContext cancel,
		// we need to retry the request. But for context cancel active, for example, limitExec gets the required rows,
		// we shouldn't retry the request, it will go to backoff and hang in retry logic.
		if ctx.Err() != nil && errors.Cause(ctx.Err()) == context.Canceled {
			metrics.TiKVRPCErrorCounter.WithLabelValues("context-canceled", storeIDLabel(rpcCtx)).Inc()
			return nil, false, errors.WithStack(ctx.Err())
		}

		if val, e := util.EvalFailpoint("noRetryOnRpcError"); e == nil {
			if val.(bool) {
				return nil, false, err
			}
		}
		if e := s.onSendFail(bo, rpcCtx, req, err); e != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	return
}

func storeIDLabel(rpcCtx *RPCContext) string {
	if rpcCtx != nil && rpcCtx.Store != nil {
		return strconv.FormatUint(rpcCtx.Store.storeID, 10)
	}
	return "nil"
}

func (s *RegionRequestSender) getStoreToken(st *Store, limit int64) error {
	// Checking limit is not thread safe, preferring this for avoiding load in loop.
	count := st.tokenCount.Load()
	if count < limit {
		// Adding tokenCount is no thread safe, preferring this for avoiding check in loop.
		st.tokenCount.Add(1)
		return nil
	}
	metrics.TiKVStoreLimitErrorCounter.WithLabelValues(st.addr, strconv.FormatUint(st.storeID, 10)).Inc()
	return errors.WithStack(&tikverr.ErrTokenLimit{StoreID: st.storeID})
}

func (s *RegionRequestSender) releaseStoreToken(st *Store) {
	count := st.tokenCount.Load()
	// Decreasing tokenCount is no thread safe, preferring this for avoiding check in loop.
	if count > 0 {
		st.tokenCount.Add(-1)
		return
	}
	logutil.BgLogger().Warn("release store token failed, count equals to 0")
}

func (s *RegionRequestSender) onSendFail(bo *retry.Backoffer, ctx *RPCContext, req *tikvrpc.Request, err error) error {
	if span := opentracing.SpanFromContext(bo.GetCtx()); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("regionRequest.onSendFail", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		bo.SetCtx(opentracing.ContextWithSpan(bo.GetCtx(), span1))
	}
	storeLabel := storeIDLabel(ctx)
	// If it failed because the context is cancelled by ourself, don't retry.
	if errors.Cause(err) == context.Canceled {
		metrics.TiKVRPCErrorCounter.WithLabelValues("context-canceled", storeLabel).Inc()
		return errors.WithStack(err)
	} else if LoadShuttingDown() > 0 {
		metrics.TiKVRPCErrorCounter.WithLabelValues("shutting-down", storeLabel).Inc()
		return errors.WithStack(tikverr.ErrTiDBShuttingDown)
	} else if isCauseByDeadlineExceeded(err) {
		if s.replicaSelector != nil && s.replicaSelector.onReadReqConfigurableTimeout(req) {
			errLabel := "read-timeout-" + strconv.FormatUint(req.MaxExecutionDurationMs, 10) + "ms"
			metrics.TiKVRPCErrorCounter.WithLabelValues(errLabel, storeLabel).Inc()
			return nil
		}
	}
	if status.Code(errors.Cause(err)) == codes.Canceled {
		select {
		case <-bo.GetCtx().Done():
			metrics.TiKVRPCErrorCounter.WithLabelValues("grpc-canceled", storeLabel).Inc()
			return errors.WithStack(err)
		default:
			// If we don't cancel, but the error code is Canceled, it may be canceled by keepalive or gRPC remote.
			// For the case of canceled by keepalive, we need to re-establish the connection, otherwise following requests will always fail.
			// Canceled by gRPC remote may happen when tikv is killed and exiting.
			// Close the connection, backoff, and retry.
			logutil.Logger(bo.GetCtx()).Warn("receive a grpc cancel signal", zap.Error(err))
			var errConn *client.ErrConn
			if errors.As(err, &errConn) {
				if ext := s.getClientExt(); ext != nil {
					ext.CloseAddrVer(errConn.Addr, errConn.Ver)
				} else {
					s.client.CloseAddr(errConn.Addr)
				}
			}
		}
	}
	if errStr := getErrMsg(err); len(errStr) > 0 {
		metrics.TiKVRPCErrorCounter.WithLabelValues(getErrMsg(err), storeLabel).Inc()
	} else {
		metrics.TiKVRPCErrorCounter.WithLabelValues("unknown", storeLabel).Inc()
	}

	if ctx.Store != nil && ctx.Store.storeType == tikvrpc.TiFlashCompute {
		s.regionCache.InvalidateTiFlashComputeStoresIfGRPCError(err)
	} else if ctx.Meta != nil {
		if s.replicaSelector != nil {
			s.replicaSelector.onSendFailure(bo, err)
		} else {
			s.regionCache.OnSendFail(bo, ctx, s.NeedReloadRegion(ctx), err)
		}
	}

	// don't need to retry for ResourceGroup error
	if errors.Is(err, pderr.ErrClientResourceGroupThrottled) {
		return err
	}
	if errors.Is(err, pderr.ErrClientResourceGroupConfigUnavailable) {
		return err
	}
	var errGetResourceGroup *pderr.ErrClientGetResourceGroup
	if errors.As(err, &errGetResourceGroup) {
		return err
	}

	// Retry on send request failure when it's not canceled.
	// When a store is not available, the leader of related region should be elected quickly.
	// TODO: the number of retry time should be limited:since region may be unavailable
	// when some unrecoverable disaster happened.
	if ctx.Store != nil && ctx.Store.storeType.IsTiFlashRelatedType() {
		err = bo.Backoff(
			retry.BoTiFlashRPC,
			errors.Errorf("send tiflash request error: %v, ctx: %v, try next peer later", err, ctx),
		)
	} else {
		err = bo.Backoff(
			retry.BoTiKVRPC,
			errors.Errorf("send tikv request error: %v, ctx: %v, try next peer later", err, ctx),
		)
	}
	return err
}

func isCauseByDeadlineExceeded(err error) bool {
	causeErr := errors.Cause(err)
	return causeErr == context.DeadlineExceeded || // batch-client will return this error.
		status.Code(causeErr) == codes.DeadlineExceeded // when batch-client is disabled, grpc will return this error.
}

// NeedReloadRegion checks is all peers has sent failed, if so need reload.
func (s *RegionRequestSender) NeedReloadRegion(ctx *RPCContext) (need bool) {
	if s.failStoreIDs == nil {
		s.failStoreIDs = make(map[uint64]struct{})
	}
	if s.failProxyStoreIDs == nil {
		s.failProxyStoreIDs = make(map[uint64]struct{})
	}
	s.failStoreIDs[ctx.Store.storeID] = struct{}{}
	if ctx.ProxyStore != nil {
		s.failProxyStoreIDs[ctx.ProxyStore.storeID] = struct{}{}
	}

	if ctx.AccessMode == tiKVOnly && len(s.failStoreIDs)+len(s.failProxyStoreIDs) >= ctx.TiKVNum {
		need = true
	} else if ctx.AccessMode == tiFlashOnly && len(s.failStoreIDs) >= len(ctx.Meta.Peers)-ctx.TiKVNum {
		need = true
	} else if len(s.failStoreIDs)+len(s.failProxyStoreIDs) >= len(ctx.Meta.Peers) {
		need = true
	}

	if need {
		s.failStoreIDs = nil
		s.failProxyStoreIDs = nil
	}
	return
}

// regionErrorToLogging constructs the logging content with extra information like returned leader peer id.
func regionErrorToLogging(e *errorpb.Error, errLabel string) string {
	str := errLabel
	if e.GetNotLeader() != nil {
		notLeader := e.GetNotLeader()
		if notLeader.GetLeader() != nil {
			str = fmt.Sprintf("%v_with_leader_%v", str, notLeader.GetLeader().GetId())
		} else {
			str = fmt.Sprintf("%v_with_no_leader", str)
		}
	}
	return str
}

func regionErrorToLabel(e *errorpb.Error) string {
	if e.GetNotLeader() != nil {
		return "not_leader"
	} else if e.GetRegionNotFound() != nil {
		return "region_not_found"
	} else if e.GetKeyNotInRegion() != nil {
		return "key_not_in_region"
	} else if e.GetEpochNotMatch() != nil {
		return "epoch_not_match"
	} else if e.GetServerIsBusy() != nil {
		if strings.Contains(e.GetServerIsBusy().GetReason(), "deadline is exceeded") {
			return "deadline_exceeded"
		}
		return "server_is_busy"
	} else if e.GetStaleCommand() != nil {
		return "stale_command"
	} else if e.GetStoreNotMatch() != nil {
		return "store_not_match"
	} else if e.GetRaftEntryTooLarge() != nil {
		return "raft_entry_too_large"
	} else if e.GetMaxTimestampNotSynced() != nil {
		return "max_timestamp_not_synced"
	} else if e.GetReadIndexNotReady() != nil {
		return "read_index_not_ready"
	} else if e.GetProposalInMergingMode() != nil {
		return "proposal_in_merging_mode"
	} else if e.GetDataIsNotReady() != nil {
		return "data_is_not_ready"
	} else if e.GetRegionNotInitialized() != nil {
		return "region_not_initialized"
	} else if e.GetDiskFull() != nil {
		return "disk_full"
	} else if e.GetRecoveryInProgress() != nil {
		return "recovery_in_progress"
	} else if e.GetFlashbackInProgress() != nil {
		return "flashback_in_progress"
	} else if e.GetFlashbackNotPrepared() != nil {
		return "flashback_not_prepared"
	} else if e.GetIsWitness() != nil {
		return "peer_is_witness"
	} else if isDeadlineExceeded(e) {
		return "deadline_exceeded"
	} else if e.GetMismatchPeerId() != nil {
		return "mismatch_peer_id"
	} else if e.GetBucketVersionNotMatch() != nil {
		return "bucket_version_not_match"
	}
	return "unknown"
}

func isDeadlineExceeded(e *errorpb.Error) bool {
	return strings.Contains(e.GetMessage(), "Deadline is exceeded")
}

func (s *RegionRequestSender) onRegionError(
	bo *retry.Backoffer, ctx *RPCContext, req *tikvrpc.Request, regionErr *errorpb.Error,
) (shouldRetry bool, err error) {
	if span := opentracing.SpanFromContext(bo.GetCtx()); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("tikv.onRegionError", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		bo.SetCtx(opentracing.ContextWithSpan(bo.GetCtx(), span1))
	}

	regionErrLabel := regionErrorToLabel(regionErr)
	metrics.TiKVRegionErrorCounter.WithLabelValues(regionErrLabel, storeIDLabel(ctx)).Inc()
	if s.Stats != nil {
		s.Stats.RecordRPCErrorStats(regionErrLabel)
		s.recordRPCAccessInfo(req, ctx, regionErrorToLogging(regionErr, regionErrLabel))
	}

	// NOTE: Please add the region error handler in the same order of errorpb.Error.
	if notLeader := regionErr.GetNotLeader(); notLeader != nil {
		// Retry if error is `NotLeader`.
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `NotLeader` retry later",
			zap.String("notLeader", notLeader.String()),
			zap.String("ctx", ctx.String()),
		)

		if s.replicaSelector != nil {
			return s.replicaSelector.onNotLeader(bo, ctx, notLeader)
		} else if notLeader.GetLeader() == nil {
			// The peer doesn't know who is the current leader. Generally it's because
			// the Raft group is in an election, but it's possible that the peer is
			// isolated and removed from the Raft group. So it's necessary to reload
			// the region from PD.
			s.regionCache.InvalidateCachedRegionWithReason(ctx.Region, NoLeader)
			if err = bo.Backoff(
				retry.BoRegionScheduling,
				errors.Errorf("not leader: %v, ctx: %v", notLeader, ctx),
			); err != nil {
				return false, err
			}
			return false, nil
		} else {
			// don't backoff if a new leader is returned.
			s.regionCache.UpdateLeader(ctx.Region, notLeader.GetLeader(), ctx.AccessIdx)
			return true, nil
		}
	}

	// Retry it when tikv disk full happens.
	if diskFull := regionErr.GetDiskFull(); diskFull != nil {
		if err = bo.Backoff(
			retry.BoTiKVDiskFull,
			errors.Errorf("tikv disk full: %v ctx: %v", diskFull.String(), ctx.String()),
		); err != nil {
			return false, nil
		}
		return true, nil
	}

	if regionErr.GetRecoveryInProgress() != nil {
		s.regionCache.InvalidateCachedRegion(ctx.Region)
		logutil.Logger(bo.GetCtx()).Debug("tikv reports `RecoveryInProgress`", zap.Stringer("ctx", ctx))
		err = bo.Backoff(retry.BoRegionRecoveryInProgress, errors.Errorf("region recovery in progress, ctx: %v", ctx))
		if err != nil {
			return false, err
		}
		return false, nil
	}

	if regionErr.GetIsWitness() != nil {
		s.regionCache.InvalidateCachedRegion(ctx.Region)
		logutil.Logger(bo.GetCtx()).Debug("tikv reports `IsWitness`", zap.Stringer("ctx", ctx))
		err = bo.Backoff(retry.BoIsWitness, errors.Errorf("is witness, ctx: %v", ctx))
		if err != nil {
			return false, err
		}
		return false, nil
	}

	// Since we expect that the workload should be stopped during the flashback progress,
	// if a request meets the FlashbackInProgress error, it should stop retrying immediately
	// to avoid unnecessary backoff and potential unexpected data status to the user.
	if flashbackInProgress := regionErr.GetFlashbackInProgress(); flashbackInProgress != nil {
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `FlashbackInProgress`",
			zap.Stringer("req", req),
			zap.Stringer("ctx", ctx),
		)
		if req != nil {
			if s.onFlashbackInProgressRegionError(ctx, req) {
				return true, nil
			}
		}
		return false, errors.Errorf(
			"region %d is in flashback progress, FlashbackStartTS is %d",
			flashbackInProgress.GetRegionId(), flashbackInProgress.GetFlashbackStartTs(),
		)
	}
	// This error means a second-phase flashback request is sent to a region that is not
	// prepared for the flashback before, it should stop retrying immediately to avoid
	// unnecessary backoff.
	if regionErr.GetFlashbackNotPrepared() != nil {
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `FlashbackNotPrepared`",
			zap.Stringer("req", req),
			zap.Stringer("ctx", ctx),
		)
		return false, errors.Errorf(
			"region %d is not prepared for the flashback",
			regionErr.GetFlashbackNotPrepared().GetRegionId(),
		)
	}

	// This peer is removed from the region. Invalidate the region since it's too stale.
	// if the region error is from follower, can we mark the peer unavailable and reload region asynchronously?
	if regionErr.GetRegionNotFound() != nil {
		s.regionCache.InvalidateCachedRegion(ctx.Region)
		return false, nil
	}

	if regionErr.GetKeyNotInRegion() != nil {
		logutil.Logger(bo.GetCtx()).Error("tikv reports `KeyNotInRegion`", zap.Stringer("req", req), zap.Stringer("ctx", ctx))
		s.regionCache.InvalidateCachedRegion(ctx.Region)
		return false, nil
	}

	if epochNotMatch := regionErr.GetEpochNotMatch(); epochNotMatch != nil {
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `EpochNotMatch` retry later",
			zap.Stringer("EpochNotMatch", epochNotMatch),
			zap.Stringer("ctx", ctx),
		)
		retry, err := s.regionCache.OnRegionEpochNotMatch(bo, ctx, epochNotMatch.CurrentRegions)
		if !retry && s.replicaSelector != nil {
			s.replicaSelector.invalidateRegion()
		}
		return retry, err
	}

	if bucketVersionNotMatch := regionErr.GetBucketVersionNotMatch(); bucketVersionNotMatch != nil {
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `BucketVersionNotMatch` retry later",
			zap.Uint64("latest bucket version", bucketVersionNotMatch.GetVersion()),
			zap.Uint64("request bucket version", ctx.BucketVersion),
			zap.Stringer("ctx", ctx),
		)
		// bucket version is not match, we should split this cop request again.
		s.regionCache.OnBucketVersionNotMatch(ctx, bucketVersionNotMatch.Version, bucketVersionNotMatch.Keys)
		return false, nil
	}

	if serverIsBusy := regionErr.GetServerIsBusy(); serverIsBusy != nil {
		if s.replicaSelector != nil && strings.Contains(serverIsBusy.GetReason(), "deadline is exceeded") {
			if s.replicaSelector.onReadReqConfigurableTimeout(req) {
				return true, nil
			}
		}
		if s.replicaSelector != nil {
			return s.replicaSelector.onServerIsBusy(bo, ctx, req, serverIsBusy)
		}
		logutil.Logger(bo.GetCtx()).Warn(
			"tikv reports `ServerIsBusy` retry later",
			zap.String("reason", regionErr.GetServerIsBusy().GetReason()),
			zap.Stringer("ctx", ctx),
		)
		if ctx != nil && ctx.Store != nil && ctx.Store.storeType.IsTiFlashRelatedType() {
			err = bo.Backoff(retry.BoTiFlashServerBusy, errors.Errorf("server is busy, ctx: %v", ctx))
		} else {
			err = bo.Backoff(retry.BoTiKVServerBusy, errors.Errorf("server is busy, ctx: %v", ctx))
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}

	// StaleCommand error indicates the request is sent to the old leader and its term is changed.
	// We can't know whether the request is committed or not, so it's an undetermined error too,
	// but we don't handle it now.
	if regionErr.GetStaleCommand() != nil {
		logutil.Logger(bo.GetCtx()).Debug("tikv reports `StaleCommand`", zap.Stringer("ctx", ctx))
		if s.replicaSelector != nil {
			// Needn't backoff because the new leader should be elected soon
			// and the replicaSelector will try the next peer.
		} else {
			err = bo.Backoff(retry.BoStaleCmd, errors.Errorf("stale command, ctx: %v", ctx))
			if err != nil {
				return false, err
			}
		}
		return true, nil
	}

	if storeNotMatch := regionErr.GetStoreNotMatch(); storeNotMatch != nil {
		// store not match
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `StoreNotMatch` retry later",
			zap.Stringer("storeNotMatch", storeNotMatch),
			zap.Stringer("ctx", ctx),
		)
		s.regionCache.stores.markStoreNeedCheck(ctx.Store)
		s.regionCache.InvalidateCachedRegion(ctx.Region)
		// It's possible the address of store is not changed but the DNS resolves to a different address in k8s environment,
		// so we always reconnect in this case.
		s.client.CloseAddr(ctx.Addr)
		return false, nil
	}

	if regionErr.GetRaftEntryTooLarge() != nil {
		logutil.Logger(bo.GetCtx()).Warn("tikv reports `RaftEntryTooLarge`", zap.Stringer("ctx", ctx))
		return false, errors.New(regionErr.String())
	}

	if regionErr.GetMaxTimestampNotSynced() != nil {
		logutil.Logger(bo.GetCtx()).Debug("tikv reports `MaxTimestampNotSynced`", zap.Stringer("ctx", ctx))
		err = bo.Backoff(retry.BoMaxTsNotSynced, errors.Errorf("max timestamp not synced, ctx: %v", ctx))
		if err != nil {
			return false, err
		}
		return true, nil
	}

	// A read request may be sent to a peer which has not been initialized yet, we should retry in this case.
	if regionErr.GetRegionNotInitialized() != nil {
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `RegionNotInitialized` retry later",
			zap.Uint64("store-id", ctx.Store.storeID),
			zap.Uint64("region-id", regionErr.GetRegionNotInitialized().GetRegionId()),
			zap.Stringer("ctx", ctx),
		)
		err = bo.Backoff(retry.BoMaxRegionNotInitialized, errors.Errorf("region not initialized"))
		if err != nil {
			return false, err
		}
		return true, nil
	}

	// The read-index can't be handled timely because the region is splitting or merging.
	if regionErr.GetReadIndexNotReady() != nil {
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `ReadIndexNotReady` retry later",
			zap.Uint64("store-id", ctx.Store.storeID),
			zap.Uint64("region-id", regionErr.GetRegionNotInitialized().GetRegionId()),
			zap.Stringer("ctx", ctx),
		)
		// The region can't provide service until split or merge finished, so backoff.
		err = bo.Backoff(retry.BoRegionScheduling, errors.Errorf("read index not ready, ctx: %v", ctx))
		if err != nil {
			return false, err
		}
		return true, nil
	}

	if regionErr.GetProposalInMergingMode() != nil {
		logutil.Logger(bo.GetCtx()).Debug("tikv reports `ProposalInMergingMode`", zap.Stringer("ctx", ctx))
		// The region is merging and it can't provide service until merge finished, so backoff.
		err = bo.Backoff(retry.BoRegionScheduling, errors.Errorf("region is merging, ctx: %v", ctx))
		if err != nil {
			return false, err
		}
		return true, nil
	}

	// A stale read request may be sent to a peer which the data is not ready yet, we should retry in this case.
	// This error is specific to stale read and the target replica is randomly selected. If the request is sent
	// to the leader, the data must be ready, so we don't backoff here.
	if regionErr.GetDataIsNotReady() != nil {
		logutil.Logger(bo.GetCtx()).Debug(
			"tikv reports `DataIsNotReady` retry later",
			zap.Uint64("store-id", ctx.Store.storeID),
			zap.Uint64("peer-id", regionErr.GetDataIsNotReady().GetPeerId()),
			zap.Uint64("region-id", regionErr.GetDataIsNotReady().GetRegionId()),
			zap.Uint64("safe-ts", regionErr.GetDataIsNotReady().GetSafeTs()),
			zap.Stringer("ctx", ctx),
		)
		if s.replicaSelector != nil {
			s.replicaSelector.onDataIsNotReady()
		}
		if !req.IsGlobalStaleRead() {
			// only backoff local stale reads as global should retry immediately against the leader as a normal read
			err = bo.Backoff(retry.BoMaxDataNotReady, errors.New("data is not ready"))
			if err != nil {
				return false, err
			}
		}
		return true, nil
	}

	if isDeadlineExceeded(regionErr) && s.replicaSelector != nil && s.replicaSelector.onReadReqConfigurableTimeout(req) {
		return true, nil
	}

	if mismatch := regionErr.GetMismatchPeerId(); mismatch != nil {
		logutil.Logger(bo.GetCtx()).Warn(
			"tikv reports `MismatchPeerId`, invalidate region cache",
			zap.Uint64("req peer id", mismatch.GetRequestPeerId()),
			zap.Uint64("store peer id", mismatch.GetStorePeerId()),
		)
		if s.replicaSelector != nil {
			s.replicaSelector.invalidateRegion()
		}
		return false, nil
	}

	logutil.Logger(bo.GetCtx()).Debug(
		"tikv reports region failed",
		zap.Stringer("regionErr", regionErr),
		zap.Stringer("ctx", ctx),
	)

	if s.replicaSelector != nil {
		// Try the next replica.
		return true, nil
	}

	// When the request is sent to TiDB, there is no region in the request, so the region id will be 0.
	// So when region id is 0, there is no business with region cache.
	if ctx.Region.id != 0 {
		s.regionCache.InvalidateCachedRegion(ctx.Region)
	}
	// For other errors, we only drop cache here.
	// Because caller may need to re-split the request.
	return false, nil
}

func (s *RegionRequestSender) onFlashbackInProgressRegionError(ctx *RPCContext, req *tikvrpc.Request) bool {
	switch selector := s.replicaSelector.(type) {
	case *replicaSelector:
		// if the failure is caused by replica read, we can retry it with leader safely.
		if ctx.contextPatcher.replicaRead != nil && *ctx.contextPatcher.replicaRead {
			req.BusyThresholdMs = 0
			selector.busyThreshold = 0
			ctx.contextPatcher.replicaRead = nil
			ctx.contextPatcher.busyThreshold = nil
			return true
		}
		if req.ReplicaReadType.IsFollowerRead() {
			s.replicaSelector = nil
			req.ReplicaReadType = kv.ReplicaReadLeader
			return true
		}
	case *replicaSelectorV2:
		return selector.onFlashbackInProgress(ctx, req)
	}
	return false
}

type staleReadMetricsCollector struct {
}

func (s *staleReadMetricsCollector) onReq(req *tikvrpc.Request, isLocalTraffic bool) {
	size := 0
	switch req.Type {
	case tikvrpc.CmdGet:
		size = req.Get().Size()
	case tikvrpc.CmdBatchGet:
		size = req.BatchGet().Size()
	case tikvrpc.CmdScan:
		size = req.Scan().Size()
	case tikvrpc.CmdCop:
		size = req.Cop().Size()
	default:
		// ignore non-read requests
		return
	}
	size += req.Context.Size()
	if isLocalTraffic {
		metrics.StaleReadLocalOutBytes.Add(float64(size))
		metrics.StaleReadReqLocalCounter.Add(1)
	} else {
		metrics.StaleReadRemoteOutBytes.Add(float64(size))
		metrics.StaleReadReqCrossZoneCounter.Add(1)
	}
}

func (s *staleReadMetricsCollector) onResp(tp tikvrpc.CmdType, resp *tikvrpc.Response, isLocalTraffic bool) {
	size := 0
	switch tp {
	case tikvrpc.CmdGet:
		size += resp.Resp.(*kvrpcpb.GetResponse).Size()
	case tikvrpc.CmdBatchGet:
		size += resp.Resp.(*kvrpcpb.BatchGetResponse).Size()
	case tikvrpc.CmdScan:
		size += resp.Resp.(*kvrpcpb.ScanResponse).Size()
	case tikvrpc.CmdCop:
		size += resp.Resp.(*coprocessor.Response).Size()
	default:
		// ignore non-read requests
		return
	}
	if isLocalTraffic {
		metrics.StaleReadLocalInBytes.Add(float64(size))
	} else {
		metrics.StaleReadRemoteInBytes.Add(float64(size))
	}
}

func (s *replicaSelector) replicaType(rpcCtx *RPCContext) string {
	leaderIdx := -1
	switch v := s.state.(type) {
	case *accessKnownLeader:
		return "leader"
	case *tryFollower:
		return "follower"
	case *accessFollower:
		leaderIdx = int(v.leaderIdx)
	case *tryIdleReplica:
		leaderIdx = int(v.leaderIdx)
	}
	if leaderIdx > -1 && rpcCtx != nil && rpcCtx.Peer != nil {
		for idx, replica := range s.replicas {
			if replica.peer.Id == rpcCtx.Peer.Id {
				if idx == leaderIdx {
					return "leader"
				}
				return "follower"
			}
		}
	}
	return "unknown"
}

func patchRequestSource(req *tikvrpc.Request, replicaType string) {
	var sb strings.Builder
	defer func() {
		// TiKV does the limit control by the last part of the request source.
		sb.WriteByte('_')
		sb.WriteString(req.InputRequestSource)
		req.RequestSource = sb.String()
	}()

	if req.IsRetryRequest {
		sb.WriteString("retry_")
		sb.WriteString(req.ReadType)
		sb.WriteByte('_')
		sb.WriteString(replicaType)
		return
	}
	if req.StaleRead {
		req.ReadType = "stale_" + replicaType
	} else {
		req.ReadType = replicaType
	}
	sb.WriteString(req.ReadType)
}

func recordAttemptedTime(s ReplicaSelector, duration time.Duration) {
	if targetReplica := s.targetReplica(); targetReplica != nil {
		targetReplica.attemptedTime += duration
	}
	if proxyReplica := s.proxyReplica(); proxyReplica != nil {
		proxyReplica.attemptedTime += duration
	}
}

// canFastRetry returns true if the request can be sent to next replica.
func (s *replicaSelector) canFastRetry() bool {
	accessLeader, ok := s.state.(*accessKnownLeader)
	if ok && isLeaderCandidate(s.replicas[accessLeader.leaderIdx]) {
		// If leader is still candidate, the request will be sent to leader again,
		// so don't skip since the leader is still busy.
		return false
	}
	return true
}

type backoffArgs struct {
	cfg *retry.Config
	err error
}

// addPendingBackoff adds pending backoff for the store.
func (s *baseReplicaSelector) addPendingBackoff(store *Store, cfg *retry.Config, err error) {
	storeId := uint64(0)
	if store != nil {
		storeId = store.storeID
	}
	if s.pendingBackoffs == nil {
		s.pendingBackoffs = make(map[uint64]*backoffArgs)
	}
	s.pendingBackoffs[storeId] = &backoffArgs{cfg, err}
}

// backoffOnRetry apply pending backoff on the store when retry in this store.
func (s *baseReplicaSelector) backoffOnRetry(store *Store, bo *retry.Backoffer) error {
	storeId := uint64(0)
	if store != nil {
		storeId = store.storeID
	}
	args, ok := s.pendingBackoffs[storeId]
	if !ok {
		return nil
	}
	delete(s.pendingBackoffs, storeId)
	return bo.Backoff(args.cfg, args.err)
}

// backoffOnNoCandidate apply the largest base pending backoff when no candidate.
func (s *baseReplicaSelector) backoffOnNoCandidate(bo *retry.Backoffer) error {
	var args *backoffArgs
	for _, pbo := range s.pendingBackoffs {
		if args == nil || args.cfg.Base() < pbo.cfg.Base() {
			args = pbo
		}
	}
	if args == nil {
		return nil
	}
	return bo.Backoff(args.cfg, args.err)
}
