// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"math/big"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// miningLogAtDepth is the number of confirmations before logging successful mining.
	miningLogAtDepth = 7

	// minRecommitInterval is the minimal time interval to recreate the mining block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// maxRecommitInterval is the maximum time interval to recreate the mining block with
	// any newly arrived transactions.
	//bencq+
	maxRecommitInterval = 3 * time.Second
	//bencq-

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	//bencq+
	staleThreshold = 1
	//bencq-
)

// environment is the worker's current environment and holds all of the current state information.
type environment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)
	uncles    mapset.Set     // uncle set
	tcount    int            // tx count in cycle
	gasPool   *core.GasPool  // available gas used to pack transactions

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts  []*types.Receipt
	state     *state.StateDB
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt *int32
	noempty   bool
	timestamp int64
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config      *Config
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	eth         Backend
	chain       *core.BlockChain

	//bencq+
	lockHelper *LockHelper
	//bencq-

	// Feeds
	pendingLogsFeed event.Feed

	// Subscriptions
	mux          *event.TypeMux
	txsCh        chan core.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription
	chainSideCh  chan core.ChainSideEvent
	chainSideSub event.Subscription

	// Channels
	newWorkCh          chan *newWorkReq
	taskCh             chan *task
	resultCh           chan *types.Block
	startCh            chan struct{}
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration
	resubmitAdjustCh   chan *intervalAdjust

	current      *environment                 // An environment for current running cycle.
	localUncles  map[common.Hash]*types.Block // A set of side blocks generated locally as the possible uncle blocks.
	remoteUncles map[common.Hash]*types.Block // A set of side blocks as the possible uncle blocks.
	unconfirmed  *unconfirmedBlocks           // A set of locally mined blocks pending canonicalness confirmations.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu    sync.RWMutex // The lock used to protect the block snapshot and state snapshot
	snapshotBlock *types.Block
	snapshotState *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.
	newTxs  int32 // New arrival transaction count since last sealing work submitting.

	// noempty is the flag used to control whether the feature of pre-seal empty
	// block is enabled. The default value is false(pre-seal is enabled by default).
	// But in some special scenario the consensus engine will seal blocks instantaneously,
	// in this case this feature will add all empty blocks into canonical chain
	// non-stop and no real transaction will be included.
	noempty uint32

	// External functions
	isLocalBlock func(block *types.Block) bool // Function used to determine whether the specified block is mined by local miner.

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.
}

//bencq+

// ReqBody is a struct of request for json unmarshal
type ReqBody struct {
	ToBlkNumber uint64 `json:"toBlkNumber"`
}

// LockHelper is a struct for lock based POA
type LockHelper struct {
	serverMux *http.ServeMux

	lockVal *int32
	lockMu  sync.Mutex

	endpoints     []string
	endpointIndex int
	etherbases    []common.Address
	eb2endpts     map[common.Address]string
	overtime      int // unit: ms

}

func newLockHelper(config *Config, exBlkNumber uint64) *LockHelper {
	ret := &LockHelper{
		serverMux:     nil,
		lockVal:       new(int32),
		endpoints:     config.Endpts,
		endpointIndex: config.EndptsIndex,
		etherbases:    config.Etherbases,
		eb2endpts:     make(map[common.Address]string),
		overtime:      config.Overtime,
	}
	// log.Error("bencq: newLockHelper:", "lock: in", exBlkNumber)
	for ind := range ret.endpoints {
		ret.eb2endpts[ret.etherbases[ind]] = ret.endpoints[ind]
	}
	for k, v := range ret.eb2endpts {
		log.Error("bencq: newLockHelper:", "k", k, "v", v)
	}
	return ret
}

// handle functions here

func (w *worker) HandleGetLockFromFollower(resp http.ResponseWriter, req *http.Request) {

	bodyData, err := ioutil.ReadAll(req.Body)
	if err != nil {
		//log.Error("bencq: HandleGetLockFromFollower: ioutil.ReadAll", "err", err)
	}
	var reqBody ReqBody
	err = json.Unmarshal(bodyData, &reqBody)
	if err != nil {
		//log.Error("bencq: HandleGetLockFromFollower: json.Unmarshal", "err", err)
	}
	toBlkNumber := reqBody.ToBlkNumber

	if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
		resp.WriteHeader(http.StatusForbidden)
		// log.Error("bencq: HandleGrabLockFromFollower: bf lockMu", "resp.WriteHeader", http.StatusForbidden)
	} else {
		w.lockHelper.lockMu.Lock()
		if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
			resp.WriteHeader(http.StatusForbidden)
			// log.Error("bencq: HandleGetLockFromFollower: af lockMu", "resp.WriteHeader", http.StatusForbidden)
		} else {
			exBlkNumber := w.chain.CurrentHeader().Number.Uint64()
			if toBlkNumber != exBlkNumber+1 {
				resp.WriteHeader(http.StatusForbidden)
				// log.Error("bencq: HandleGetLockFromFollower: ", "lock: in", exBlkNumber, "resp.WriteHeader", http.StatusForbidden)
			} else {
				atomic.StoreInt32(w.lockHelper.lockVal, 1)
				// log.Error("bencq: HandleGetLockFromFollower: ", "tlock: in", RemoteLock, "resp.WriteHeader", http.StatusOK)
				resp.WriteHeader(http.StatusOK)
			}
		}
		w.lockHelper.lockMu.Unlock()
	}
}

func (w *worker) HandleGrabLockFromLeader(resp http.ResponseWriter, req *http.Request) {

	bodyData, err := ioutil.ReadAll(req.Body)
	if err != nil {
		//log.Error("bencq: HandleGrabLockFromLeader: ioutil.ReadAll", "err", err)
	}
	var reqBody ReqBody
	err = json.Unmarshal(bodyData, &reqBody)
	if err != nil {
		//log.Error("bencq: HandleGrabLockFromLeader: json.Unmarshal", "err", err)
	}
	toBlkNumber := reqBody.ToBlkNumber

	if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
		resp.WriteHeader(http.StatusForbidden)
		// log.Error("bencq: HandleGrabLockFromFollower: bf lockMu", "resp.WriteHeader", http.StatusForbidden)
	} else {
		w.lockHelper.lockMu.Lock()
		if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
			resp.WriteHeader(http.StatusForbidden)
			// log.Error("bencq: HandleGrabLockFromLeader: af lockMu", "resp.WriteHeader", http.StatusForbidden)
		} else {
			exBlkNumber := w.chain.CurrentHeader().Number.Uint64()
			if toBlkNumber != exBlkNumber+1 {
				resp.WriteHeader(http.StatusForbidden)
				// log.Error("bencq: HandleGrabLockFromLeader: ", "lock: in", exBlkNumber, "resp.WriteHeader", http.StatusForbidden)
			} else {
				atomic.StoreInt32(w.lockHelper.lockVal, 1)
				// log.Error("bencq: HandleGrabLockFromLeader: ", "tlock: in", RemoteLock, "resp.WriteHeader", http.StatusOK)
				resp.WriteHeader(http.StatusOK)
			}
		}
		w.lockHelper.lockMu.Unlock()
	}

}

func (w *worker) HandleGrabLockFromFollower(resp http.ResponseWriter, req *http.Request) {

	bodyData, err := ioutil.ReadAll(req.Body)
	if err != nil {
		//log.Error("bencq: HandleGrabLockFromFollower: ioutil.ReadAll", "err", err)
	}
	var reqBody ReqBody
	err = json.Unmarshal(bodyData, &reqBody)
	if err != nil {
		//log.Error("bencq: HandleGrabLockFromFollower: json.Unmarshal", "err", err)
	}
	toBlkNumber := reqBody.ToBlkNumber

	if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
		resp.WriteHeader(http.StatusForbidden)
		// log.Error("bencq: HandleGrabLockFromFollower: bf lockMu", "resp.WriteHeader", http.StatusForbidden)
	} else {
		w.lockHelper.lockMu.Lock()
		if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
			resp.WriteHeader(http.StatusForbidden)
			// log.Error("bencq: HandleGrabLockFromFollower: af lockMu", "resp.WriteHeader", http.StatusForbidden)
		} else {
			exBlkNumber := w.chain.CurrentHeader().Number.Uint64()
			if toBlkNumber != exBlkNumber+1 {
				resp.WriteHeader(http.StatusForbidden)
				// log.Error("bencq: HandleGrabLockFromFollower: ", "lock: in", exBlkNumber, "resp.WriteHeader", http.StatusForbidden)
			} else {
				atomic.StoreInt32(w.lockHelper.lockVal, 1)
				// log.Error("bencq: HandleGrabLockFromFollower: ", "tlock: in", RemoteLock, "resp.WriteHeader", http.StatusOK)
				resp.WriteHeader(http.StatusOK)
			}
		}
		w.lockHelper.lockMu.Unlock()
	}

}

func (w *worker) HandleReturnLock(resp http.ResponseWriter, req *http.Request) {

	bodyData, err := ioutil.ReadAll(req.Body)
	if err != nil {
		// log.Error("bencq: HandleReturnLock: ioutil.ReadAll", "err", err)
	}
	var reqBody ReqBody
	err = json.Unmarshal(bodyData, &reqBody)
	if err != nil {
		// log.Error("bencq: HandleReturnLock: json.Unmarshal", "err", err)
	}
	toBlkNumber := reqBody.ToBlkNumber

	if atomic.LoadInt32(w.lockHelper.lockVal) == 0 {
		resp.WriteHeader(http.StatusForbidden)
		// log.Error("bencq: HandleReturnLock: bf lockMu", "resp.WriteHeader", http.StatusForbidden)
	} else {
		w.lockHelper.lockMu.Lock()
		if atomic.LoadInt32(w.lockHelper.lockVal) == 0 {
			resp.WriteHeader(http.StatusForbidden)
			// log.Error("bencq: HandleReturnLock: af lockMu: ", "resp.WriteHeader", http.StatusForbidden)
		} else {
			exBlkNumber := w.chain.CurrentHeader().Number.Uint64()
			if toBlkNumber != exBlkNumber+1 {
				// log.Error("bencq: HandleReturnLock: ", "resp.WriteHeader", http.StatusForbidden)
				resp.WriteHeader(http.StatusForbidden)
			} else {
				atomic.StoreInt32(w.lockHelper.lockVal, 0)
				// log.Error("bencq: HandleReturnLock: tlock: out", "toBlkNumber", toBlkNumber, "exBlkNumber", exBlkNumber)
			}
		}
		w.lockHelper.lockMu.Unlock()
	}
}

func (w *worker) newLockServerMux() *http.ServeMux {
	//log.Error("bencq: newLockServerMux: ")
	rand.Seed(time.Now().UnixNano())
	sm := http.NewServeMux()

	sm.HandleFunc("/"+GetLockFromFollower, w.HandleGetLockFromFollower)
	sm.HandleFunc("/"+GrabLockFromLeader, w.HandleGrabLockFromLeader)
	sm.HandleFunc("/"+GrabLockFromFollower, w.HandleGrabLockFromFollower)
	sm.HandleFunc("/"+ReturnLock, w.HandleReturnLock)
	sm.HandleFunc("/", func(resp http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPost:
			resp.Write([]byte(http.MethodPost))
			resp.WriteHeader(http.StatusOK)
		case http.MethodGet:
			resp.Write([]byte(http.MethodGet))
			resp.WriteHeader(http.StatusOK)
		default:
			resp.Write([]byte("none method"))
			resp.WriteHeader(http.StatusOK)
		}
	})

	return sm
}

func (w *worker) overtimeResumeLock(message string) {
	// time.Sleep(time.Duration(w.config.Overtime) * time.Millisecond)
	// //log.Error("bencq: overtimeResumeLock:", "message", message)
	// if len(w.lockHelper.lockChan) == 0 {
	// 	//log.Error("bencq: overtimeResumelock: in1")
	// 	w.lockHelper.lockChan <- 1
	// }

}

//peilin
func (w *worker) insertResumeLockLoop() {
	for {
		insertNumber := <-w.chain.InsertEventChannel
		log.Error("bencq: insertResumeLockLoop: ", "insertNumber", insertNumber)
		exBlkNumber := w.chain.CurrentHeader().Number.Uint64()

		if atomic.LoadInt32(w.lockHelper.lockVal) == 0 {
			// log.Error("bencq: insertResumeLockLoop: bf lockMu: , lock: out&in", "exBlkNumber", exBlkNumber)
		} else {
			w.lockHelper.lockMu.Lock()
			if atomic.LoadInt32(w.lockHelper.lockVal) == 0 {
				// log.Error("bencq: insertResumeLockLoop: af lockMu: , lock: out&in", "exBlkNumber", exBlkNumber)
			} else {
				atomic.StoreInt32(w.lockHelper.lockVal, 0)
				log.Error("bencq: insertResumeLockLoop: tlock: out", "exBlkNumber", exBlkNumber)
			}
			w.lockHelper.lockMu.Unlock()
		}
	}
}

func (w *worker) lockReqest() bool {

	//log.Error("bencq: CommitNewWork", "toBlkNumber", header.Number.Uint64())

	//bencq+
	// ensure the consensus method is Clique rather than Ethash
	//log.Error("bencq:", "w.chainConfig.Clique", w.chainConfig.Clique)
	if w.isRunning() && w.chainConfig.Clique != nil {
		parent := w.chain.CurrentBlock()
		num := parent.Number()
		header := &types.Header{
			ParentHash: parent.Hash(),
			Number:     num.Add(num, common.Big1),
		}

		falseResume := func(exBlkNumber uint64) {
			atomic.StoreInt32(w.lockHelper.lockVal, 0)
			// log.Error("bencq: falseResume commitNewWork: tlock: out", "exBlkNumber", exBlkNumber)
		}
		exBlkNumber := parent.NumberU64()
		if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
			// log.Error("bencq: commitNewWork: bf lockMu: ", "exBlkNumber", exBlkNumber)
			return false
		} else {
			w.lockHelper.lockMu.Lock()
			defer w.lockHelper.lockMu.Unlock()
			if atomic.LoadInt32(w.lockHelper.lockVal) == 1 {
				// log.Error("bencq: commitNewWork: af lockMu: ", "exBlkNumber", exBlkNumber)
				return false
			} else {
				atomic.StoreInt32(w.lockHelper.lockVal, 1)

				toBlkNumber := header.Number.Uint64()
				// log.Error("bencq: commitNewWork lock: out", "toBlkNumber", toBlkNumber, "exBlkNumber", exBlkNumber)
				// log.Error("bencq: commitNewWork: ", "tlock: in", LocalLock, "toBlkNumber", toBlkNumber, "exBlkNumber", exBlkNumber)
				if toBlkNumber != exBlkNumber+1 {
					falseResume(exBlkNumber)
					return false
				}

				cE := w.chain.Engine()

				leader, signers, inturn, err := cE.(*clique.Clique).RetrieveInfo(w.chain, header, nil)
				//log.Error("bencq: RetrieveInfo result", "leader", leader, "signers", signers, "inturn", inturn, "err", err)
				//log.Error("bencq: after RetrieveInfo, check", "w.lockHelper.etherbases[w.lockHelper.endpointIndex]", w.lockHelper.etherbases[w.lockHelper.endpointIndex])
				if err != nil {
					// log.Error("bencq: Failed to execute func IsInturn", "err", err)
					falseResume(exBlkNumber)
					return false
				}
				if inturn {

					requestLocksSuc, approveSignerInds := w.requestLocks(signers, toBlkNumber)
					// log.Error("bencq:", "requestLocksSuc", requestLocksSuc)
					if !requestLocksSuc {
						w.returnLocks(signers, approveSignerInds, toBlkNumber)
						falseResume(exBlkNumber)
						return false
					}

				} else {

					grabLeaderSuc := w.grabLockFromLeaderNode(leader, toBlkNumber)
					// log.Error("bencq: ", "grabLeaderSuc", grabLeaderSuc)
					if !grabLeaderSuc {
						falseResume(exBlkNumber)
						return false
					}

					grabFollowersSuc, approveSignerInds := w.reqLockFromFollowerNodes(leader, signers, toBlkNumber)
					// log.Error("bencq: ", "grabFollowersSuc", grabFollowersSuc)
					if !grabFollowersSuc {
						w.returnLocks(signers, approveSignerInds, toBlkNumber)
						falseResume(exBlkNumber)
						return false
					}

				}
			}

		}
		return true
	}
	return false
	//bencq-

}

//bencq-

func newWorker(config *Config, chainConfig *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, isLocalBlock func(*types.Block) bool, init bool) *worker {
	worker := &worker{
		config:             config,
		chainConfig:        chainConfig,
		engine:             engine,
		eth:                eth,
		mux:                mux,
		chain:              eth.BlockChain(),
		lockHelper:         newLockHelper(config, eth.BlockChain().CurrentHeader().Number.Uint64()), //bencq+ lockServer provides the POA with lock operations
		isLocalBlock:       isLocalBlock,
		localUncles:        make(map[common.Hash]*types.Block),
		remoteUncles:       make(map[common.Hash]*types.Block),
		unconfirmed:        newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),
		pendingTasks:       make(map[common.Hash]*task),
		txsCh:              make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:        make(chan core.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:        make(chan core.ChainSideEvent, chainSideChanSize),
		newWorkCh:          make(chan *newWorkReq),
		taskCh:             make(chan *task),
		resultCh:           make(chan *types.Block, resultQueueSize),
		exitCh:             make(chan struct{}),
		startCh:            make(chan struct{}, 1),
		resubmitIntervalCh: make(chan time.Duration),
		resubmitAdjustCh:   make(chan *intervalAdjust, resubmitAdjustChanSize),
	}
	//bencq+
	//log.Error("bencq:", "worker.fullTaskHook", worker.fullTaskHook)
	//initialize lockHelper.serverMux

	worker.lockHelper.serverMux = worker.newLockServerMux()
	//log.Error("bencq:", "worker.lockHelper.endpoints", worker.lockHelper.endpoints, "worker.lockHelper.endpointIndex", worker.lockHelper.endpointIndex)

	addrs := strings.Split(worker.lockHelper.endpoints[worker.lockHelper.endpointIndex], ":")
	addr := ":" + addrs[len(addrs)-1]
	//log.Error("bencq:", "addr", addr)
	go http.ListenAndServe(addr, worker.lockHelper.serverMux) //bencq

	//peilin
	go worker.insertResumeLockLoop()
	//bencq-

	// Subscribe NewTxsEvent for tx pool
	worker.txsSub = eth.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := worker.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	//bencq+
	// recommit = time.Duration(chainConfig.Clique.Period) * time.Second
	//log.Error("bencq: newWorker: ", "recommit", recommit)
	//bencq-

	go worker.mainLoop()
	go worker.newWorkLoop(recommit, time.Duration(chainConfig.Clique.Period)*time.Second)
	go worker.resultLoop()
	go worker.taskLoop()

	//bencq: test
	// go func() {
	// 	for {
	// 		log.Error("bencq: test: ", "*worker.lockHelper.lockVal", *worker.lockHelper.lockVal)
	// 		worker.lockHelper.lockMu.Lock()
	// 		log.Error("bencq: test: inlock")
	// 		worker.lockHelper.lockMu.Unlock()
	// 		time.Sleep(time.Second)
	// 	}
	// }()

	// Submit first work to initialize pending state.
	if init {
		worker.startCh <- struct{}{}
	}
	return worker
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	w.resubmitIntervalCh <- interval
}

// disablePreseal disables pre-sealing mining feature
func (w *worker) disablePreseal() {
	atomic.StoreUint32(&w.noempty, 1)
}

// enablePreseal enables pre-sealing mining feature
func (w *worker) enablePreseal() {
	atomic.StoreUint32(&w.noempty, 0)
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	atomic.StoreInt32(&w.running, 1)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	if w.current != nil && w.current.state != nil {
		w.current.state.StopPrefetcher()
	}
	atomic.StoreInt32(&w.running, 0)
	close(w.exitCh)
}

// recalcRecommit recalculates the resubmitting interval upon feedback.
func recalcRecommit(minRecommit, prev time.Duration, target float64, inc bool) time.Duration {
	var (
		prevF = float64(prev.Nanoseconds())
		next  float64
	)
	if inc {
		next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
		max := float64(maxRecommitInterval.Nanoseconds())
		if next > max {
			next = max
		}
	} else {
		next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
		min := float64(minRecommit.Nanoseconds())
		if next < min {
			next = min
		}
	}
	return time.Duration(int64(next))
}

// newWorkLoop is a standalone goroutine to submit new mining work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration, liveInterval time.Duration) {
	var (
		interrupt   *int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of mining.
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C // discard the initial tick

	liveTimer := time.NewTimer(0)
	defer liveTimer.Stop()
	<-liveTimer.C

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(noempty bool, s int32) {
		// log.Error("bencq: commit:")
		res := w.lockReqest()
		if !res {
			delayT := liveInterval * time.Duration(rand.Intn(5)+2) / 10
			liveTimer.Reset(delayT)
			// timer.Reset(delayT + recommit)
			return
		}

		if interrupt != nil {
			atomic.StoreInt32(interrupt, s)
		}
		interrupt = new(int32)
		select {
		case w.newWorkCh <- &newWorkReq{interrupt: interrupt, noempty: noempty, timestamp: timestamp}:
		case <-w.exitCh:
			return
		}
		timer.Reset(recommit)
		atomic.StoreInt32(&w.newTxs, 0)
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().NumberU64())
			timestamp = time.Now().Unix()
			commit(false, commitInterruptNewHead)

		case head := <-w.chainHeadCh:
			// runtime.GC()
			//log.Error("bencq: <-w.chainHeadCh:", "head.Block.NumberU64()", head.Block.NumberU64())
			clearPending(head.Block.NumberU64())
			timestamp = time.Now().Unix()
			//bencq+
			commit(false, commitInterruptNewHead)
			//bencq-

		case <-liveTimer.C:
			clearPending(w.chain.CurrentBlock().NumberU64())
			timestamp = time.Now().Unix()

			if atomic.LoadInt32(&w.newTxs) == 0 {
				commit(false, commitInterruptNewHead)
			} else {
				//log.Error("bencq: <-timer.C commit true")
				commit(true, commitInterruptNewHead)
			}

		case <-timer.C:
			log.Error("bencq: <-timer.C:", "w.lockHelper.lockVal", atomic.LoadInt32(w.lockHelper.lockVal))
			// If mining is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() && (w.chainConfig.Clique == nil || w.chainConfig.Clique.Period > 0) {
				// Short circuit if no new transaction arrives.
				// bencq+
				// disable short circuit
				if atomic.LoadInt32(&w.newTxs) == 0 {
					timer.Reset(recommit)
					continue
				} else {
					//log.Error("bencq: <-timer.C commit true")
					commit(true, commitInterruptResubmit)
				}
				// bencq-

			}

		case interval := <-w.resubmitIntervalCh:
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}
			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case adjust := <-w.resubmitAdjustCh:
			// Adjust resubmit interval by feedback.
			if adjust.inc {
				before := recommit
				target := float64(recommit.Nanoseconds()) / adjust.ratio
				recommit = recalcRecommit(minRecommit, recommit, target, true)
				log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
			} else {
				before := recommit
				recommit = recalcRecommit(minRecommit, recommit, float64(minRecommit.Nanoseconds()), false)
				log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
			}

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is a standalone goroutine to regenerate the sealing task based on the received event.
func (w *worker) mainLoop() {
	defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	defer w.chainSideSub.Unsubscribe()

	for {
		select {
		case req := <-w.newWorkCh:
			w.commitNewWork(req.interrupt, req.noempty, req.timestamp)

		case ev := <-w.chainSideCh:
			//bencq+
			log.Error("bencq: mainLoop ev := <-w.chainSideCh:", "ev.Block.NumberU64()", ev.Block.NumberU64())
			//bencq-
			// Short circuit for duplicate side blocks
			if _, exist := w.localUncles[ev.Block.Hash()]; exist {
				continue
			}
			if _, exist := w.remoteUncles[ev.Block.Hash()]; exist {
				continue
			}
			// Add side block to possible uncle block set depending on the author.
			if w.isLocalBlock != nil && w.isLocalBlock(ev.Block) {
				w.localUncles[ev.Block.Hash()] = ev.Block
			} else {
				w.remoteUncles[ev.Block.Hash()] = ev.Block
			}
			// If our mining block contains less than 2 uncle blocks,
			// add the new uncle block if valid and regenerate a mining block.
			if w.isRunning() && w.current != nil && w.current.uncles.Cardinality() < 2 {
				start := time.Now()
				if err := w.commitUncle(w.current, ev.Block.Header()); err == nil {
					var uncles []*types.Header
					w.current.uncles.Each(func(item interface{}) bool {
						hash, ok := item.(common.Hash)
						if !ok {
							return false
						}
						uncle, exist := w.localUncles[hash]
						if !exist {
							uncle, exist = w.remoteUncles[hash]
						}
						if !exist {
							return false
						}
						uncles = append(uncles, uncle.Header())
						return false
					})
					w.commit(uncles, nil, true, start)
				}
			}

		case ev := <-w.txsCh:
			//bencq+
			// //log.Error("bencq: mainloop: for: ev := <-w.txsCh:")
			//bencq-
			// Apply transactions to the pending state if we're not mining.
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current mining block. These transactions will
			// be automatically eliminated.
			if !w.isRunning() && w.current != nil {
				// If block is already full, abort
				//log.Error("bencq: mainLoop: ev := <-w.txsCh: !w.isRunning() && w.current != nil: ")
				if gp := w.current.gasPool; gp != nil && gp.Gas() < params.TxGas {
					continue
				}
				w.mu.RLock()
				coinbase := w.coinbase
				w.mu.RUnlock()

				txs := make(map[common.Address]types.Transactions)
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(w.current.signer, tx)
					txs[acc] = append(txs[acc], tx)
				}
				txset := types.NewTransactionsByPriceAndNonce(w.current.signer, txs)
				tcount := w.current.tcount
				w.commitTransactions(txset, coinbase, nil)
				// Only update the snapshot if any new transactons were added
				// to the pending block
				if tcount != w.current.tcount {
					w.updateSnapshot()
				}
			} else {
				// Special case, if the consensus engine is 0 period clique(dev mode),
				// submit mining work here since all empty submission will be rejected
				// by clique. Of course the advance sealing(empty submission) is disabled.
				if w.chainConfig.Clique != nil && w.chainConfig.Clique.Period == 0 {
					w.commitNewWork(nil, true, time.Now().Unix())
				}
			}
			atomic.AddInt32(&w.newTxs, int32(len(ev.Txs)))

		// System stopped
		case <-w.exitCh:
			return
		case <-w.txsSub.Err():
			return
		case <-w.chainHeadSub.Err():
			return
		case <-w.chainSideSub.Err():
			return
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}
			w.pendingMu.Lock()
			w.pendingTasks[sealHash] = task
			w.pendingMu.Unlock()

			if err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	for {
		select {
		case block := <-w.resultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(task.receipts))
				logs     []*types.Log
			)
			for i, receipt := range task.receipts {
				// add block location fields
				receipt.BlockHash = hash
				receipt.BlockNumber = block.Number()
				receipt.TransactionIndex = uint(i)

				receipts[i] = new(types.Receipt)
				*receipts[i] = *receipt
				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				for _, log := range receipt.Logs {
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}
			// Commit block and state to database.
			_, err := w.chain.WriteBlockWithState(block, receipts, logs, task.state, true)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(task.createdAt)))

			//bencq+
			//log.Error("bencq: resultLoop: bf w.mux.Post")
			//bencq-

			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

			//bencq+
			//log.Error("bencq: resultLoop: af w.mux.Post")
			//bencq-

			// Insert the block into the set of pending ones to resultLoop for confirmations
			w.unconfirmed.Insert(block.NumberU64(), block.Hash())

			//bencq+
			//log.Error("bencq: resultLoop: bf")

			// exBlkNumber := block.NumberU64()
			// log.Error("bencq: resultLoop: tlock: out", "exBlkNumber", exBlkNumber)
			if atomic.LoadInt32(w.lockHelper.lockVal) == 0 {
				// log.Error("bencq: resultLoop: bf lockMu: , lock: out&in", "exBlkNumber", exBlkNumber)
			} else {
				w.lockHelper.lockMu.Lock()
				if atomic.LoadInt32(w.lockHelper.lockVal) == 0 {
					// log.Error("bencq: resultLoop: af lockMu: , lock: out&in", "exBlkNumber", exBlkNumber)
				} else {
					atomic.StoreInt32(w.lockHelper.lockVal, 0)
					// log.Error("bencq: resultLoop: tlock: out", "exBlkNumber", exBlkNumber)
				}
				w.lockHelper.lockMu.Unlock()
			}

			//log.Error("bencq: resultLoop: af")
			//bencq-

		case <-w.exitCh:
			return
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (w *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	// Retrieve the parent state to execute on top and start a prefetcher for
	// the miner to speed block sealing up a bit
	state, err := w.chain.StateAt(parent.Root())
	if err != nil {
		return err
	}
	state.StartPrefetcher("miner")

	env := &environment{
		signer:    types.MakeSigner(w.chainConfig, header.Number),
		state:     state,
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		uncles:    mapset.NewSet(),
		header:    header,
	}
	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range w.chain.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			env.family.Add(uncle.Hash())
		}
		env.family.Add(ancestor.Hash())
		env.ancestors.Add(ancestor.Hash())
	}
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0

	// Swap out the old work with the new one, terminating any leftover prefetcher
	// processes in the mean time and starting a new one.
	if w.current != nil && w.current.state != nil {
		w.current.state.StopPrefetcher()
	}
	w.current = env
	return nil
}

// commitUncle adds the given block to uncle block set, returns error if failed to add.
func (w *worker) commitUncle(env *environment, uncle *types.Header) error {
	hash := uncle.Hash()
	if env.uncles.Contains(hash) {
		return errors.New("uncle not unique")
	}
	if env.header.ParentHash == uncle.ParentHash {
		return errors.New("uncle is sibling")
	}
	if !env.ancestors.Contains(uncle.ParentHash) {
		return errors.New("uncle's parent unknown")
	}
	if env.family.Contains(hash) {
		return errors.New("uncle already included")
	}
	env.uncles.Add(uncle.Hash())
	return nil
}

// updateSnapshot updates pending snapshot block and state.
// Note this function assumes the current variable is thread safe.
func (w *worker) updateSnapshot() {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	var uncles []*types.Header
	w.current.uncles.Each(func(item interface{}) bool {
		hash, ok := item.(common.Hash)
		if !ok {
			return false
		}
		uncle, exist := w.localUncles[hash]
		if !exist {
			uncle, exist = w.remoteUncles[hash]
		}
		if !exist {
			return false
		}
		uncles = append(uncles, uncle.Header())
		return false
	})

	w.snapshotBlock = types.NewBlock(
		w.current.header,
		w.current.txs,
		uncles,
		w.current.receipts,
		trie.NewStackTrie(nil),
	)
	w.snapshotState = w.current.state.Copy()
}

func (w *worker) commitTransaction(tx *types.Transaction, coinbase common.Address) ([]*types.Log, error) {
	snap := w.current.state.Snapshot()

	receipt, err := core.ApplyTransaction(w.chainConfig, w.chain, &coinbase, w.current.gasPool, w.current.state, w.current.header, tx, &w.current.header.GasUsed, *w.chain.GetVMConfig())
	if err != nil {
		//log.Error("bencq: commitTransaction: bf RevertToSnapshot", "err", err)
		w.current.state.RevertToSnapshot(snap)
		//log.Error("bencq: commitTransaction: af RevertToSnapshot", "err", err)
		return nil, err
	}
	w.current.txs = append(w.current.txs, tx)
	w.current.receipts = append(w.current.receipts, receipt)

	return receipt.Logs, nil
}

func (w *worker) commitTransactions(txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32) bool {
	// Short circuit if current is nil
	if w.current == nil {
		return true
	}

	if w.current.gasPool == nil {
		w.current.gasPool = new(core.GasPool).AddGas(w.current.header.GasLimit)
	}

	var coalescedLogs []*types.Log

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the mining block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(w.current.header.GasLimit-w.current.gasPool.Gas()) / float64(w.current.header.GasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}
		// If we don't have enough gas for any further transactions then we're done
		if w.current.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", w.current.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(w.current.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.chainConfig.IsEIP155(w.current.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.chainConfig.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		w.current.state.Prepare(tx.Hash(), common.Hash{}, w.current.tcount)

		logs, err := w.commitTransaction(tx, coinbase)
		switch {
		case errors.Is(err, core.ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			//log.Error("bencq: bf core.ErrGasLimitReached, txs.Pop()")
			txs.Pop()
			//log.Error("bencq: af core.ErrGasLimitReached, txs.Pop()")

		case errors.Is(err, core.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			//log.Error("bencq: bf core.ErrNonceTooLow, txs.Shift()")
			txs.Shift()
			//log.Error("bencq: af core.ErrNonceTooLow, txs.Shift()")

		case errors.Is(err, core.ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			//log.Error("bencq: bf core.ErrNonceTooHigh, txs.Pop()")
			txs.Pop()
			//log.Error("bencq: bf core.ErrNonceTooHigh, txs.Pop()")

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			w.current.tcount++
			// //log.Error("bencq: bf err nil, txs.Shift()")
			txs.Shift()
			// //log.Error("bencq: bf err nil, txs.Shift()")

		case errors.Is(err, core.ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			log.Trace("Skipping unsupported transaction type", "sender", from, "type", tx.Type())
			//log.Error("bencq: bf core.ErrTxTypeNotSupported, txs.Pop()")
			txs.Pop()
			//log.Error("bencq: bf core.ErrTxTypeNotSupported, txs.Pop()")

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			//log.Error("bencq: bf err default, txs.Shift()")
			txs.Shift()
			//log.Error("bencq: bf err default, txs.Shift()")
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		w.pendingLogsFeed.Send(cpy)
	}
	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}
	return false
}

// commitNewWork generates several new sealing tasks based on the parent block.
func (w *worker) commitNewWork(interrupt *int32, noempty bool, timestamp int64) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	tstart := time.Now()
	parent := w.chain.CurrentBlock()

	if parent.Time() >= uint64(timestamp) {
		timestamp = int64(parent.Time() + 1)
	}
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, w.config.GasFloor, w.config.GasCeil),
		Extra:      w.extra,
		Time:       uint64(timestamp),
	}

	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		if w.coinbase == (common.Address{}) {
			log.Error("Refusing to mine without etherbase")
			return
		}
		header.Coinbase = w.coinbase
	}
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}
	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	if daoBlock := w.chainConfig.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			if w.chainConfig.DAOForkSupport {
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}
	// Could potentially happen if starting to mine in an odd state.
	err := w.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}
	// Create the current work task and check any fork transitions needed
	env := w.current
	if w.chainConfig.DAOForkSupport && w.chainConfig.DAOForkBlock != nil && w.chainConfig.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(env.state)
	}
	// Accumulate the uncles for the current block
	uncles := make([]*types.Header, 0, 2)
	commitUncles := func(blocks map[common.Hash]*types.Block) {
		// Clean up stale uncle blocks first
		for hash, uncle := range blocks {
			if uncle.NumberU64()+staleThreshold <= header.Number.Uint64() {
				delete(blocks, hash)
			}
		}
		for hash, uncle := range blocks {
			if len(uncles) == 2 {
				break
			}
			if err := w.commitUncle(env, uncle.Header()); err != nil {
				log.Trace("Possible uncle rejected", "hash", hash, "reason", err)
			} else {
				log.Debug("Committing new uncle to block", "hash", hash)
				uncles = append(uncles, uncle.Header())
			}
		}
	}
	// Prefer to locally generated uncle
	commitUncles(w.localUncles)
	commitUncles(w.remoteUncles)

	// Create an empty block based on temporary copied state for
	// sealing in advance without waiting block execution finished.
	//bencq+
	// disable commitnewWork in advance
	if !noempty && atomic.LoadUint32(&w.noempty) == 0 {
		//log.Error("bencq: commitNewWork sealing in advance", "num", header.Number.Uint64())
		w.commit(uncles, nil, false, tstart)
	}
	//bencq-

	// Fill the block with all available pending transactions.
	pending, err := w.eth.TxPool().Pending()
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return
	}

	// Short circuit if there is no available pending transactions.
	// But if we disable empty precommit already, ignore it. Since
	// empty block is necessary to keep the liveness of the network.
	//bencq+
	//disable short circuit
	if len(pending) == 0 && atomic.LoadUint32(&w.noempty) == 0 {
		//log.Error("bencq: commitNewWork short circuit", "num", header.Number.Uint64())
		w.updateSnapshot()
		return
	}
	//bencq-

	// Split the pending transactions into locals and remotes
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}
	//bencq+
	txCnt := len(localTxs) + len(remoteTxs)
	log.Error("bencq: commitNewWork", "txCnt", txCnt)
	//bencq-
	if len(localTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, localTxs)
		if w.commitTransactions(txs, w.coinbase, interrupt) {
			//log.Error("bencq: commitNewWork len(localTxs) > 0 w.commitTransactions")
			return
		}
	}
	if len(remoteTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, remoteTxs)
		if w.commitTransactions(txs, w.coinbase, interrupt) {
			//log.Error("bencq: commitNewWork len(remoteTxs) > 0 w.commitTransactions")
			return
		}
	}
	//log.Error("bencq: w.commit", "num", header.Number.Uint64())
	w.commit(uncles, w.fullTaskHook, true, tstart)
}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
func (w *worker) commit(uncles []*types.Header, interval func(), update bool, start time.Time) error {
	// Deep copy receipts here to avoid interaction between different tasks.
	receipts := copyReceipts(w.current.receipts)
	s := w.current.state.Copy()
	block, err := w.engine.FinalizeAndAssemble(w.chain, w.current.header, s, w.current.txs, uncles, receipts)
	if err != nil {
		return err
	}
	if w.isRunning() {
		if interval != nil {
			interval()
		}
		select {
		case w.taskCh <- &task{receipts: receipts, state: s, block: block, createdAt: time.Now()}:
			w.unconfirmed.Shift(block.NumberU64() - 1)
			log.Info("Commit new mining work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()),
				"uncles", len(uncles), "txs", w.current.tcount,
				"gas", block.GasUsed(), "fees", totalFees(block, receipts),
				"elapsed", common.PrettyDuration(time.Since(start)))

		case <-w.exitCh:
			log.Info("Worker has exited")
		}
	}
	if update {
		w.updateSnapshot()
	}
	return nil
}

// copyReceipts makes a deep copy of the given receipts.
func copyReceipts(receipts []*types.Receipt) []*types.Receipt {
	result := make([]*types.Receipt, len(receipts))
	for i, l := range receipts {
		cpy := *l
		result[i] = &cpy
	}
	return result
}

// postSideBlock fires a side chain event, only use it for testing.
func (w *worker) postSideBlock(event core.ChainSideEvent) {
	select {
	case w.chainSideCh <- event:
	case <-w.exitCh:
	}
}

// bencq+
func (w *worker) requestLockSync(endpoint string, toBlkNumber uint64) bool {

	// TODO: there may be some mistakes here

	url := "http://" + endpoint + "/" + GetLockFromFollower
	//log.Error("bencq: requestLockSync: ", "url", url, "toBlkNumber", toBlkNumber)

	reqBody := ReqBody{
		ToBlkNumber: toBlkNumber,
	}

	jsonStr, _ := json.Marshal(reqBody)

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonStr))

	ret := true
	if err != nil || resp.StatusCode != http.StatusOK {
		ret = false
	}
	//log.Error("bencq: requestLockSync: ", "ret", ret)
	return ret
}

func (w *worker) returnLocks(signers []common.Address, approveSignerInds []int, toBlkNumber uint64) bool {
	// log.Error("bencq: returnLocks: ", "approveSignerInds", approveSignerInds, "toBlkNumber", toBlkNumber)
	suc := true
	var wg sync.WaitGroup
	var mutex sync.Mutex
	wg.Add(len(approveSignerInds))
	for _, arrInd := range approveSignerInds {
		signer := signers[arrInd]
		if signer == w.lockHelper.etherbases[w.lockHelper.endpointIndex] {
			//do nothing
			wg.Done()
		} else {
			go func(signer common.Address) {
				mutex.Lock()
				defer mutex.Unlock()
				returnSuc := w.returnLock(signer, toBlkNumber)
				suc = suc && returnSuc
				wg.Done()
			}(signer)
		}
	}
	wg.Wait()
	// log.Error("bencq: returnLocks: ", "suc", suc)
	return suc
}

func (w *worker) returnLock(signer common.Address, toBlkNumber uint64) bool {
	if signer == w.lockHelper.etherbases[w.lockHelper.endpointIndex] {
		// log.Error("bencq: "+ReturnLock+": ", "leader == thisEb", true)
		return true
	}
	endpoint := w.getEndPoint(signer)
	if endpoint == "" {
		// log.Error("bencq: "+ReturnLock+": ", "endpoint", endpoint)
		return false
	}

	url := "http://" + endpoint + "/" + ReturnLock
	// log.Error("bencq: "+ReturnLock+": ", "url", url, "toBlkNumber", toBlkNumber)

	reqBody := ReqBody{
		ToBlkNumber: toBlkNumber,
	}

	jsonStr, _ := json.Marshal(reqBody)

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonStr))

	ret := true
	if err != nil || resp.StatusCode != http.StatusOK {
		// log.Error("bencq: "+ReturnLock+": ", "err", err)
		if resp != nil {
			// log.Error("bencq: "+ReturnLock+": ", "resp.StatusCode", resp.StatusCode)
		}
		ret = false
	}
	log.Error("bencq: "+ReturnLock+": ", "ret", ret)
	return ret
}

func (w *worker) requestLocks(signers []common.Address, toBlkNumber uint64) (bool, []int) {

	suc, approveSignerInds := w.reqLockFromFollowerNodes(common.Address{}, signers, toBlkNumber)
	//log.Error("bencq: requestLocks: ", "suc", suc)
	return suc, approveSignerInds
}

func (w *worker) getEndPoint(signer common.Address) string {
	addr, ok := w.lockHelper.eb2endpts[signer]
	log.Error("bencq: getEndPoint: ", "signer", signer, "addr", addr, "ok", ok)
	return addr
}

func (w *worker) grabLockFromLeaderNode(leader common.Address, toBlkNumber uint64) bool {
	if leader == w.lockHelper.etherbases[w.lockHelper.endpointIndex] {
		//log.Error("bencq: grabLockFromLeaderNode: ", "leader == thisEb", true)
		return true
	}
	endpoint := w.getEndPoint(leader)
	if endpoint == "" {
		//log.Error("bencq: grabLockFromLeaderNode: ", "endpoint", endpoint)
		return false
	}

	url := "http://" + endpoint + "/" + GrabLockFromLeader
	//log.Error("bencq: grabLockFromLeaderNode: ", "url", url, "toBlkNumber", toBlkNumber)

	reqBody := ReqBody{
		ToBlkNumber: toBlkNumber,
	}

	jsonStr, _ := json.Marshal(reqBody)

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonStr))

	ret := true
	if err != nil || resp.StatusCode != http.StatusOK {
		ret = false
	}
	//log.Error("bencq: grabLockFromLeaderNode: ", "ret", ret)
	return ret
}

func (w *worker) reqLockFromFollowerNodes(leader common.Address, signers []common.Address, toBlkNumber uint64) (bool, []int) {

	//log.Error("bencq: reqLockFromFollowerNodes")

	signersLen := len(signers)

	minF := signersLen / 2 // "+2" gets ceiling of (signersLen - 1) / 3
	minApproveCnt := minF + 1
	maxDisapproveCnt := signersLen - minApproveCnt

	//log.Error("bencq: reqLockFromFollowerNodes", "signersLen", signersLen, "minF", minF, "minApproveCnt", minApproveCnt, "maxDisapproveCnt", maxDisapproveCnt)

	approveSignerInds := make([]int, 0, minApproveCnt)
	disapproveSignerInds := make([]int, 0, maxDisapproveCnt+1)

	var wg sync.WaitGroup
	reqSucChan := make(chan bool, 1)
	closed := false
	var mutex sync.Mutex
	var randStartInd = rand.Intn(signersLen)
	for ind := range signers {
		wg.Add(1)
		arrInd := (ind + randStartInd) % signersLen
		signer := signers[arrInd]
		if signer == leader || signer == w.lockHelper.etherbases[w.lockHelper.endpointIndex] {
			mutex.Lock()
			approveSignerInds = append(approveSignerInds, arrInd)
			if len(approveSignerInds) >= minApproveCnt {
				if !closed {
					reqSucChan <- true
					closed = true
				}
				mutex.Unlock()
				wg.Done()
				break
			} else {
				mutex.Unlock()
				wg.Done()
				continue
			}
		} else {
			go func(signer common.Address) {
				endpt := w.getEndPoint(signer)
				reqSuc := w.requestLockSync(endpt, toBlkNumber)
				mutex.Lock()
				defer mutex.Unlock()
				defer wg.Done()
				if reqSuc {
					approveSignerInds = append(approveSignerInds, arrInd)
					if len(approveSignerInds) >= minApproveCnt {
						if !closed {
							reqSucChan <- true
							closed = true
						}
					}
				} else {
					disapproveSignerInds = append(disapproveSignerInds, arrInd)
					if len(disapproveSignerInds) > maxDisapproveCnt {
						if !closed {
							reqSucChan <- false
							closed = true
						}
					}
				}
			}(signer)
		}
	}
	suc := <-reqSucChan
	wg.Wait()
	//log.Error("bencq: reqLockFromFollowerNodes: ", "approveCnt", len(approveSignerInds), "disapproveCnt", len(disapproveSignerInds), "suc", suc)
	return suc, approveSignerInds
}

const (
	GetLockFromFollower  string = "getLockFromFollower"
	GrabLockFromLeader   string = "grabLockFromLeader"
	GrabLockFromFollower string = "grabLockFromFollower"
	ReturnLock           string = "returnLock"
	NoneLock             string = "none"
	LocalLock            string = "local"
	RemoteLock           string = "remote"
)

// bencq-

// totalFees computes total consumed fees in ETH. Block transactions and receipts have to have the same order.
func totalFees(block *types.Block, receipts []*types.Receipt) *big.Float {
	feesWei := new(big.Int)
	for i, tx := range block.Transactions() {
		feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), tx.GasPrice()))
	}
	return new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Ether)))
}
