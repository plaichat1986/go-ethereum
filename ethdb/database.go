// Copyright 2014 The go-ethereum Authors
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

package ethdb

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"fmt"
	//"runtime/debug"
)

var OpenFileLimit = 64

type LDBDatabase struct {
	fn string      // filename for reporting
	db *leveldb.DB // LevelDB instance

	getTimer       metrics.Timer // Timer for measuring the database get request counts and latencies //记录get的调用时间
	putTimer       metrics.Timer // Timer for measuring the database put request counts and latencies //记录put的调用时间
	delTimer       metrics.Timer // Timer for measuring the database delete request counts and latencies //记录删除的调用时间
	missMeter      metrics.Meter // Meter for measuring the missed database get requests  //记录get时，miss的次数
	readMeter      metrics.Meter // Meter for measuring the database get request data usage //读取的结果的len
	writeMeter     metrics.Meter // Meter for measuring the database put request data usage
	compTimeMeter  metrics.Meter // Meter for measuring the total time spent in database compaction
	compReadMeter  metrics.Meter // Meter for measuring the data read during compaction
	compWriteMeter metrics.Meter // Meter for measuring the data written during compaction

	quitLock sync.Mutex      // Mutex protecting the quit channel access
	quitChan chan chan error // Quit channel to stop the metrics(指标) collection before closing the database

	log log.Logger // Contextual(上下文) logger tracking the database path
}

/**
   file: home/wuliang/liang/job/gopath/src/github.com/ethereum/go-ethereum/dev/testnet3/node1/geth/chaindata
   cache: 768
   handles: 512
 */
// NewLDBDatabase returns a LevelDB wrapped object.
func NewLDBDatabase(file string, cache int, handles int) (*LDBDatabase, error) {
	logger := log.New("database", file)
	fmt.Printf(" %s\n", " ")
	fmt.Printf(" %s\n", " ")
	fmt.Printf("test database.go NewLDBDatabase start file = %s\n", file)
	fmt.Printf("test database.go NewLDBDatabase start cache = %s\n", cache)
	fmt.Printf("test database.go NewLDBDatabase start handles = %s\n", handles)
	//debug.PrintStack()
	fmt.Printf("test database.go NewLDBDatabase end %s\n", "")
	fmt.Printf(" %s\n", " ")
	fmt.Printf(" %s\n", " ")

	// Ensure we have some minimal caching and file guarantees
	if cache < 16 {
		cache = 16
	}
	if handles < 16 {
		handles = 16
	}
	logger.Info("Allocated cache and file handles", "cache", cache, "handles", handles)

	// Open the db and recover(恢复) any potential corruptions(腐败)
	db, err := leveldb.OpenFile(file, &opt.Options{ //创建或者打开一个db
		OpenFilesCacheCapacity: handles,
		BlockCacheCapacity:     cache / 2 * opt.MiB,
		WriteBuffer:            cache / 4 * opt.MiB, // Two of these are used internally
		Filter:                 filter.NewBloomFilter(10),
	})
	if _, corrupted := err.(*errors.ErrCorrupted); corrupted {  //corrupted 损坏
		db, err = leveldb.RecoverFile(file, nil)
	}
	// (Re)check for errors and abort if opening of the db failed
	if err != nil {
		return nil, err
	}
	return &LDBDatabase{
		fn:  file,
		db:  db,
		log: logger,
	}, nil
}

// Path returns the path to the database directory.
func (db *LDBDatabase) Path() string {
	return db.fn
}

//执行leveldb的put方法
// Put puts the given key / value to the queue
func (db *LDBDatabase) Put(key []byte, value []byte) error {
	fmt.Printf("test database.go Put key = %s\n", string(key[:]));

	// Measure the database put latency(潜伏), if requested
	if db.putTimer != nil {
		defer db.putTimer.UpdateSince(time.Now())
	}
	// Generate the data to write to disk, update the meter and write
	//value = rle.Compress(value)

	if db.writeMeter != nil {
		db.writeMeter.Mark(int64(len(value)))
	}
	return db.db.Put(key, value, nil) //执行leveldb的put方法
}

//执行leveldb的Has方法
func (db *LDBDatabase) Has(key []byte) (bool, error) {
	return db.db.Has(key, nil)
}

//执行leveldb的Get方法
// Get returns the given key if it's present.
func (db *LDBDatabase) Get(key []byte) ([]byte, error) {
	fmt.Printf("test database.go Get key = %s\n", string(key[:]));

	// Measure the database get latency, if requested
	if db.getTimer != nil {
		defer db.getTimer.UpdateSince(time.Now())
	}
	// Retrieve the key and increment the miss counter if not found
	dat, err := db.db.Get(key, nil)
	if err != nil {
		if db.missMeter != nil {
			db.missMeter.Mark(1)
		}
		return nil, err
	}
	// Otherwise update the actually retrieved amount of data
	if db.readMeter != nil {
		db.readMeter.Mark(int64(len(dat)))
	}
	return dat, nil
	//return rle.Decompress(dat)
}

//执行leveldb的Delete方法
// Delete deletes the key from the queue and database
func (db *LDBDatabase) Delete(key []byte) error {
	fmt.Printf("test database.go Delete key = %s\n", string(key[:]));

	// Measure the database delete latency(潜伏), if requested
	if db.delTimer != nil {
		defer db.delTimer.UpdateSince(time.Now())
	}
	// Execute the actual operation
	return db.db.Delete(key, nil)
}

// NewIterator returns an iterator for the latest snapshot of the
// underlying DB.
func (db *LDBDatabase) NewIterator() iterator.Iterator {
	return db.db.NewIterator(nil, nil)
}


//执行leveldb的Close方法
func (db *LDBDatabase) Close() {
	// Stop the metrics collection to avoid internal database races
	db.quitLock.Lock()
	defer db.quitLock.Unlock()

	if db.quitChan != nil {
		errc := make(chan error)
		db.quitChan <- errc
		if err := <-errc; err != nil {
			db.log.Error("Metrics collection failed", "err", err)
		}
	}
	err := db.db.Close()
	if err == nil {
		db.log.Info("Database closed")
	} else {
		db.log.Error("Failed to close database", "err", err)
	}
}

func (db *LDBDatabase) LDB() *leveldb.DB {
	return db.db
}

// Meter configures the database metrics collectors and
func (db *LDBDatabase) Meter(prefix string) { //"eth/db/chaindata/"
	// Short circuit metering if the metrics system is disabled
	if !metrics.Enabled {
		return
	}
	// Initialize all the metrics collector at the requested prefix
	db.getTimer = metrics.NewRegisteredTimer(prefix+"user/gets", nil)
	db.putTimer = metrics.NewRegisteredTimer(prefix+"user/puts", nil)
	db.delTimer = metrics.NewRegisteredTimer(prefix+"user/dels", nil)
	db.missMeter = metrics.NewRegisteredMeter(prefix+"user/misses", nil)
	db.readMeter = metrics.NewRegisteredMeter(prefix+"user/reads", nil)
	db.writeMeter = metrics.NewRegisteredMeter(prefix+"user/writes", nil)
	db.compTimeMeter = metrics.NewRegisteredMeter(prefix+"compact/time", nil)
	db.compReadMeter = metrics.NewRegisteredMeter(prefix+"compact/input", nil)
	db.compWriteMeter = metrics.NewRegisteredMeter(prefix+"compact/output", nil)

	// Create a quit channel for the periodic collector and run it
	db.quitLock.Lock()
	db.quitChan = make(chan chan error)
	db.quitLock.Unlock()

	go db.meter(3 * time.Second)
}

// meter periodically retrieves internal leveldb counters and reports them to
// the metrics subsystem.
//
// This is how a stats table look like (currently):
//   Compactions
//    Level |   Tables   |    Size(MB)   |    Time(sec)  |    Read(MB)   |   Write(MB)
//   -------+------------+---------------+---------------+---------------+---------------
//      0   |          0 |       0.00000 |       1.27969 |       0.00000 |      12.31098
//      1   |         85 |     109.27913 |      28.09293 |     213.92493 |     214.26294
//      2   |        523 |    1000.37159 |       7.26059 |      66.86342 |      66.77884
//      3   |        570 |    1113.18458 |       0.00000 |       0.00000 |       0.00000
func (db *LDBDatabase) meter(refresh time.Duration) {
	// Create the counters to store current and previous values
	counters := make([][]float64, 2)
	for i := 0; i < 2; i++ {
		counters[i] = make([]float64, 3)
	}
	// Iterate ad infinitum and collect the stats
	for i := 1; ; i++ {
		// Retrieve the database stats
		stats, err := db.db.GetProperty("leveldb.stats")
		if err != nil {
			db.log.Error("Failed to read database stats", "err", err)
			return
		}
		// Find the compaction table, skip the header
		lines := strings.Split(stats, "\n")
		for len(lines) > 0 && strings.TrimSpace(lines[0]) != "Compactions" {
			lines = lines[1:]
		}
		if len(lines) <= 3 {
			db.log.Error("Compaction table not found")
			return
		}
		lines = lines[3:]

		// Iterate over all the table rows, and accumulate the entries
		for j := 0; j < len(counters[i%2]); j++ {
			counters[i%2][j] = 0
		}
		for _, line := range lines {
			parts := strings.Split(line, "|")
			if len(parts) != 6 {
				break
			}
			for idx, counter := range parts[3:] {
				value, err := strconv.ParseFloat(strings.TrimSpace(counter), 64)
				if err != nil {
					db.log.Error("Compaction entry parsing failed", "err", err)
					return
				}
				counters[i%2][idx] += value
			}
		}
		// Update all the requested meters
		if db.compTimeMeter != nil {
			db.compTimeMeter.Mark(int64((counters[i%2][0] - counters[(i-1)%2][0]) * 1000 * 1000 * 1000))
		}
		if db.compReadMeter != nil {
			db.compReadMeter.Mark(int64((counters[i%2][1] - counters[(i-1)%2][1]) * 1024 * 1024))
		}
		if db.compWriteMeter != nil {
			db.compWriteMeter.Mark(int64((counters[i%2][2] - counters[(i-1)%2][2]) * 1024 * 1024))
		}
		// Sleep a bit, then repeat the stats collection
		select {
		case errc := <-db.quitChan:
			// Quit requesting, stop hammering the database
			errc <- nil
			return

		case <-time.After(refresh):
			// Timeout, gather a new set of stats
		}
	}
}

func (db *LDBDatabase) NewBatch() Batch {
	return &ldbBatch{db: db.db, b: new(leveldb.Batch)}
}

type ldbBatch struct {
	db   *leveldb.DB
	b    *leveldb.Batch
	size int
}

func (b *ldbBatch) Put(key, value []byte) error {
	fmt.Printf("test database.go batch Put key = %s\n", string(key[:]));
	b.b.Put(key, value)  //把数据放入leveldb.Batch,等调用Write()时才真正的执行写入db动作
	b.size += len(value)
	return nil
}

func (b *ldbBatch) Write() error {
	fmt.Printf("test database.go batch Write%s\n", "");
	return b.db.Write(b.b, nil) //把leveldb.Batch的数据写入leveldb.DB
}

func (b *ldbBatch) ValueSize() int {
	return b.size
}

func (b *ldbBatch) Reset() {
	b.b.Reset()
	b.size = 0
}

type table struct {
	db     Database
	prefix string
}

// NewTable returns a Database object that prefixes all keys with a given
// string.
func NewTable(db Database, prefix string) Database {
	return &table{
		db:     db,
		prefix: prefix,
	}
}

//prefix + key
func (dt *table) Put(key []byte, value []byte) error {
	fmt.Printf("test database.go table Put%s\n", "");
	return dt.db.Put(append([]byte(dt.prefix), key...), value)
}

func (dt *table) Has(key []byte) (bool, error) {
	fmt.Printf("test database.go table Has%s\n", "");
	return dt.db.Has(append([]byte(dt.prefix), key...))
}

func (dt *table) Get(key []byte) ([]byte, error) {
	fmt.Printf("test database.go table Get%s\n", "");
	return dt.db.Get(append([]byte(dt.prefix), key...))
}

func (dt *table) Delete(key []byte) error {
	fmt.Printf("test database.go table Delete%s\n", "");
	return dt.db.Delete(append([]byte(dt.prefix), key...))
}

func (dt *table) Close() {
	// Do nothing; don't close the underlying DB.
}

type tableBatch struct {
	batch  Batch
	prefix string
}

// NewTableBatch returns a Batch object which prefixes all keys with a given string.
func NewTableBatch(db Database, prefix string) Batch {
	return &tableBatch{db.NewBatch(), prefix}
}

func (dt *table) NewBatch() Batch {
	return &tableBatch{dt.db.NewBatch(), dt.prefix}
}

func (tb *tableBatch) Put(key, value []byte) error {
	fmt.Printf("test database.go tableBatch Put%s\n", "");
	return tb.batch.Put(append([]byte(tb.prefix), key...), value)
}

func (tb *tableBatch) Write() error {
	fmt.Printf("test database.go tableBatch Write%s\n", "");
	return tb.batch.Write()
}

func (tb *tableBatch) ValueSize() int {
	return tb.batch.ValueSize()
}

func (tb *tableBatch) Reset() {
	tb.batch.Reset()
}
