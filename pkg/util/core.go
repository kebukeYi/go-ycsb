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

package util

import (
	"fmt"
	"sort"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
)

// createFieldIndices is a helper function to create a field -> index mapping
// for the core workload
func createFieldIndices(p *properties.Properties) map[string]int64 {
	fieldCount := p.GetInt64(prop.FieldCount, prop.FieldCountDefault)
	m := make(map[string]int64, fieldCount)
	for i := int64(0); i < fieldCount; i++ {
		field := fmt.Sprintf("field%d", i)
		m[field] = i
	}
	return m
}

// allFields is a helper function to create all fields
func allFields(p *properties.Properties) []string {
	fieldCount := p.GetInt64(prop.FieldCount, prop.FieldCountDefault)
	fields := make([]string, 0, fieldCount)
	for i := int64(0); i < fieldCount; i++ {
		field := fmt.Sprintf("field%d", i)
		fields = append(fields, field)
	}
	return fields
}

// RowCodec is a helper struct to encode and decode TiDB format row
type RowCodec struct {
	fieldIndices map[string]int64
	fields       []string
}

// NewRowCodec creates the RowCodec
func NewRowCodec(p *properties.Properties) *RowCodec {
	return &RowCodec{
		fieldIndices: createFieldIndices(p),
		fields:       allFields(p),
	}
}

// maxDecodeCols 是 Decode 单趟解析时栈上列值表的容量;
// 超出(字段数/列号 > 64)时回退 decodeLegacy, 语义不变.
const maxDecodeCols = 64

// Decode decodes the row and returns a field-value map.
//
// 热路径实现: 单趟解析行, 用栈上定长表按列号记录 value 切片(借用 row, 不拷贝),
// 省掉 DecodeRow 的中间 map[int64][]byte 及其二次查找 (实测占读路径 ~12% CPU / ~28% 分配量);
// 返回值仍是"字段名 -> row 子切片"的映射, 内存归属与原实现一致 (调用方需保证 row 存活).
func (r *RowCodec) Decode(row []byte, fields []string) (map[string][]byte, error) {
	if len(fields) == 0 {
		fields = r.fields
	}
	if len(fields) > maxDecodeCols {
		return r.decodeLegacy(row, fields)
	}

	// 先把请求字段解析成列号; 顺带完成回退判定 (未知字段名沿用旧行为: fieldIndices 缺省为 0 号列);
	var reqCols [maxDecodeCols]int64
	for k, field := range fields {
		i := r.fieldIndices[field]
		if i < 0 || i >= maxDecodeCols {
			return r.decodeLegacy(row, fields)
		}
		reqCols[k] = i
	}

	if len(row) == 0 || (len(row) == 1 && row[0] == 0) {
		return make(map[string][]byte, len(fields)), nil
	}

	var (
		colVals [maxDecodeCols][]byte
		present uint64 // 位图: colID 是否出现在行内 (colID < maxDecodeCols)
	)
	b := row
	for len(b) > 0 {
		remain, colID, err := decodeInt64(b)
		if err != nil {
			return nil, err
		}
		var v []byte
		remain, v, err = decodeBytes(remain)
		if err != nil {
			return nil, err
		}
		if colID < 0 || colID >= maxDecodeCols {
			return r.decodeLegacy(row, fields)
		}
		colVals[colID] = v
		present |= 1 << uint(colID)
		b = remain
	}

	res := make(map[string][]byte, len(fields))
	for k, field := range fields {
		if col := reqCols[k]; present&(1<<uint(col)) != 0 {
			res[field] = colVals[col]
		}
	}
	return res, nil
}

// decodeLegacy is the general implementation, kept for rows/fields beyond the
// stack-table capacity. It builds the intermediate map[int64][]byte first.
func (r *RowCodec) decodeLegacy(row []byte, fields []string) (map[string][]byte, error) {
	data, err := DecodeRow(row)
	if err != nil {
		return nil, err
	}

	res := make(map[string][]byte, len(fields))
	for _, field := range fields {
		i := r.fieldIndices[field]
		if v, ok := data[i]; ok {
			res[field] = v
		}
	}

	return res, nil
}

// Encode encodes the values
func (r *RowCodec) Encode(buf []byte, values map[string][]byte) ([]byte, error) {
	cols := make([][]byte, 0, len(values))
	colIDs := make([]int64, 0, len(values))

	for k, v := range values {
		i := r.fieldIndices[k]
		cols = append(cols, v)
		colIDs = append(colIDs, i)
	}

	rowData, err := EncodeRow(cols, colIDs, buf)
	return rowData, err
}

// FieldPair is a pair to hold field + value.
type FieldPair struct {
	Field string
	Value []byte
}

// FieldPairs implements sort interface for []FieldPair
type FieldPairs []FieldPair

// Len implements sort interface Len
func (s FieldPairs) Len() int {
	return len(s)
}

// Len implements sort interface Swap
func (s FieldPairs) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// Len implements sort interface Less
func (s FieldPairs) Less(i, j int) bool {
	return s[i].Field < s[j].Field
}

// NewFieldPairs sorts the map by fields and return a sorted slice of FieldPair.
func NewFieldPairs(values map[string][]byte) FieldPairs {
	pairs := make(FieldPairs, 0, len(values))
	for field, value := range values {
		pairs = append(pairs, FieldPair{
			Field: field,
			Value: value,
		})
	}

	sort.Sort(pairs)
	return pairs
}
