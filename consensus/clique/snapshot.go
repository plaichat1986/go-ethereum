// Copyright 2017 The go-ethereum Authors
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

package clique

import (
	"bytes"
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	lru "github.com/hashicorp/golang-lru"
)

// Vote represents a single vote that an authorized signer made to modify the
// list of authorizations.
type Vote struct {
	//谁投的这张票
	Signer    common.Address `json:"signer"`    // Authorized signer that cast(投) this vote

	//投这张票时的block number
	Block     uint64         `json:"block"`     // Block number the vote was cast in(被投入) (expire old votes)

	//谁被投
	Address   common.Address `json:"address"`   // Account being voted on to change its authorization

	//加入还是被踢出
	Authorize bool           `json:"authorize"` // Whether to authorize or deauthorize the voted account
}

//Tally结构体用来记录投票数据，即某个(被投票)地址总共被投了多少票，新认证状态是什么。
// Tally is a simple vote tally to keep the current score of votes. Votes that
// go against the proposal aren't counted since it's equivalent to not voting.
//一个地址会不会有一个表示被踢出，一个表示被加入?
//已经加入的只能被踢出，不在的只能被加入,所以不会出现上面的情况？
type Tally struct {
	Authorize bool `json:"authorize"` // Whether the vote is about authorizing or kicking someone //加入还是被踢出
	Votes     int  `json:"votes"`     // Number of votes until now wanting to pass the proposal //票张数
}

// Snapshot is the state of the authorization(授权) voting at a given point in time.
type Snapshot struct {
	config   *params.CliqueConfig // Consensus engine parameters to fine tune behavior

	//最近块签名的缓存
	//与Clique.signatures是同一个值
	//Header.hash:common.Address(signer的地址)
	sigcache *lru.ARCCache        // Cache of recent block signatures to speed up ecrecover

	//创建快照时的block号
	Number  uint64                      `json:"number"`  // Block number where the snapshot was created

	//创建快照时的block hash
	Hash    common.Hash                 `json:"hash"`    // Block hash where the snapshot was created

	//此刻的授权的signers
	//难道被授权Authorize，值就为struct{}{}，否则不存在于Signers中
	//common.Address:struct{}{}
	//if tally.Authorize { //授权
	//snap.Signers[header.Coinbase] = struct{}{}
	Signers map[common.Address]struct{} `json:"signers"` // Set of authorized signers at this moment

	//用来记录最近担当过数字签名算法的signer的地址
	//number:common.Address
	Recents map[uint64]common.Address   `json:"recents"` // Set of recent signers for spam protections
	//所有投票数据
	Votes   []*Vote                     `json:"votes"`   // List of votes cast in chronological order 一个Vote对象表示一张记名投票
	//每个地址的被投票状况
	Tally   map[common.Address]Tally    `json:"tally"`   // Current vote tally(相符) to avoid recalculating
}

//只能用于genesis的snapshot的创建，因为没有给Recents赋值
// newSnapshot creates a new snapshot with the specified startup parameters. This
// method does not initialize the set of recent signers, so only ever use if for
// the genesis block.
func newSnapshot(config *params.CliqueConfig, sigcache *lru.ARCCache, number uint64, hash common.Hash, signers []common.Address) *Snapshot {
	snap := &Snapshot{
		config:   config,
		sigcache: sigcache,
		Number:   number,
		Hash:     hash,
		Signers:  make(map[common.Address]struct{}),
		Recents:  make(map[uint64]common.Address), //uint64:common.Address
		Tally:    make(map[common.Address]Tally),
	}
	for _, signer := range signers {
		snap.Signers[signer] = struct{}{} //赋值Snapshot的Signers
	}
	return snap
}

//从Clique.db数据库读取
// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *params.CliqueConfig, sigcache *lru.ARCCache, db ethdb.Database, hash common.Hash) (*Snapshot, error) {
	blob, err := db.Get(append([]byte("clique-"), hash[:]...)) //从ethdb.Database通过clique-hash获得blob; hash:创建snapshot时的block的header的hash
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {//数据库读取出的数据生成snapshot  反解析json
		return nil, err
	}
	snap.config = config  //将Clique的config赋值给snapshot的config
	snap.sigcache = sigcache //将Clique的sigcache赋值给snapshot的sigcache

	return snap, nil
}

//存入Clique.db,key: clique-Hash
// store inserts the snapshot into the database.
func (s *Snapshot) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append([]byte("clique-"), s.Hash[:]...), blob)
}

//Snapshot的深copy
// copy creates a deep copy of the snapshot, though not the individual votes.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{
		config:   s.config,
		sigcache: s.sigcache,
		Number:   s.Number,
		Hash:     s.Hash,
		Signers:  make(map[common.Address]struct{}),
		Recents:  make(map[uint64]common.Address),
		Votes:    make([]*Vote, len(s.Votes)),
		Tally:    make(map[common.Address]Tally),
	}

	//设置Signers
	for signer := range s.Signers {
		cpy.Signers[signer] = struct{}{}
	}

	//设置Recents
	for block, signer := range s.Recents { //number:common.Address
		cpy.Recents[block] = signer //Recents
	}

	//设置Tally
	for address, tally := range s.Tally {
		cpy.Tally[address] = tally  //Tally
	}

	copy(cpy.Votes, s.Votes) //数组可以这样copy

	return cpy
}

//验证投票是否有效:是signer可以踢出，不是signer可以被加入
// validVote returns whether it makes sense to cast the specified vote in the
// given snapshot context (e.g. don't try to add an already authorized signer).
func (s *Snapshot) validVote(address common.Address, authorize bool) bool {
	_, signer := s.Signers[address] //struct{}{} or nil
	return (signer && !authorize) || (!signer && authorize)
}

//添加一个新票进Snapshot.Tally
// cast(投) adds a new vote into the tally.
func (s *Snapshot) cast(address common.Address, authorize bool) bool {
	// Ensure the vote is meaningful
	if !s.validVote(address, authorize) { //有意义的
		return false
	}
	// Cast the vote into an existing or new tally
	if old, ok := s.Tally[address]; ok { //address: 被投票的地址
		old.Votes++
		s.Tally[address] = old
	} else {
		s.Tally[address] = Tally{Authorize: authorize, Votes: 1} //不存在，需要新建
	}
	return true
}

//从Snapshot.Tally删除之前的一张投票
// uncast removes a previously cast vote from the tally.
func (s *Snapshot) uncast(address common.Address, authorize bool) bool {
	// If there's no tally, it's a dangling(悬空) vote, just drop
	tally, ok := s.Tally[address] //Tally: map[common.Address]Tally
	if !ok {
		return false
	}
	// Ensure we only revert counted votes
	if tally.Authorize != authorize {//authorize需要一致
		return false
	}
	// Otherwise revert the vote
	if tally.Votes > 1 {
		tally.Votes-- //减少投票数目
		s.Tally[address] = tally
	} else {
		delete(s.Tally, address) //通过key从map删除数据
	}
	return true
}

//处理header包含的投票数据
// apply creates a new authorization snapshot by applying the given headers to
// the original one.
func (s *Snapshot) apply(headers []*types.Header) (*Snapshot, error) {
	// Allow passing in no headers for cleaner code
	if len(headers) == 0 {
		return s, nil
	}

	//检测header的Number是否正确(是连贯的)
	// Sanity check that the headers can be applied
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].Number.Uint64() != headers[i].Number.Uint64()+1 {
			return nil, errInvalidVotingChain
		}
	}

	//举例: 在number为5时创建了snapshot，那么headers[0]的num必须为6
	//Number:Snapshot 被创建时的block number
	//headers[0]为下一个block的header
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, errInvalidVotingChain
	}
	// Iterate through the headers and create a new snapshot
	snap := s.copy()

	for _, header := range headers {
		// Remove any votes on checkpoint blocks
		number := header.Number.Uint64()
		if number%s.config.Epoch == 0 { //高度到达Epoch, 清空之前投票的相关数据
			snap.Votes = nil
			snap.Tally = make(map[common.Address]Tally)
		}
		// Delete the oldest signer from the recent list to allow it signing again
		if limit := uint64(len(snap.Signers)/2 + 1); number >= limit {
			delete(snap.Recents, number-limit) //把从number往前数limit个元素后的signer删除
		}
		// Resolve the authorization key and check against signers
		signer, err := ecrecover(header, s.sigcache) //获得signer
		if err != nil {
			return nil, err
		}
		if _, ok := snap.Signers[signer]; !ok { //这个块的签名者属于SnapShot的Signers
			return nil, errUnauthorized
		}
		for _, recent := range snap.Recents { //最近已经有过签名 , 不允许再次签名
			if recent == signer {
				return nil, errUnauthorized
			}
		}
		snap.Recents[number] = signer //上面检查通过后，放入Recents

		// Header authorized, discard any previous votes from the signer
		for i, vote := range snap.Votes {
			if vote.Signer == signer && vote.Address == header.Coinbase { //如果已经存在投票者对这个地址的投票，需要删除之前的投票
				// Uncast the vote from the cached tally
				snap.uncast(vote.Address, vote.Authorize)

				// Uncast the vote from the chronological(时间顺序) list
				snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
				break // only one vote allowed
			}
		}


		// Tally up the new vote from the signer
		var authorize bool
		switch {
		case bytes.Equal(header.Nonce[:], nonceAuthVote):
			authorize = true
		case bytes.Equal(header.Nonce[:], nonceDropVote):
			authorize = false
		default:
			return nil, errInvalidVote
		}
		//处理这个header的投票数据
		if snap.cast(header.Coinbase, authorize) { //加进snapshot的Tally
			snap.Votes = append(snap.Votes, &Vote{ //加进snapshot的Votes
				Signer:    signer, //谁投的这张票
				Block:     number,
				Address:   header.Coinbase,
				Authorize: authorize,
			})
		}


		// If the vote passed, update the list of signers
		//snap.Tally[header.Coinbase]: header.Coinbase被投票的情况（几张票，被投出还是加入）
		if tally := snap.Tally[header.Coinbase]; tally.Votes > len(snap.Signers)/2 { //超过半数同意:加入一个或者踢出一个
			if tally.Authorize {
				snap.Signers[header.Coinbase] = struct{}{} //加入Snapshot.Signers
			} else {
				delete(snap.Signers, header.Coinbase) //从Snapshot.Signers移除

				//因为Signers出现了变化, limit值出现了变化
				// Signer list shrunk(压缩), delete any leftover(剩) recent caches
				if limit := uint64(len(snap.Signers)/2 + 1); number >= limit {
					delete(snap.Recents, number-limit)
				}

				// Discard any previous votes the deauthorized signer cast
				for i := 0; i < len(snap.Votes); i++ {
					if snap.Votes[i].Signer == header.Coinbase {//存在被踢出的人的vote
						// Uncast the vote from the cached tally
						snap.uncast(snap.Votes[i].Address, snap.Votes[i].Authorize) //从Tally删除

						// Uncast the vote from the chronological list
						snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...) //从Votes删除

						i--
					}
				}
			}

			//针对一个地址的投票已经被处理，需要删除相关数据。
			//从Votes删除
			// Discard any previous votes around the just changed account
			for i := 0; i < len(snap.Votes); i++ {
				if snap.Votes[i].Address == header.Coinbase {
					snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
					i--
				}
			}

			//从Tally删除
			delete(snap.Tally, header.Coinbase)
		}
	}
	snap.Number += uint64(len(headers)) //更新SnapShot的Number
	snap.Hash = headers[len(headers)-1].Hash() //最后一个header的hash

	return snap, nil
}

//Snapshot.Signers以升序排列
// signers retrieves the list of authorized signers in ascending(上升) order.
func (s *Snapshot) signers() []common.Address {
	signers := make([]common.Address, 0, len(s.Signers))
	for signer := range s.Signers {
		signers = append(signers, signer)
	}
	for i := 0; i < len(signers); i++ {
		for j := i + 1; j < len(signers); j++ {
			if bytes.Compare(signers[i][:], signers[j][:]) > 0 {
				signers[i], signers[j] = signers[j], signers[i]
			}
		}
	}
	return signers
}

//根据轮询机制，判断该signer是intur还是outturn
// inturn returns if a signer at a given block height is in-turn or not.
func (s *Snapshot) inturn(number uint64, signer common.Address) bool {
	signers, offset := s.signers(), 0 //获得所有的投票者
	for offset < len(signers) && signers[offset] != signer { //计算参数携带的signer在所有的投票者的位置
		offset++
	}
	return (number % uint64(len(signers))) == uint64(offset) //判断是否轮到
}
