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

// Package clique implements the proof-of-authority consensus engine.
package clique

import (
	"bytes"
	"errors"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/sha3"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
	"fmt"
)

const (
	//即每当区块链的高度为1024的整数倍时，到达checkpointInterval时间点
	checkpointInterval = 1024 // Number of blocks after which to save the vote snapshot to the database
	inmemorySnapshots  = 128  // Number of recent vote snapshots to keep in memory
	inmemorySignatures = 4096 // Number of recent block signatures to keep in memory

	wiggleTime = 500 * time.Millisecond // Random delay (per signer) to allow concurrent signers
)

// Clique proof-of-authority protocol constants.
var (
	epochLength = uint64(30000) // Default number of blocks after which to checkpoint and reset the pending votes
	blockPeriod = uint64(15)    // Default minimum difference between two consecutive block's timestamps

	extraVanity = 32 // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal   = 65 // Fixed number of extra-data suffix bytes reserved for signer seal

	nonceAuthVote = hexutil.MustDecode("0xffffffffffffffff") // Magic nonce number to vote on adding a new signer
	nonceDropVote = hexutil.MustDecode("0x0000000000000000") // Magic nonce number to vote on removing a signer.

	uncleHash = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.

	diffInTurn = big.NewInt(2) // Block difficulty for in-turn signatures
	diffNoTurn = big.NewInt(1) // Block difficulty for out-of-turn signatures
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errInvalidCheckpointBeneficiary is returned if a checkpoint/epoch transition
	// block has a beneficiary set to non-zeroes.
	errInvalidCheckpointBeneficiary = errors.New("beneficiary in checkpoint block non-zero")

	// errInvalidVote is returned if a nonce value is something else that the two
	// allowed constants of 0x00..0 or 0xff..f.
	errInvalidVote = errors.New("vote nonce not 0x00..0 or 0xff..f")

	// errInvalidCheckpointVote is returned if a checkpoint/epoch transition block
	// has a vote nonce set to non-zeroes.
	errInvalidCheckpointVote = errors.New("vote nonce in checkpoint block non-zero")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's extra-data section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 65 byte suffix signature missing")

	// errExtraSigners is returned if non-checkpoint block contain signer data in
	// their extra-data fields.
	errExtraSigners = errors.New("non-checkpoint block contains extra signer list")

	// errInvalidCheckpointSigners is returned if a checkpoint block contains an
	// invalid list of signers (i.e. non divisible by 20 bytes, or not the correct
	// ones).
	errInvalidCheckpointSigners = errors.New("invalid signer list on checkpoint block")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errInvalidDifficulty is returned if the difficulty of a block is not either
	// of 1 or 2, or if the value does not match the turn of the signer.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// ErrInvalidTimestamp is returned if the timestamp of a block is lower than
	// the previous block's timestamp + the minimum block period.
	ErrInvalidTimestamp = errors.New("invalid timestamp")

	// errInvalidVotingChain is returned if an authorization list is attempted to
	// be modified via out-of-range(超出范围) or non-contiguous(邻近的) headers.
	errInvalidVotingChain = errors.New("invalid voting chain")

	// errUnauthorized is returned if a header is signed by a non-authorized entity.
	errUnauthorized = errors.New("unauthorized")

	// errWaitTransactions is returned if an empty block is attempted to be sealed
	// on an instant chain (0 second period). It's important to refuse these as the
	// block reward is zero, so an empty block just bloats(膨胀) the chain... fast.
	errWaitTransactions = errors.New("waiting for transactions")
)

// SignerFn is a signer callback function to request a hash to be signed by a
// backing account.
type SignerFn func(accounts.Account, []byte) ([]byte, error)

// sigHash returns the hash which is used as input for the proof-of-authority
// signing. It is the hash of the entire header apart from the 65 byte signature
// contained at the end of the extra data.(签名时排除65byte的signature)
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally(偶然) using both forms (signature present
// or not), which could be abused(滥用) to produce different hashes for the same header.
func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewKeccak256()

	rlp.Encode(hasher, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-65], // Yes, this will panic(恐慌) if extra is too short
		header.MixDigest,
		header.Nonce,
	})
	hasher.Sum(hash[:0])
	return hash
}

//获得header的signer的address
// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.ARCCache) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()

	//从sigcache获得
	if address, known := sigcache.Get(hash); known { //sigcache: hash: address
		return address.(common.Address), nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < extraSeal { //Extra小于extraSeal(65)表示没有Signature
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:] //得到signature

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature) //得到pubkey
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])  //crypto.Keccak256(pubkey[1:])[12:]:由pubkey计算出地址

	sigcache.Add(hash, signer)
	return signer, nil
}

//共识引擎
// Clique is the proof-of-authority consensus engine proposed to support the
// Ethereum testnet following the Ropsten attacks.
type Clique struct {
	config *params.CliqueConfig // Consensus engine configuration parameters
	db     ethdb.Database       // Database to store and retrieve snapshot checkpoints

	//c.recents.Add(snap.Hash, snap)
	recents    *lru.ARCCache // Snapshots for recent block to speed up reorgs

	//Header.hash:common.Address(signer的地址)
	signatures *lru.ARCCache // Signatures of recent blocks to speed up mining

	//所有的不记名投票，即每张投票只带有被投票地址和投票内容(新认证状态)
	//proposal: 提案
	proposals map[common.Address]bool // Current list of proposals we are pushing ////当前signer提出的proposals列表

	signer common.Address // Ethereum address of the signing key
	signFn SignerFn       // Signer function to authorize hashes with
	lock   sync.RWMutex   // Protects the signer fields
}

//创建一个Clique
// New creates a Clique proof-of-authority consensus engine with the initial
// signers set to the ones provided by the user.
func New(config *params.CliqueConfig, db ethdb.Database) *Clique {
	// Set any missing consensus parameters to their defaults
	conf := *config
	if conf.Epoch == 0 {
		conf.Epoch = epochLength //30000
	}
	// Allocate the snapshot caches and create the engine
	recents, _ := lru.NewARC(inmemorySnapshots) //128
	signatures, _ := lru.NewARC(inmemorySignatures) //4096

	return &Clique{
		config:     &conf,
		db:         db,
		recents:    recents,
		signatures: signatures,
		proposals:  make(map[common.Address]bool),
	}
}

//从header's extradata的signature恢复出来的address
// Author implements consensus.Engine, returning the Ethereum address recovered
// from the signature in the header's extra-data section.
func (c *Clique) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, c.signatures)
}

// VerifyHeader checks whether a header conforms to(符合) the consensus rules.
func (c *Clique) VerifyHeader(chain consensus.ChainReader, header *types.Header, seal bool) error {
	return c.verifyHeader(chain, header, nil)
}

//验证一批的header
// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort(舍弃) the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (c *Clique) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := c.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort: //读abort通道的数据,abort不是在这里才创建的么
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

//检查header的各个成员是否符合协议要求
// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending上升 order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (c *Clique) verifyHeader(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	// Don't waste time checking blocks from the future
	if header.Time.Cmp(big.NewInt(time.Now().Unix())) > 0 {//header的时间新于当前时间
		return consensus.ErrFutureBlock
	}
	// Checkpoint blocks need to enforce zero beneficiary(受益人)
	//达到Epoch点,这时header.Coinbase必须为空
	checkpoint := (number % c.config.Epoch) == 0 //%求余运算
	if checkpoint && header.Coinbase != (common.Address{}) {//coinbase不为空
		return errInvalidCheckpointBeneficiary
	}
	//Nonce的值必须为nonceAuthVote("0xffffffffffffffff")或者nonceDropVote("0x0000000000000000")
	// Nonces must be 0x00..0 or 0xff..f, zeroes enforced on checkpoints
	if !bytes.Equal(header.Nonce[:], nonceAuthVote) && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidVote
	}

	//达到Epoch点,这时Nonce必须为nonceDropVote("0x0000000000000000")
	if checkpoint && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidCheckpointVote
	}

	//header.Extra的长度必须大于等于extraVanity(32)
	// Check that the extra-data contains both the vanity and signature
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}

	//header.Extra的长度必须大于等于extraVanity(32) + extraSeal(65)
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	//没有达到Epoch点时,header.Extra的长度必须等于(extraVanity 32 + extraSeal 65)
	// Ensure that the extra-data contains a signer list on checkpoint, but none otherwise
	signersBytes := len(header.Extra) - extraVanity - extraSeal
	if !checkpoint && signersBytes != 0 {
		return errExtraSigners
	}

	//达到Epoch点时,len(header.Extra) - extraVanity - extraSeal的值必须是AddressLength(20)的整数倍
	if checkpoint && signersBytes%common.AddressLength != 0 {
		return errInvalidCheckpointSigners
	}

	//MixDigest必须为空(这个用来干吗？？)
	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}

	//POA中Uncle毫无意义
	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}
	// Ensure that the block's difficulty is meaningful (may not be correct at this point)
	if number > 0 {
		//Difficulty等于nil或者Difficulty既不是diffInTurn也不是diffNoTurn
		if header.Difficulty == nil || (header.Difficulty.Cmp(diffInTurn) != 0 && header.Difficulty.Cmp(diffNoTurn) != 0) {
			return errInvalidDifficulty
		}
	}

	//检查EIP150Block
	// If all checks passed, validate any special fields for hard forks
	if err := misc.VerifyForkHashes(chain.Config(), header, false); err != nil {
		return err
	}
	// All basic checks passed, verify cascading(级联) fields
	return c.verifyCascadingFields(chain, header, parents)
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
//Cascading级联
func (c *Clique) verifyCascadingFields(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {//如果是genesis block, 不需要检查
		return nil
	}
	// Ensure that the block's timestamp isn't too close to it's parent
	var parent *types.Header
	if len(parents) > 0 { //如果有携带parents参数
		parent = parents[len(parents)-1] //获取最后一个parent
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1) //通过ParentHash和number获取
	}

	//得到的parent是否真的是header里申明的parent
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor //Ancestor祖先
	}

	//两块之间的时间间隔是否小于Period
	if parent.Time.Uint64()+c.config.Period > header.Time.Uint64() {
		return ErrInvalidTimestamp
	}

	//根据number和ParentHash得到snapshot
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	//如果达到Epoch点,
	// If the block is a checkpoint block, verify the signer list
	if number%c.config.Epoch == 0 {
		signers := make([]byte, len(snap.Signers)*common.AddressLength) //创建一个空数组，size为signer的个数*AddressLength(20)
		for i, signer := range snap.signers() {
			copy(signers[i*common.AddressLength:], signer[:])//将snap.signers()读入创建的数组
		}
		extraSuffix := len(header.Extra) - extraSeal //header.Extra的组成为vanity:signer:Seal
		if !bytes.Equal(header.Extra[extraVanity:extraSuffix], signers) {//验证header.Extra的元素是否正确
			return errInvalidCheckpointSigners
		}
	}
	// All basic checks passed, verify the seal and return
	return c.verifySeal(chain, header, parents)
}

// snapshot retrieves(取回) the authorization snapshot at a given point in time(及时).
/**
 number: 要建立的header的父块高度
 hash: 要建立的header的父块hash
 */
func (c *Clique) snapshot(chain consensus.ChainReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	// Search for a snapshot in memory or on disk for checkpoints
	var (
		headers []*types.Header
		snap    *Snapshot
	)
	fmt.Printf("test clique.go snapshot snap = %s\n", snap)
	for snap == nil {
		fmt.Printf("test clique.go snapshot number = %s\n",number)
		// If an in-memory snapshot was found, use that
		//1.从lru.ARCCache通过hash获得Snapshot
		if s, ok := c.recents.Get(hash); ok { //hash: Snapshot(get from recents cache)
			snap = s.(*Snapshot)
			fmt.Printf("%s\n","test clique.go snapshot break 1")
			break
		}
		// If an on-disk checkpoint snapshot can be found, use that
		if number%checkpointInterval == 0 {//reach checkpoint time
			//2.从数据库clique.db读取后，创建snapshot
			if s, err := loadSnapshot(c.config, c.signatures, c.db, hash); err == nil {
				log.Trace("Loaded voting snapshot form disk", "number", number, "hash", hash)
				snap = s
				fmt.Printf("%s\n","test clique.go snapshot break 2")
				break
			}
		}
		// If we're at block zero, make a snapshot
		if number == 0 {
			fmt.Printf("%s\n", "test clique.go snapshot")
			//得到创世纪块
			genesis := chain.GetHeaderByNumber(0) //get Header through number
			if err := c.VerifyHeader(chain, genesis, false); err != nil {
				return nil, err
			}

			//地址长度20，创建genesis.Extra包含的签名者数组
			signers := make([]common.Address, (len(genesis.Extra)-extraVanity-extraSeal)/common.AddressLength)
			for i := 0; i < len(signers); i++ {
				copy(signers[i][:], genesis.Extra[extraVanity+i*common.AddressLength:])
			}
			snap = newSnapshot(c.config, c.signatures, 0, genesis.Hash(), signers) //3.创建snapshot
			if err := snap.store(c.db); err != nil {//将snapshot存入db
				return nil, err
			}
			log.Trace("Stored genesis voting snapshot to disk")
			fmt.Printf("test clique.go snapshot break 3")
			break
		}
		// No snapshot for this header, gather the header and move backward(向后移动)
		var header *types.Header
		if len(parents) > 0 {//如果参数parents有值
			// If we have explicit parents, pick from there (enforced)
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}
			parents = parents[:len(parents)-1]
		} else {
			// No explicit parents (or no more left), reach out to the database
			header = chain.GetHeader(hash, number) //通过hash和number获取header
			if header == nil {
				return nil, consensus.ErrUnknownAncestor
			}
		}
		headers = append(headers, header) //header放入headers
		number, hash = number-1, header.ParentHash //往前查找
	}
	//headers元素顺序是：header5 header4 header3...，也就是从链尾到链头
	fmt.Printf("%s\n", "test clique.go snapshot end")
	fmt.Printf("%s\n", "          ")
	fmt.Printf("%s\n", "          ")
	fmt.Printf("%s\n", "          ")
	// Previous snapshot found, apply any pending headers on top of it
	//举例:原始数据[5 4 3 2 1]
	//    for循环后的数据[1 2 3 4 5]
	//所以这里调换位置
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}
	snap, err := snap.apply(headers) //处理headers包含的投票数据
	if err != nil {
		return nil, err
	}
	c.recents.Add(snap.Hash, snap) //snap放入缓存 lru.ARCCache: snap.Hash:snap

	//新checkpoint的snapshot
	// If we've generated a new checkpoint snapshot, save to disk
	if snap.Number%checkpointInterval == 0 && len(headers) > 0 {
		if err = snap.store(c.db); err != nil {  //将snapshot存入db
			return nil, err
		}
		log.Trace("Stored voting snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}
	return snap, err
}

//POA不允许uncle
// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (c *Clique) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (c *Clique) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	return c.verifySeal(chain, header, nil)
}

//判断header的signer是否有授权，且difficulty是否正确
//VerifySeal()函数基于跟Seal()完全一样的算法原理，通过验证区块的某些属性(Header.Nonce，Header.MixDigest等)
//是否正确，来确定该区块是否已经经过Seal操作
// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
func (c *Clique) verifySeal(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	fmt.Printf("test consensus/clique/clique.go VerifySeal %s\n", "")

	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 { //genesis block不能用来verifySeal
		return errUnknownBlock
	}
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(chain, number-1, header.ParentHash, parents) //获得父块的snapshot
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against signers
	signer, err := ecrecover(header, c.signatures) //获得header的signer的address
	if err != nil {
		return err
	}
	if _, ok := snap.Signers[signer]; !ok { //该signer是否存在于Signers(为什么通过父的number和hash获取的snapshot可以验证该header的signer是否有权限)
		return errUnauthorized
	}
	for seen, recent := range snap.Recents { //seen:区块高度   recent:common.Address
		if recent == signer { //seen:signer签名时的区块高度
			// Signer is among recents, only fail if the current block doesn't shift it out
			//可是如果是轮询机制，那么下次签名的number不应该是是间隔len(snap.Signers)么
			if limit := uint64(len(snap.Signers)/2 + 1); seen > number-limit { //如果中间没有间隔(len(snap.Signers)/2 + 1)个block
				return errUnauthorized //signer没有权限对该header签名
			}
		}
	}
	// Ensure that the difficulty corresponds to the turn-ness of the signer
	inturn := snap.inturn(header.Number.Uint64(), signer)
	if inturn && header.Difficulty.Cmp(diffInTurn) != 0 {//如果是inturn，判断difficulty是否匹配
		return errInvalidDifficulty
	}
	if !inturn && header.Difficulty.Cmp(diffNoTurn) != 0 { //不是inturn也可以sign？？
		return errInvalidDifficulty
	}
	return nil
}

//设置header的Coinbase,Nonce,Difficulty,Extra,MixDigest,Time
// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (c *Clique) Prepare(chain consensus.ChainReader, header *types.Header) error {
	// If the block isn't a checkpoint, cast a random vote (good enough for now)
	header.Coinbase = common.Address{} //header.Coinbase重置为空
	header.Nonce = types.BlockNonce{}  //header.Nonce重置为空

	number := header.Number.Uint64() //当前块的高度，不是父块的高度
	fmt.Printf("%s\n", "test clique/clique.go Prepare ")
	fmt.Printf("test clique/clique.go Prepare number = %x\n", header.Number)
	fmt.Printf("test clique/clique.go Prepare ParentHash = %x\n", header.ParentHash)
	// Assemble the voting snapshot to check which votes make sense
	snap, err := c.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}
	if number%c.config.Epoch != 0 { //高度是否是Epoch
		//不是Epoch高度
		c.lock.RLock()


		//proposals是自己发起的投票集合？？？
		//选择一张投票作为本次的投票,更新到header
		// Gather all the proposals that make sense voting on
		addresses := make([]common.Address, 0, len(c.proposals)) //common.Address:bool
		for address, authorize := range c.proposals {
			if snap.validVote(address, authorize) {//如果proposals的这张票是有效的
				addresses = append(addresses, address) //符合标准的address添加至addresses
			}
		}
		// If there's pending proposals, cast a vote on them
		if len(addresses) > 0 {
			header.Coinbase = addresses[rand.Intn(len(addresses))] //随机从addresses读取一个address
			if c.proposals[header.Coinbase] { //authorize为true
				copy(header.Nonce[:], nonceAuthVote) //0xffffffffffffffff
			} else {//authorize为false
				copy(header.Nonce[:], nonceDropVote)  //"0x0000000000000000"
			}
		}
		c.lock.RUnlock()
	}
	// Set the correct difficulty
	header.Difficulty = CalcDifficulty(snap, c.signer) //根据是否属于inturn，设置Difficulty

	// Ensure the extra data has all it's components
	if len(header.Extra) < extraVanity { //extraVanity(32) 添加0，将extra的数据长度填充至32
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]

	if number%c.config.Epoch == 0 { //高度是Epoch时，block携带所有的signer
		for _, signer := range snap.signers() {
			header.Extra = append(header.Extra, signer[:]...) //添加signer
		}
	}
	header.Extra = append(header.Extra, make([]byte, extraSeal)...) //预留Seal的空间

	// Mix digest is reserved(保留的) for now(目前), set to empty
	header.MixDigest = common.Hash{}

	// Ensure the timestamp has the correct delay
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil { //父block为nil
		return consensus.ErrUnknownAncestor
	}

	//parent的时间 + Period
	header.Time = new(big.Int).Add(parent.Time, new(big.Int).SetUint64(c.config.Period))
	if header.Time.Int64() < time.Now().Unix() { //假定的时间比当前时间小
		header.Time = big.NewInt(time.Now().Unix())
	}
	return nil
}

//做一下清空/update动作，生成新块
// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards(奖励) given, and returns the final block.
func (c *Clique) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// No block rewards in PoA, so the state remains as is and uncles are dropped
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number)) //s.trie.Hash()   更新trie中的相应数据
	header.UncleHash = types.CalcUncleHash(nil)//UncleHash由nil生成

	// Assemble and return the final block for sealing
	return types.NewBlock(header, txs, nil, receipts), nil //receipt:收据
}

//启动挖矿时被调用
// Authorize injects a private key into the consensus engine to mint new blocks
// with.
func (c *Clique) Authorize(signer common.Address, signFn SignerFn) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.signer = signer //赋值signer
	c.signFn = signFn //赋值signFn, it is a func
}

//Seal: 密封
// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (c *Clique) Seal(chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {
	fmt.Printf("%s\n", "test clique.go Seal start")
	header := block.Header()

	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 { //genesis block 不支持 seal
		return nil, errUnknownBlock
	}
	// For 0-period chains, refuse to seal empty blocks (no reward but would spin sealing)
	if c.config.Period == 0 && len(block.Transactions()) == 0 {
		return nil, errWaitTransactions //Period为0，同时没有交易
	}
	// Don't hold the signer fields for the entire sealing procedure
	c.lock.RLock()
	signer, signFn := c.signer, c.signFn //setvalue in backend.go StartMining
	c.lock.RUnlock()

	// Bail out if we're unauthorized to sign a block
	snap, err := c.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return nil, err
	}
	if _, authorized := snap.Signers[signer]; !authorized {
		return nil, errUnauthorized
	}
	// If we're amongst the recent signers, wait for the next block
	for seen, recent := range snap.Recents {
		if recent == signer {//已经在Recents
			// Signer is among recents, only wait if the current block doesn't shift it out
			if limit := uint64(len(snap.Signers)/2 + 1); number < limit || seen > number-limit { //且没有间隔len(snap.Signers)/2 + 1)
				log.Info("Signed recently, must wait for others")
				<-stop //中断(cpuAgent的quitCurrentOp通道被写入,没看到哪里监听呢)
				return nil, nil
			}
		}
	}
	// Sweet, the protocol permits us to sign the block, wait for our time
	delay := time.Unix(header.Time.Int64(), 0).Sub(time.Now()) // nolint: gosimple
	if header.Difficulty.Cmp(diffNoTurn) == 0 {
		// It's not our turn explicitly(明确地) to sign, delay it a bit
		wiggle := time.Duration(len(snap.Signers)/2+1) * wiggleTime  //wiggle摆动
		delay += time.Duration(rand.Int63n(int64(wiggle))) //delay a rand time

		log.Trace("Out-of-turn signing requested", "wiggle", common.PrettyDuration(wiggle))
	}
	log.Trace("Waiting for slot to sign and propagate", "delay", common.PrettyDuration(delay))

	select {
	case <-stop:
		return nil, nil
	case <-time.After(delay):
	}

	//进行签名
	// Sign all the things!
	sighash, err := signFn(accounts.Account{Address: signer}, sigHash(header).Bytes())
	if err != nil {
		return nil, err
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sighash) //sighash赋值到Extra的Seal位置

	return block.WithSeal(header), nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func (c *Clique) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	snap, err := c.snapshot(chain, parent.Number.Uint64(), parent.Hash(), nil) //由parent.Number和parent.Hash()得到snapshot
	if err != nil {
		return nil
	}
	return CalcDifficulty(snap, c.signer)
}


//计算difficulty,值只能为diffInTurn(2)或者diffNoTurn(1)
// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func CalcDifficulty(snap *Snapshot, signer common.Address) *big.Int {
	if snap.inturn(snap.Number+1, signer) { //目标地址在下一块是否为inturn
		return new(big.Int).Set(diffInTurn)
	}
	return new(big.Int).Set(diffNoTurn)
}

// APIs implements consensus.Engine, returning the user facing RPC API to allow
// controlling the signer voting.
func (c *Clique) APIs(chain consensus.ChainReader) []rpc.API {
	return []rpc.API{{
		Namespace: "clique",
		Version:   "1.0",
		Service:   &API{chain: chain, clique: c},
		Public:    false,
	}}
}
