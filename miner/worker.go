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
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"gopkg.in/fatih/set.v0"
	"strconv"
)

const (
	resultQueueSize  = 10
	miningLogAtDepth = 5

	// txChanSize is the size of channel listening to TxPreEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10
	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10
)

// Agent can register themself with the worker
type Agent interface {
	Work() chan<- *Work
	SetReturnCh(chan<- *Result)
	Stop()
	Start()
	GetHashRate() int64
}

// Work is the workers current environment and holds
// all of the current state information
type Work struct {
	config *params.ChainConfig
	signer types.Signer  //签名者

	state     *state.StateDB // apply state changes here  //状态数据库
	ancestors *set.Set       // ancestor set (used for checking uncle parent validity) //祖先集合，用来检查祖先是否有效
	family    *set.Set       // family set (used for checking uncle invalidity) //家族集合，用来检查祖先的无效性
	uncles    *set.Set       // uncle set  //uncles集合
	tcount    int            // tx count in cycle //这个周期的交易数量

	Block *types.Block // the new block  //新的区块

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt

	createdAt time.Time
}

type Result struct {
	Work  *Work
	Block *types.Block
}

// worker is the main object which takes care of applying messages to the new state
type worker struct {
	config *params.ChainConfig
	engine consensus.Engine    //consensus:共识

	mu sync.Mutex

	// update loop
	mux          *event.TypeMux
	txCh         chan core.TxPreEvent  //用来接受txPool里面的交易的通道
	txSub        event.Subscription    //用来接受txPool里面的交易的订阅器
	chainHeadCh  chan core.ChainHeadEvent  //用来接受区块头的通道
	chainHeadSub event.Subscription
	chainSideCh  chan core.ChainSideEvent  //用来接受一个区块链从规范区块链移出的通道
	chainSideSub event.Subscription
	wg           sync.WaitGroup

	agents map[Agent]struct{} //Agent: struct{}
	recv   chan *Result   //agent会把结果发送到这个通道(挖到的结果写入returnCh通道)

	eth     Backend
	chain   *core.BlockChain  //区块链
	proc    core.Validator
	chainDb ethdb.Database

	coinbase common.Address
	extra    []byte

	currentMu sync.Mutex
	current   *Work

	uncleMu        sync.Mutex
	possibleUncles map[common.Hash]*types.Block

	unconfirmed *unconfirmedBlocks // set of locally mined blocks pending canonicalness confirmations

	// atomic status counters
	mining int32
	atWork int32
}

//new worker
func newWorker(config *params.ChainConfig, engine consensus.Engine, coinbase common.Address, eth Backend, mux *event.TypeMux) *worker {
	worker := &worker{
		config:         config,
		engine:         engine,
		eth:            eth,
		mux:            mux,
		txCh:           make(chan core.TxPreEvent, txChanSize),
		chainHeadCh:    make(chan core.ChainHeadEvent, chainHeadChanSize), //创建通道
		chainSideCh:    make(chan core.ChainSideEvent, chainSideChanSize),
		chainDb:        eth.ChainDb(),
		recv:           make(chan *Result, resultQueueSize), //10 数据结构为Result的通道
		chain:          eth.BlockChain(),
		proc:           eth.BlockChain().Validator(),
		possibleUncles: make(map[common.Hash]*types.Block),
		coinbase:       coinbase,
		agents:         make(map[Agent]struct{}),
		unconfirmed:    newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth), //miningLogAtDepth: 5
	}
	// Subscribe TxPreEvent for tx pool
	worker.txSub = eth.TxPool().SubscribeTxPreEvent(worker.txCh) //有TxPreEvent时,txCh会被写入值
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh) //与BlockChain的事件关联起来
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh) //与BlockChain的事件关联起来
	go worker.update() //TxPool的事件、BlockChain的事件

	go worker.wait()//打包一个块成功后被触发
	worker.commitNewWork() //打包准备工作

	return worker
}

func (self *worker) setEtherbase(addr common.Address) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.coinbase = addr
}

func (self *worker) setExtra(extra []byte) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.extra = extra
}

//获得work的block and state
func (self *worker) pending() (*types.Block, *state.StateDB) {
	self.currentMu.Lock()
	defer self.currentMu.Unlock()

	if atomic.LoadInt32(&self.mining) == 0 {
		return types.NewBlock(
			self.current.header,
			self.current.txs,
			nil,
			self.current.receipts,
		), self.current.state.Copy()
	}
	return self.current.Block, self.current.state.Copy()
}

//获得work的block
func (self *worker) pendingBlock() *types.Block {
	self.currentMu.Lock()
	defer self.currentMu.Unlock()

	if atomic.LoadInt32(&self.mining) == 0 { //没有在挖矿，创建一个block
		return types.NewBlock(
			self.current.header,
			self.current.txs,
			nil,
			self.current.receipts,
		)
	}
	return self.current.Block
}

//启动Agent
func (self *worker) start() {
	self.mu.Lock()
	defer self.mu.Unlock()

	atomic.StoreInt32(&self.mining, 1) //worker's mining to be 1
	fmt.Printf("test worker.go start self.agents.length = %s\n", strconv.Itoa(len(self.agents)))
	// spin up(旋转起来) agents
	for agent := range self.agents {
		fmt.Printf("worker.go start() self.agents %s\n", "")
		agent.Start() //start agent
	}
}

//stop agents
func (self *worker) stop() {
	self.wg.Wait()

	self.mu.Lock()
	defer self.mu.Unlock()
	if atomic.LoadInt32(&self.mining) == 1 {
		for agent := range self.agents {
			agent.Stop()
		}
	}
	atomic.StoreInt32(&self.mining, 0)
	atomic.StoreInt32(&self.atWork, 0)
}

//将worker和Agent联系起来
func (self *worker) register(agent Agent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.agents[agent] = struct{}{} //map[Agent]struct{}
	agent.SetReturnCh(self.recv) //worker.recv通道被赋值给Agent.returnCh通道
}


//agents删除agent
func (self *worker) unregister(agent Agent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	delete(self.agents, agent)
	agent.Stop()
}

//TxPool的事件、BlockChain的事件
//1.ChainHeadEvent: commitNewWork
//2.ChainSideEvent
//3.TxPreEvent: 新交易到来, 执行交易
//监听自身的各个chan的变化，处理各个chan的值
func (self *worker) update() {
	defer self.txSub.Unsubscribe() //取消事件关注
	defer self.chainHeadSub.Unsubscribe() //取消事件关注
	defer self.chainSideSub.Unsubscribe() //取消事件关注

	for {
		// A real event arrived, process interesting content
		select {
		// Handle ChainHeadEvent
		//ChainHeadEvent是指区块链中已经加入了一个新的区块作为整个链的链头，这时worker的回应是立即开始准备挖掘下一个新区块(也是够忙的)
		case <-self.chainHeadCh:
			self.commitNewWork() //开始准备挖掘下一个新区块

		// Handle ChainSideEvent
		//指区块链中加入了一个新区块作为当前链头的旁支，worker会把这个区块收纳进possibleUncles[]数组，作为下一个挖掘新区块可能的Uncle之一
		case ev := <-self.chainSideCh:
			self.uncleMu.Lock()
			self.possibleUncles[ev.Block.Hash()] = ev.Block //hash:Block
			self.uncleMu.Unlock()

		// Handle TxPreEvent
		//指的是一个新的交易tx被加入了TxPool，这时如果worker没有处于挖掘中，那么就去执行这个tx，并把它收纳进Work.txs数组，为下次挖掘新区块备用。
		case ev := <-self.txCh:
			fmt.Printf("test worker.go update handle TxPreEvent %s\n", "")
			// Apply transaction to the pending state if we're not mining
			if atomic.LoadInt32(&self.mining) == 0 {//如果不在mining，就放到pending池子里面
				self.currentMu.Lock()
				acc, _ := types.Sender(self.current.signer, ev.Tx)
				txs := map[common.Address]types.Transactions{acc: {ev.Tx}}
				txset := types.NewTransactionsByPriceAndNonce(self.current.signer, txs)

				self.current.commitTransactions(self.mux, txset, self.chain, self.coinbase) //执行交易
				self.currentMu.Unlock()
			} else {
				// If we're mining, but nothing is being processed, wake on new transactions
				if self.config.Clique != nil && self.config.Clique.Period == 0 {
					self.commitNewWork()
				}
			}

			// System stopped
		case <-self.txSub.Err():
			return
		case <-self.chainHeadSub.Err():
			return
		case <-self.chainSideSub.Err():
			return
		}
	}
}

//处理挖矿结果
//处理数据结构为Result的通道
func (self *worker) wait() {
	for {//死循环
		mustCommitNewWork := true
		for result := range self.recv { //得到挖矿结果(agent.go的mine方法) 能够不断的读取channel里面的数据，直到该channel被显式的关闭
			atomic.AddInt32(&self.atWork, -1)

			if result == nil {
				fmt.Printf("%s\n", "test worker.go wait result is nil")
				continue
			}

			//挖矿后返回的block
			block := result.Block
			work := result.Work

			//update BlockHash
			// Update the block hash in all logs since it is now available and not when the
			// receipt/log of individual transactions were created.
			for _, r := range work.receipts {
				for _, l := range r.Logs {
					l.BlockHash = block.Hash()
				}
			}
			for _, log := range work.state.Logs() {
				log.BlockHash = block.Hash()
			}


			stat, err := self.chain.WriteBlockWithState(block, work.receipts, work.state)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			// check if canon block and write transactions
			if stat == core.CanonStatTy {
				// implicit by posting ChainHeadEvent
				mustCommitNewWork = false
			}
			// Broadcast the block and announce(宣布) chain insertion event
			self.mux.Post(core.NewMinedBlockEvent{Block: block})
			var (
				events []interface{}
				logs   = work.state.Logs()
			)
			events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
			if stat == core.CanonStatTy {//stat如果是CanonStatTy,则event是ChainHeadEvent
				events = append(events, core.ChainHeadEvent{Block: block})
			}
			//ChainEvent/ChainHeadEvent
			self.chain.PostChainEvents(events, logs)

			//block插入到unconfirmed
			// Insert the block into the set of pending ones to wait for confirmations(确认)
			self.unconfirmed.Insert(block.NumberU64(), block.Hash())

			if mustCommitNewWork {
				self.commitNewWork()
			}
		}
	}
}

//通知Agent，work准备好，可以开始挖矿了
// push sends a new work task to currently live miner agents.
func (self *worker) push(work *Work) {
	fmt.Println("test worker.go push %s\n", "")
	if atomic.LoadInt32(&self.mining) != 1 { //如果没有在挖矿, 返回
		fmt.Println("test worker.go push return for not mining %s\n", "")
		return
	}
	for agent := range self.agents {
		atomic.AddInt32(&self.atWork, 1)
		if ch := agent.Work(); ch != nil { //agent.Work()得到cpuAgent的workCh通道
			ch <- work //往workCh通道写入work,这时agent的update()方法因为监听了workCh而被触发
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (self *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	state, err := self.chain.StateAt(parent.Root())
	if err != nil {
		return err
	}
	work := &Work{
		config:    self.config,
		signer:    types.NewEIP155Signer(self.config.ChainId),
		state:     state,
		ancestors: set.New(),
		family:    set.New(),
		uncles:    set.New(),
		header:    header,
		createdAt: time.Now(),
	}

	// when 08 is processed ancestors contain 07 (quick block)
	//ancestor: parent6->parent5->parent4->parent3->parent2->parent1->parent
	for _, ancestor := range self.chain.GetBlocksFromHash(parent.Hash(), 7) { //ancestor:祖先
		for _, uncle := range ancestor.Uncles() {
			work.family.Add(uncle.Hash())
		}
		work.family.Add(ancestor.Hash())
		work.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return errors so they can be removed
	work.tcount = 0
	self.current = work
	return nil
}

func (self *worker) commitNewWork() {
	//mu\uncleMu\currentMu
	self.mu.Lock()
	defer self.mu.Unlock()
	self.uncleMu.Lock()
	defer self.uncleMu.Unlock()
	self.currentMu.Lock()
	defer self.currentMu.Unlock()

	tstart := time.Now() //当前时间
	parent := self.chain.CurrentBlock()//当前块

	tstamp := tstart.Unix() //当前时间的时间戳

	//当前块的时间 > 当前时间的时间戳
	if parent.Time().Cmp(new(big.Int).SetInt64(tstamp)) >= 0 { //If parent's time > current time?
		tstamp = parent.Time().Int64() + 1 //当前块的时间 + 1
	}
	// this will ensure we're not going off too far in the future
	if now := time.Now().Unix(); tstamp > now+1 { //大超过1
		wait := time.Duration(tstamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}

	num := parent.Number() //当前块的高度
	fmt.Printf("test worker.go commitNewWork num = %s\n ", num)
	//1.构造Header:ParentHash,Number,GasLimit,Extra,Time
	header := &types.Header{
		ParentHash: parent.Hash(),             //parent hash
		Number:     num.Add(num, common.Big1), //new block's height
		GasLimit:   core.CalcGasLimit(parent), //GasLimit
		Extra:      self.extra, //miner 创建后，通过config读取extra后设置到worker
		Time:       big.NewInt(tstamp),
	}

	//自身正在挖矿时, 才设置header.Coinbase的值
	// Only set the coinbase if we are mining (avoid spurious(伪) block rewards)
	//Coinbase is the block’s beneficiary address
	if atomic.LoadInt32(&self.mining) == 1 { //mining has been update to be 1 in method worker.start()
		header.Coinbase = self.coinbase //worker.coinbase is the param coinbase(get value in method Start(coinbase common.Address))
	}
	fmt.Printf("test worker.go commitNewWork 1 header.Coinbase = %x\n", header.Coinbase)
	//ethash just update header's Difficulty
	if err := self.engine.Prepare(self.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}
	fmt.Printf("test worker.go commitNewWork 2 header.Coinbase = %x\n", header.Coinbase)
	fmt.Printf("test worker.go commitNewWork  self.coinbase = %x\n", self.coinbase)
	//update header.Extra
	//If we are care about TheDAO hard-fork check whether to override the extra-data or not
	if daoBlock := self.config.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange) //daoBlock + 10

		//If header.Number >= daoBlock && header.Number < limit
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			if self.config.DAOForkSupport { //support dao fork
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra) //header.Extra to be 0x64616f2d686172642d666f726b
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) { //reset header.Extra to be blank
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}

	// Could potentially happen if starting to mine in an odd state.
	err := self.makeCurrent(parent, header) //worker.current
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}
	// Create the current work task and check any fork transitions needed
	work := self.current
	if self.config.DAOForkSupport && self.config.DAOForkBlock != nil && self.config.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(work.state)
	}
	pending, err := self.eth.TxPool().Pending() //获得pending的交易
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return
	}
	txs := types.NewTransactionsByPriceAndNonce(self.current.signer, pending)


	work.commitTransactions(self.mux, txs, self.chain, self.coinbase) //如果没有tx，就break回来

	// compute uncles for the new block.
	var (
		uncles    []*types.Header
		badUncles []common.Hash
	)
	for hash, uncle := range self.possibleUncles {
		if len(uncles) == 2 {
			break
		}
		if err := self.commitUncle(work, uncle.Header()); err != nil {
			log.Trace("Bad uncle found and will be removed", "hash", hash)
			log.Trace(fmt.Sprint(uncle))

			badUncles = append(badUncles, hash)
		} else {
			log.Debug("Committing new uncle to block", "hash", hash)
			uncles = append(uncles, uncle.Header())
		}
	}
	for _, hash := range badUncles {
		delete(self.possibleUncles, hash)
	}
	// Create the new block to seal with the consensus engine
	if work.Block, err = self.engine.Finalize(self.chain, header, work.state, work.txs, uncles, work.receipts); err != nil {//create a block
		log.Error("Failed to finalize block for sealing", "err", err)
		return
	}
	// We only care about logging if we're actually mining.
	if atomic.LoadInt32(&self.mining) == 1 { //如果现在正在挖矿
		log.Info("Commit new mining work", "number", work.Block.Number(), "txs", work.tcount, "uncles", len(uncles), "elapsed", common.PrettyDuration(time.Since(tstart)))
		self.unconfirmed.Shift(work.Block.NumberU64() - 1)
	}
	self.push(work) //通知Agent，work准备好，可以开始挖矿了
}

func (self *worker) commitUncle(work *Work, uncle *types.Header) error {
	hash := uncle.Hash()
	if work.uncles.Has(hash) {
		return fmt.Errorf("uncle not unique")
	}
	if !work.ancestors.Has(uncle.ParentHash) {
		return fmt.Errorf("uncle's parent unknown (%x)", uncle.ParentHash[0:4])
	}
	if work.family.Has(hash) {
		return fmt.Errorf("uncle already in family (%x)", hash)
	}
	work.uncles.Add(uncle.Hash())
	return nil
}


//如果没有tx，就break回来
func (env *Work) commitTransactions(mux *event.TypeMux, txs *types.TransactionsByPriceAndNonce, bc *core.BlockChain, coinbase common.Address) {
	gp := new(core.GasPool).AddGas(env.header.GasLimit)
	fmt.Printf(" %s\n", " ")
	fmt.Printf(" %s\n", " ")
	fmt.Printf("test worker.go commitTransactions coinbase = %x\n", coinbase)
	fmt.Printf("test worker.go commitTransactions gp.gas = %s\n", gp.Gas())
	fmt.Printf(" %s\n", " ")
	fmt.Printf(" %s\n", " ")
	var coalescedLogs []*types.Log

	for {
		// If we don't have enough gas for any further transactions then we're done
		if gp.Gas() < params.TxGas { //21000
			log.Trace("Not enough gas for further transactions", "gp", gp)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek() //t.heads[0]
		if tx == nil {
			fmt.Printf("test worker.go commitTransactions tx is nil = %s\n", " ")
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		fmt.Printf("test worker.go commitTransactions from = %x\n", from)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !env.config.IsEIP155(env.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", env.config.EIP155Block)

			txs.Pop() //丢弃
			continue
		}
		// Start executing the transaction
		env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount) //设置state.StateDB的thash,bhash,txIndex

		err, logs := env.commitTransaction(tx, bc, coinbase, gp)
		fmt.Printf("test worker.go commitTransactions err = %s\n", err)
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if len(coalescedLogs) > 0 || env.tcount > 0 {
		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs)) //coalesced合并
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go func(logs []*types.Log, tcount int) {
			if len(logs) > 0 {
				mux.Post(core.PendingLogsEvent{Logs: logs}) //PendingLogsEvent
			}
			if tcount > 0 {
				mux.Post(core.PendingStateEvent{}) //PendingStateEvent
			}
		}(cpy, env.tcount)
	}
}

func (env *Work) commitTransaction(tx *types.Transaction, bc *core.BlockChain, coinbase common.Address, gp *core.GasPool) (error, []*types.Log) {
	snap := env.state.Snapshot()

	receipt, _, err := core.ApplyTransaction(env.config, bc, &coinbase, gp, env.state, env.header, tx, &env.header.GasUsed, vm.Config{})
	if err != nil {
		env.state.RevertToSnapshot(snap)
		return err, nil
	}
	env.txs = append(env.txs, tx) //tx放入transaction数组txs
	env.receipts = append(env.receipts, receipt)//receipt放入Receipt数组receipts

	return nil, receipt.Logs
}
