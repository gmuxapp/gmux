package main

import (
	"math"
	"reflect"
	"sort"
	"sync"
	"unsafe"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// PROTO (3.0 round 2, D4 interim): the ring fingerprints rows to find what
// changed. The spike marshalled each row to JSON (~5 µs/row); with many
// viewports open that re-hashes most of the corpus per broadcast and the
// roots-only saving evaporates (review M4). Until the store carries a per-row
// revision (the real fix: O(1)/row, no hashing at all), fingerprint by
// walking the struct reflectively: every exported field of every nesting
// level feeds the hash, so a new wire field can never be silently skipped
// the way a hand-written field list could. Unexported fields are ignored,
// exactly as encoding/json ignores them.
//
// Allocation-free: FNV-1a is folded inline into a uint64 accumulator, strings
// are read in place, and each struct type's exported field indices are
// computed once. Type tags separate kinds and lengths frame variable-size
// values, so two different rows cannot collide by concatenation.

const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

type fp uint64

func (h *fp) byte(b byte) { *h = (*h ^ fp(b)) * fnvPrime64 }
func (h *fp) u64(v uint64) {
	for i := 0; i < 8; i++ {
		h.byte(byte(v >> (8 * i)))
	}
}
func (h *fp) str(s string) {
	h.u64(uint64(len(s)))
	if len(s) == 0 {
		return
	}
	for _, b := range unsafe.Slice(unsafe.StringData(s), len(s)) {
		h.byte(b)
	}
}

var exportedFields sync.Map // reflect.Type -> []int

func exportedFieldIndices(t reflect.Type) []int {
	if v, ok := exportedFields.Load(t); ok {
		return v.([]int)
	}
	var idx []int
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			idx = append(idx, i)
		}
	}
	exportedFields.Store(t, idx)
	return idx
}

func fingerprintSession(s wire.Session) uint64 {
	h := fp(fnvOffset64)
	hashValue(&h, reflect.ValueOf(&s).Elem())
	return uint64(h)
}

func hashValue(h *fp, v reflect.Value) {
	switch v.Kind() {
	case reflect.Invalid:
		h.byte(0)
	case reflect.Bool:
		h.byte(1)
		if v.Bool() {
			h.byte(1)
		} else {
			h.byte(0)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		h.byte(2)
		h.u64(uint64(v.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		h.byte(3)
		h.u64(v.Uint())
	case reflect.Float32, reflect.Float64:
		h.byte(4)
		h.u64(math.Float64bits(v.Float()))
	case reflect.String:
		h.byte(5)
		h.str(v.String())
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			h.byte(6)
			return
		}
		h.byte(7)
		hashValue(h, v.Elem())
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			h.byte(8)
			return
		}
		h.byte(9)
		n := v.Len()
		h.u64(uint64(n))
		for i := 0; i < n; i++ {
			hashValue(h, v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() {
			h.byte(10)
			return
		}
		h.byte(11)
		h.u64(uint64(v.Len()))
		if v.Type().Key().Kind() == reflect.String {
			keys := make([]string, 0, v.Len())
			iter := v.MapRange()
			for iter.Next() {
				keys = append(keys, iter.Key().String())
			}
			sort.Strings(keys)
			for _, k := range keys {
				h.str(k)
				hashValue(h, v.MapIndex(reflect.ValueOf(k)))
			}
			return
		}
		// Non-string keys do not occur on wire rows; hash order-insensitively.
		var acc uint64
		iter := v.MapRange()
		for iter.Next() {
			e := fp(fnvOffset64)
			hashValue(&e, iter.Key())
			hashValue(&e, iter.Value())
			acc += uint64(e)
		}
		h.u64(acc)
	case reflect.Struct:
		h.byte(12)
		for _, i := range exportedFieldIndices(v.Type()) {
			hashValue(h, v.Field(i))
		}
	default:
		h.byte(13)
	}
}
