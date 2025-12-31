// Package trainkv adapts TrainKV (an embedded LSM-tree KV store) to go-ycsb.
package trainkv

import (
	"context"
	"fmt"
	"os"

	TrainKV "github.com/kebukeYi/TrainKV/v2"
	"github.com/kebukeYi/TrainKV/v2/interfaces"
	"github.com/kebukeYi/TrainKV/v2/lsm"

	"github.com/magiconair/properties"

	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/util"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// properties
const (
	trainkvDir            = "trainkv.dir"
	trainkvSyncWrites     = "trainkv.sync_writes"
	trainkvValueThreshold = "trainkv.value_threshold"
	trainkvMemTableSize   = "trainkv.memtable_size"
	trainkvBlockSize      = "trainkv.block_size"
	trainkvCacheNums      = "trainkv.cache_nums"
	trainkvNumCompactors  = "trainkv.num_compactors"
)

type trainkvCreator struct{}

func init() {
	ycsb.RegisterDBCreator("trainkv", trainkvCreator{})
}

type trainkvDB struct {
	p *properties.Properties

	db *TrainKV.TrainKV

	r       *util.RowCodec
	bufPool *util.BufPool
}

func (c trainkvCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	opt := getOptions(p)

	if p.GetBool(prop.DropData, prop.DropDataDefault) {
		os.RemoveAll(opt.WorkDir)
	}

	// TrainKV.Open 不会自建 WorkDir(锁文件需要目录先存在)
	if err := os.MkdirAll(opt.WorkDir, 0o755); err != nil {
		return nil, err
	}

	db, err, _ := TrainKV.Open(opt) // WorkDir 非空时返回的清理回调是空操作
	if err != nil {
		return nil, err
	}

	return &trainkvDB{
		p:       p,
		db:      db,
		r:       util.NewRowCodec(p),
		bufPool: util.NewBufPool(),
	}, nil
}

func getOptions(p *properties.Properties) *lsm.Options {
	opt := lsm.GetDefaultOpt(p.GetString(trainkvDir, "/tmp/trainkv"))
	opt.SyncWrites = p.GetBool(trainkvSyncWrites, opt.SyncWrites)
	opt.ValueThreshold = p.GetInt64(trainkvValueThreshold, opt.ValueThreshold)
	opt.MemTableSize = p.GetInt64(trainkvMemTableSize, opt.MemTableSize)
	opt.BlockSize = uint32(p.GetInt(trainkvBlockSize, int(opt.BlockSize)))
	opt.CacheNums = p.GetInt(trainkvCacheNums, opt.CacheNums)
	opt.NumCompactors = p.GetInt(trainkvNumCompactors, opt.NumCompactors)
	return opt
}

func (db *trainkvDB) Close() error {
	return db.db.Close()
}

func (db *trainkvDB) InitThread(ctx context.Context, _ int, _ int) context.Context {
	return ctx
}

func (db *trainkvDB) CleanupThread(_ context.Context) {
}

func (db *trainkvDB) getRowKey(table string, key string) []byte {
	return util.Slice(fmt.Sprintf("%s:%s", table, key))
}

func (db *trainkvDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	txn := db.db.NewTransaction(false)
	defer txn.Discard()

	entry, err := txn.Get(db.getRowKey(table, key))
	if err != nil {
		return nil, err
	}

	return db.r.Decode(entry.Value, fields)
}

func (db *trainkvDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	txn := db.db.NewTransaction(false)
	defer txn.Discard()

	iter := txn.NewIterator(&interfaces.Options{IsAsc: true, IsSetCache: true})
	defer func() { _ = iter.Close() }()

	res := make([]map[string][]byte, 0, count)
	for iter.Seek(db.getRowKey(table, startKey)); iter.Valid() && len(res) < count; iter.Next() {
		// Item 是借用语义，Value() 返回独立拷贝，必须在 Next() 前消费完
		row, err := iter.Item().Value()
		if err != nil {
			return nil, err
		}
		m, err := db.r.Decode(row, fields)
		if err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	return res, nil
}

func (db *trainkvDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	txn := db.db.NewTransaction(true)

	rowKey := db.getRowKey(table, key)
	entry, err := txn.Get(rowKey)
	if err != nil {
		txn.Discard()
		return err
	}

	data, err := db.r.Decode(entry.Value, nil)
	if err != nil {
		txn.Discard()
		return err
	}

	for field, value := range values {
		data[field] = value
	}

	buf := db.bufPool.Get()
	defer func() {
		db.bufPool.Put(buf)
	}()

	buf, err = db.r.Encode(buf, data)
	if err != nil {
		txn.Discard()
		return err
	}

	if err := txn.Set(rowKey, buf); err != nil {
		txn.Discard()
		return err
	}

	// 契约：Commit 之后不得再 Discard（事务对象会被池化复用）
	_, err = txn.Commit()
	return err
}

func (db *trainkvDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	txn := db.db.NewTransaction(true)

	buf := db.bufPool.Get()
	defer func() {
		db.bufPool.Put(buf)
	}()

	buf, err := db.r.Encode(buf, values)
	if err != nil {
		txn.Discard()
		return err
	}

	if err := txn.Set(db.getRowKey(table, key), buf); err != nil {
		txn.Discard()
		return err
	}

	_, err = txn.Commit()
	return err
}

func (db *trainkvDB) Delete(ctx context.Context, table string, key string) error {
	txn := db.db.NewTransaction(true)

	if err := txn.Delete(db.getRowKey(table, key)); err != nil {
		txn.Discard()
		return err
	}

	_, err := txn.Commit()
	return err
}
