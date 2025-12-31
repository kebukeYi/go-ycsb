// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package badger

import (
	"context"
	"fmt"
	"os"

	"github.com/dgraph-io/badger/v4"
	badgeroptions "github.com/dgraph-io/badger/v4/options"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/util"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// properties
const (
	badgerDir               = "badger.dir"
	badgerValueDir          = "badger.valuedir"
	badgerSyncWrites        = "badger.sync_writes"
	badgerNumVersionsToKeep = "badger.num_versions_to_keep"
	badgerMemTableSize      = "badger.memtable_size"
	badgerValueThreshold    = "badger.value_threshold"
	badgerNumCompactors     = "badger.num_compactors"
	badgerCompression       = "badger.compression" // none|snappy|zstd
)

type badgerCreator struct {
}

func init() {
	ycsb.RegisterDBCreator("badger", badgerCreator{})
}

type badgerDB struct {
	p *properties.Properties

	db *badger.DB

	r       *util.RowCodec
	bufPool *util.BufPool
}

type contextKey string

const stateKey = contextKey("badgerDB")

type badgerState struct {
}

func (c badgerCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	opts := getOptions(p)

	if p.GetBool(prop.DropData, prop.DropDataDefault) {
		os.RemoveAll(opts.Dir)
		os.RemoveAll(opts.ValueDir)
	}

	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}

	return &badgerDB{
		p:       p,
		db:      db,
		r:       util.NewRowCodec(p),
		bufPool: util.NewBufPool(),
	}, nil
}

func getOptions(p *properties.Properties) badger.Options {
	dir := p.GetString(badgerDir, "/tmp/badger")
	opts := badger.DefaultOptions(dir)
	opts.ValueDir = p.GetString(badgerValueDir, dir)
	opts.SyncWrites = p.GetBool(badgerSyncWrites, opts.SyncWrites)
	opts.NumVersionsToKeep = p.GetInt(badgerNumVersionsToKeep, opts.NumVersionsToKeep)
	opts.MemTableSize = p.GetInt64(badgerMemTableSize, opts.MemTableSize)
	// v4 默认阈值为 1MB(行进 LSM 内联); 复现 v1(32B)/trainkv 的 vlog 布局时显式设 32
	opts.ValueThreshold = p.GetInt64(badgerValueThreshold, opts.ValueThreshold)
	opts.NumCompactors = p.GetInt(badgerNumCompactors, opts.NumCompactors)
	switch p.GetString(badgerCompression, "") {
	case "none":
		opts.Compression = badgeroptions.None
	case "snappy":
		opts.Compression = badgeroptions.Snappy
	case "zstd":
		opts.Compression = badgeroptions.ZSTD
	default:
		// 空值走引擎默认(v4 为 Snappy); 与 v1 行为一致需显式传 none
	}
	return opts
}

func (db *badgerDB) Close() error {
	return db.db.Close()
}

func (db *badgerDB) InitThread(ctx context.Context, _ int, _ int) context.Context {
	return ctx
}

func (db *badgerDB) CleanupThread(_ context.Context) {
}

func (db *badgerDB) getRowKey(table string, key string) []byte {
	return util.Slice(fmt.Sprintf("%s:%s", table, key))
}

func (db *badgerDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	var m map[string][]byte
	err := db.db.View(func(txn *badger.Txn) error {
		rowKey := db.getRowKey(table, key)
		item, err := txn.Get(rowKey)
		if err != nil {
			return err
		}
		row, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}

		m, err = db.r.Decode(row, fields)
		return err
	})

	return m, err
}

func (db *badgerDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	res := make([]map[string][]byte, count)
	err := db.db.View(func(txn *badger.Txn) error {
		rowStartKey := db.getRowKey(table, startKey)
		opts := badger.DefaultIteratorOptions
		if count <= 1 {
			// badger 开启 PrefetchValues 时会对每个预取 item 起一个 goroutine 读 value;
			// 只取 1 行时预取(最多 PrefetchSize 行)纯属浪费, 关闭后按需惰性读取;
			opts.PrefetchValues = false
		}
		it := txn.NewIterator(opts)
		defer it.Close()

		i := 0
		for it.Seek(rowStartKey); it.Valid() && i < count; it.Next() {
			item := it.Item()
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			m, err := db.r.Decode(value, fields)
			if err != nil {
				return err
			}

			res[i] = m
			i++
		}

		return nil
	})

	return res, err
}

func (db *badgerDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	err := db.db.Update(func(txn *badger.Txn) error {
		rowKey := db.getRowKey(table, key)
		item, err := txn.Get(rowKey)
		if err != nil {
			return err
		}

		value, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}

		data, err := db.r.Decode(value, nil)
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
		return txn.Set(rowKey, buf)
	})
	return err
}

func (db *badgerDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	err := db.db.Update(func(txn *badger.Txn) error {
		rowKey := db.getRowKey(table, key)

		buf := db.bufPool.Get()
		defer func() {
			db.bufPool.Put(buf)
		}()

		buf, err := db.r.Encode(buf, values)
		if err != nil {
			return err
		}
		return txn.Set(rowKey, buf)
	})

	return err
}

func (db *badgerDB) Delete(ctx context.Context, table string, key string) error {
	err := db.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(db.getRowKey(table, key))
	})

	return err
}
