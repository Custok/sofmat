// Package gguf derives a partitioner ModelSpec from a GGUF header.
//
// Go port of tools/gguf_modelspec.py (removed in the Python purge; this
// package is its replacement). Reads ONLY the GGUF metadata (a few KB):
// never loads tensors, never touches GPU. The output feeds the config's
// model section / partitioner.ModelSpec directly.
package gguf

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Error is any GGUF parsing/derivation failure.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errf(format string, args ...any) *Error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// maxStoredArray mirrors the Python max_array: larger metadata arrays are
// consumed (to keep the stream aligned) but not stored.
const maxStoredArray = 32

type header struct {
	meta      map[string]any
	infos     []tensorRef // (name, relative offset) in file order
	dataStart int64
}

type tensorRef struct {
	name string
	rel  uint64
}

func parseHeader(path string) (*header, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errf("gguf: %v", err)
	}
	defer f.Close()

	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != "GGUF" {
		return nil, errf("%s: not a GGUF file", path)
	}
	var version uint32
	if err := binary.Read(f, binary.LittleEndian, &version); err != nil {
		return nil, errf("%s: truncated header", path)
	}
	if version < 2 {
		return nil, errf("unsupported GGUF version %d", version)
	}
	var nTensors, nKV uint64
	if err := binary.Read(f, binary.LittleEndian, &nTensors); err != nil {
		return nil, errf("%s: truncated header", path)
	}
	if err := binary.Read(f, binary.LittleEndian, &nKV); err != nil {
		return nil, errf("%s: truncated header", path)
	}

	rdStr := func() (string, error) {
		var n uint64
		if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
			return "", err
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(f, b); err != nil {
			return "", err
		}
		return string(b), nil
	}

	var rdVal func(t uint32) (any, int, error) // value, array length (1 for scalars)
	rdVal = func(t uint32) (any, int, error) {
		read := func(v any) (any, int, error) {
			if err := binary.Read(f, binary.LittleEndian, v); err != nil {
				return nil, 0, err
			}
			return v, 1, nil
		}
		switch t {
		case 0:
			v, n, err := read(new(uint8))
			return *(v.(*uint8)), n, err
		case 1:
			v, n, err := read(new(int8))
			return *(v.(*int8)), n, err
		case 2:
			v, n, err := read(new(uint16))
			return *(v.(*uint16)), n, err
		case 3:
			v, n, err := read(new(int16))
			return *(v.(*int16)), n, err
		case 4:
			v, n, err := read(new(uint32))
			return *(v.(*uint32)), n, err
		case 5:
			v, n, err := read(new(int32))
			return *(v.(*int32)), n, err
		case 6:
			v, n, err := read(new(float32))
			return *(v.(*float32)), n, err
		case 7:
			v, n, err := read(new(bool))
			return *(v.(*bool)), n, err
		case 8:
			s, err := rdStr()
			return s, 1, err
		case 9:
			var et uint32
			if err := binary.Read(f, binary.LittleEndian, &et); err != nil {
				return nil, 0, err
			}
			var n uint64
			if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
				return nil, 0, err
			}
			arr := make([]any, 0, min(int(n), maxStoredArray))
			for i := uint64(0); i < n; i++ {
				v, _, err := rdVal(et)
				if err != nil {
					return nil, 0, err
				}
				if i < maxStoredArray {
					arr = append(arr, v)
				}
			}
			return arr, int(n), nil
		case 10:
			v, n, err := read(new(uint64))
			return *(v.(*uint64)), n, err
		case 11:
			v, n, err := read(new(int64))
			return *(v.(*int64)), n, err
		case 12:
			v, n, err := read(new(float64))
			return *(v.(*float64)), n, err
		default:
			return nil, 0, errf("unknown GGUF value type %d", t)
		}
	}

	meta := map[string]any{}
	for i := uint64(0); i < nKV; i++ {
		key, err := rdStr()
		if err != nil {
			return nil, errf("%s: truncated metadata", path)
		}
		var t uint32
		if err := binary.Read(f, binary.LittleEndian, &t); err != nil {
			return nil, errf("%s: truncated metadata", path)
		}
		val, n, err := rdVal(t)
		if err != nil {
			return nil, errf("%s: metadata %s: %v", path, key, err)
		}
		if n <= maxStoredArray {
			meta[key] = val
		}
	}

	infos := make([]tensorRef, 0, nTensors)
	for i := uint64(0); i < nTensors; i++ {
		name, err := rdStr()
		if err != nil {
			return nil, errf("%s: truncated tensor infos", path)
		}
		var nDims uint32
		if err := binary.Read(f, binary.LittleEndian, &nDims); err != nil {
			return nil, errf("%s: truncated tensor infos", path)
		}
		if _, err := f.Seek(int64(8*nDims)+4, io.SeekCurrent); err != nil { // dims + ggml type
			return nil, errf("%s: truncated tensor infos", path)
		}
		var rel uint64
		if err := binary.Read(f, binary.LittleEndian, &rel); err != nil {
			return nil, errf("%s: truncated tensor infos", path)
		}
		infos = append(infos, tensorRef{name: name, rel: rel})
	}

	alignment := asInt(meta["general.alignment"], 32)
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, errf("%s: %v", path, err)
	}
	dataStart := (pos + alignment - 1) / alignment * alignment
	return &header{meta: meta, infos: infos, dataStart: dataStart}, nil
}

// asInt coerces the numeric GGUF metadata types onto int64.
func asInt(v any, def int64) int64 {
	switch x := v.(type) {
	case uint8:
		return int64(x)
	case int8:
		return int64(x)
	case uint16:
		return int64(x)
	case int16:
		return int64(x)
	case uint32:
		return int64(x)
	case int32:
		return int64(x)
	case uint64:
		return int64(x)
	case int64:
		return x
	default:
		return def
	}
}

// ReadMetadata parses the GGUF header and returns only the key-value
// metadata (large arrays elided, like the Python tool).
func ReadMetadata(path string) (map[string]any, error) {
	h, err := parseHeader(path)
	if err != nil {
		return nil, err
	}
	return h.meta, nil
}

// TensorInfo is one tensor's layout: absolute file offset (ready for
// seek / HTTP Range) and exact byte size derived from offset deltas.
type TensorInfo struct {
	Name   string
	Offset int64
	NBytes int64
}

// TensorTable returns the tensor layout of one GGUF file in offset order.
func TensorTable(path string) ([]TensorInfo, error) {
	h, err := parseHeader(path)
	if err != nil {
		return nil, err
	}
	if len(h.infos) == 0 {
		return nil, errf("%s: no tensors in header", path)
	}
	ordered := append([]tensorRef(nil), h.infos...)
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].rel < ordered[b].rel })
	st, err := os.Stat(path)
	if err != nil {
		return nil, errf("gguf: %v", err)
	}
	table := make([]TensorInfo, 0, len(ordered))
	for i, t := range ordered {
		start := h.dataStart + int64(t.rel)
		end := st.Size()
		if i+1 < len(ordered) {
			end = h.dataStart + int64(ordered[i+1].rel)
		}
		if end <= start {
			return nil, errf("%s: non-monotonic tensor offsets at %s", path, t.name)
		}
		table = append(table, TensorInfo{Name: t.name, Offset: start, NBytes: end - start})
	}
	return table, nil
}

var shardRe = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// weightsBytes totals the file plus its sibling shards when the model is
// split (model-00001-of-00004.gguf style). Pass ANY shard.
func weightsBytes(path string) (int64, error) {
	base := filepath.Base(path)
	m := shardRe.FindStringSubmatch(base)
	if m == nil {
		st, err := os.Stat(path)
		if err != nil {
			return 0, errf("gguf: %v", err)
		}
		return st.Size(), nil
	}
	stem, nShards := m[1], m[3]
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, errf("gguf: %v", err)
	}
	folder := filepath.Dir(abs)
	var n int
	fmt.Sscanf(nShards, "%d", &n)
	total := int64(0)
	for i := 1; i <= n; i++ {
		shard := filepath.Join(folder, fmt.Sprintf("%s-%05d-of-%s.gguf", stem, i, nShards))
		st, err := os.Stat(shard)
		if err != nil {
			return 0, errf("sharded model incomplete: missing %s", shard)
		}
		total += st.Size()
	}
	return total, nil
}

// Spec is the solver-facing model description derived from the header.
type Spec struct {
	Architecture      string
	NLayers           int
	WeightsGB         float64
	KVCacheGB         float64
	MaxContextTrained int64 // 0 = header does not declare it
}

// ModelSpec derives the partitioner inputs. kvBytes: 2 = f16 KV, 1 = q8 KV.
// KV budget = n_layers * 2 (K+V) * n_kv_heads * head_dim * kvBytes * maxContext.
func ModelSpec(path string, maxContext int, kvBytes int) (Spec, error) {
	var zero Spec
	meta, err := ReadMetadata(path)
	if err != nil {
		return zero, err
	}
	arch, _ := meta["general.architecture"].(string)
	if arch == "" {
		return zero, errf("general.architecture missing from header")
	}
	get := func(key string) (int64, bool) {
		v, ok := meta[arch+"."+key]
		if !ok {
			return 0, false
		}
		return asInt(v, 0), true
	}
	nLayers, ok := get("block_count")
	if !ok {
		return zero, errf("%s.block_count missing from header", arch)
	}
	headDim, ok := get("attention.key_length")
	if !ok {
		embed, ok1 := get("embedding_length")
		heads, ok2 := get("attention.head_count")
		if !ok1 || !ok2 || heads == 0 {
			return zero, errf("%s: cannot derive head_dim (need attention.key_length or embedding_length/attention.head_count)", arch)
		}
		headDim = embed / heads
	}
	nKVHeads, ok := get("attention.head_count_kv")
	if !ok || nKVHeads == 0 {
		nKVHeads, ok = get("attention.head_count")
		if !ok {
			return zero, errf("%s.attention.head_count missing from header", arch)
		}
	}
	wb, err := weightsBytes(path)
	if err != nil {
		return zero, err
	}
	kvGB := float64(nLayers) * 2 * float64(nKVHeads) * float64(headDim) *
		float64(kvBytes) * float64(maxContext) / 1e9
	trained, _ := get("context_length")
	return Spec{
		Architecture:      arch,
		NLayers:           int(nLayers),
		WeightsGB:         float64(wb) / 1e9,
		KVCacheGB:         kvGB,
		MaxContextTrained: trained,
	}, nil
}
