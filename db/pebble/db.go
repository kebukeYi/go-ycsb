// Package pebble adapts CockroachDB's pebble (a Go LSM engine) to go-ycsb.
package pebble

import (
	"context"
	"fmt"
	"os"

	"github.com/cockroachdb/pebble"
	"github.com/magiconair/properties"

	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/util"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// properties
const (
	pebbleDir        = "pebble.dir"
	pebbleSyncWrites = "pebble.sync_writes"
	pebbleMemTableSz = "pebble.memtable_size"
	pebbleCacheSz    = "pebble.cache_size"
	pebbleMemTableSzD = int64(64 << 20) // 对齐 badger/trainkv 的默认 64MB
)

type pebbleCreator struct{}

func init() {
	ycsb.RegisterDBCreator("pebble", pebbleCreator{})
}

type pebbleDB struct {
	p *properties.Properties

	db *pebble.DB

	r       *util.RowCodec
	bufPool *util.BufPool
	sync    bool
}

func (c pebbleCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	dir := p.GetString(pebbleDir, "/tmp/pebble")

	if p.GetBool(prop.DropData, prop.DropDataDefault) {
		os.RemoveAll(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	opts := &pebble.Options{MemTableSize: uint64(p.GetInt64(pebbleMemTableSz, pebbleMemTableSzD))}
	var cache *pebble.Cache
	if cacheSz := p.GetInt64(pebbleCacheSz, 0); cacheSz > 0 {
		cache = pebble.NewCache(cacheSz)
		opts.Cache = cache
	}
	db, err := pebble.Open(dir, opts)
	if cache != nil {
		// NewCache 引用计数为 1; Open 成功后 DB 持有一份引用, 创建者释放自己的
		cache.Unref()
	}
	if err != nil {
		return nil, err
	}

	return &pebbleDB{
		p:       p,
		db:      db,
		r:       util.NewRowCodec(p),
		bufPool: util.NewBufPool(),
		sync:    p.GetBool(pebbleSyncWrites, false),
	}, nil
}

func (db *pebbleDB) Close() error {
	return db.db.Close()
}

func (db *pebbleDB) InitThread(ctx context.Context, _ int, _ int) context.Context {
	return ctx
}

func (db *pebbleDB) CleanupThread(_ context.Context) {
}

func (db *pebbleDB) getRowKey(table string, key string) []byte {
	return util.Slice(fmt.Sprintf("%s:%s", table, key))
}

// Get 返回的 value 在 closer.Close() 后即失效, 先拷贝再解码
func (db *pebbleDB) readRow(key []byte) ([]byte, error) {
	value, closer, err := db.db.Get(key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closer.Close() }()

	row := make([]byte, len(value))
	copy(row, value)
	return row, nil
}

func (db *pebbleDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	row, err := db.readRow(db.getRowKey(table, key))
	if err != nil {
		return nil, err
	}
	return db.r.Decode(row, fields)
}

func (db *pebbleDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	rowStartKey := db.getRowKey(table, startKey)
	iter, err := db.db.NewIter(&pebble.IterOptions{LowerBound: rowStartKey})
	if err != nil {
		return nil, err
	}
	defer func() { _ = iter.Close() }()

	res := make([]map[string][]byte, 0, count)
	for iter.SeekGE(rowStartKey); iter.Valid() && len(res) < count; iter.Next() {
		// value 仅在下一次定位前有效, 拷贝后再解码
		value := iter.Value()
		row := make([]byte, len(value))
		copy(row, value)

		m, err := db.r.Decode(row, fields)
		if err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	return res, iter.Error()
}

func (db *pebbleDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	rowKey := db.getRowKey(table, key)

	row, err := db.readRow(rowKey)
	if err != nil {
		return err
	}

	data, err := db.r.Decode(row, nil)
	if err != nil {
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
		return err
	}

	return db.db.Set(rowKey, buf, &pebble.WriteOptions{Sync: db.sync})
}

func (db *pebbleDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	rowKey := db.getRowKey(table, key)

	buf := db.bufPool.Get()
	defer func() {
		db.bufPool.Put(buf)
	}()

	buf, err := db.r.Encode(buf, values)
	if err != nil {
		return err
	}

	return db.db.Set(rowKey, buf, &pebble.WriteOptions{Sync: db.sync})
}

func (db *pebbleDB) Delete(ctx context.Context, table string, key string) error {
	return db.db.Delete(db.getRowKey(table, key), &pebble.WriteOptions{Sync: db.sync})
}
