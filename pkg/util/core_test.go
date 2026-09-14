package util

import (
	"math/rand"
	"reflect"
	"strconv"
	"testing"

	"github.com/magiconair/properties"

	"github.com/pingcap/go-ycsb/pkg/prop"
)

func TestFieldPair(t *testing.T) {
	m := map[string][]byte{
		"f2": []byte("b"),
		"f1": []byte("a"),
	}

	p := NewFieldPairs(m)

	check := FieldPairs{
		{"f1", []byte("a")},
		{"f2", []byte("b")},
	}

	if !reflect.DeepEqual(p, check) {
		t.Errorf("want %v, but got %v", check, p)
	}
}

// TestRowCodecDecodeMatchesLegacy 保证单趟解析路径与旧的 map 路径语义完全一致:
// 覆盖字段顺序/重复/未知字段名、空行、空 value、超范围列号、以及 fieldcount>64 的回退场景.
func TestRowCodecDecodeMatchesLegacy(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))

	for _, fieldCount := range []int{10, 100} {
		p := properties.NewProperties()
		if _, _, err := p.Set(prop.FieldCount, strconv.Itoa(fieldCount)); err != nil {
			t.Fatal(err)
		}
		codec := NewRowCodec(p)
		allFields := codec.fields

		fieldSets := [][]string{
			nil,
			allFields,
			allFields[:len(allFields)/2],
			{allFields[0], allFields[0]},
			{"notExist", allFields[1]},
		}
		if len(allFields) > 3 {
			fieldSets = append(fieldSets, []string{allFields[2], allFields[0], allFields[1]})
		}

		check := func(row []byte) {
			t.Helper()
			for _, fields := range fieldSets {
				// Decode 对 nil fields 会替换为全字段; legacy 调用需要同样的输入;
				legacyFields := fields
				if len(legacyFields) == 0 {
					legacyFields = allFields
				}
				got, errGot := codec.Decode(row, fields)
				want, errWant := codec.decodeLegacy(row, legacyFields)
				if errGot != nil || errWant != nil {
					t.Fatalf("decode error mismatch: new=%v legacy=%v (fieldCount=%d)",
						errGot, errWant, fieldCount)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("decode mismatch (fieldCount=%d, fields=%v):\n new=%v\n old=%v",
						fieldCount, fields, got, want)
				}
			}
		}

		// 空行/占位空行;
		check(nil)
		check([]byte{0})

		for iter := 0; iter < 200; iter++ {
			nCols := rnd.Intn(fieldCount) + 1
			cols := make([][]byte, 0, nCols)
			colIDs := make([]int64, 0, nCols)
			used := make(map[int64]bool, nCols)
			for len(cols) < nCols {
				id := int64(rnd.Intn(fieldCount))
				if used[id] {
					continue
				}
				used[id] = true
				val := make([]byte, rnd.Intn(120))
				_, _ = rnd.Read(val)
				cols = append(cols, val)
				colIDs = append(colIDs, id)
			}
			row, err := EncodeRow(cols, colIDs, nil)
			if err != nil {
				t.Fatal(err)
			}
			check(row)
		}

		// 超范围列号: 新路径应回退到 legacy, 语义一致;
		row, err := EncodeRow([][]byte{[]byte("v0"), []byte("v1")}, []int64{0, 100}, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(row)
	}
}
