// Contract tests for the gguf_modelspec.py port. A tiny synthetic GGUF v3
// file is built in-test (fictitious values), so no real model is needed.
package gguf

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type ggufWriter struct{ buf bytes.Buffer }

func (w *ggufWriter) u32(v uint32) { binary.Write(&w.buf, binary.LittleEndian, v) }
func (w *ggufWriter) u64(v uint64) { binary.Write(&w.buf, binary.LittleEndian, v) }
func (w *ggufWriter) str(s string) { w.u64(uint64(len(s))); w.buf.WriteString(s) }
func (w *ggufWriter) kvU32(k string, v uint32) {
	w.str(k)
	w.u32(4) // type u32
	w.u32(v)
}
func (w *ggufWriter) kvStr(k, v string) {
	w.str(k)
	w.u32(8) // type string
	w.str(v)
}

// writeTestGGUF builds a v3 file: arch "testarch", 4 layers, kv-heads 2,
// head_dim 8 (key_length), 2 tensors of 64 and 32 data bytes, alignment 32.
func writeTestGGUF(t *testing.T, path string) {
	t.Helper()
	var w ggufWriter
	w.buf.WriteString("GGUF")
	w.u32(3) // version
	w.u64(2) // n_tensors
	w.u64(6) // n_kv
	w.kvStr("general.architecture", "testarch")
	w.kvU32("general.alignment", 32)
	w.kvU32("testarch.block_count", 4)
	w.kvU32("testarch.attention.head_count_kv", 2)
	w.kvU32("testarch.attention.key_length", 8)
	w.kvU32("testarch.context_length", 4096)
	// tensor infos: (name, n_dims, dims, ggml type, rel offset)
	w.str("t0")
	w.u32(1)
	w.u64(16) // dims
	w.u32(0)  // ggml type f32
	w.u64(0)  // rel offset
	w.str("t1")
	w.u32(1)
	w.u64(8)
	w.u32(0)
	w.u64(64)
	// pad to alignment 32, then 64+32 data bytes
	pos := w.buf.Len()
	dataStart := (pos + 31) / 32 * 32
	w.buf.Write(make([]byte, dataStart-pos))
	w.buf.Write(make([]byte, 96))
	if err := os.WriteFile(path, w.buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModelSpecFromHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	writeTestGGUF(t, path)
	spec, err := ModelSpec(path, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Architecture != "testarch" || spec.NLayers != 4 {
		t.Fatalf("header mismatch: %+v", spec)
	}
	// kv = layers(4) * 2 * kv_heads(2) * head_dim(8) * kv_bytes(2) * ctx(1000) / 1e9
	want := 4.0 * 2 * 2 * 8 * 2 * 1000 / 1e9
	if math.Abs(spec.KVCacheGB-want) > 1e-12 {
		t.Fatalf("kv_cache_gb %v != %v", spec.KVCacheGB, want)
	}
	if spec.MaxContextTrained != 4096 {
		t.Fatalf("context_length mismatch: %+v", spec)
	}
	st, _ := os.Stat(path)
	if math.Abs(spec.WeightsGB-float64(st.Size())/1e9) > 1e-12 {
		t.Fatalf("weights_gb must be the file size: %+v", spec)
	}
}

func TestTensorTableOffsetsExact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	writeTestGGUF(t, path)
	table, err := TensorTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 2 {
		t.Fatalf("want 2 tensors, got %d", len(table))
	}
	if table[0].Name != "t0" || table[0].NBytes != 64 {
		t.Fatalf("t0 mismatch: %+v", table[0])
	}
	if table[1].Name != "t1" || table[1].NBytes != 32 {
		t.Fatalf("t1 mismatch: %+v", table[1])
	}
	if table[1].Offset != table[0].Offset+64 {
		t.Fatalf("offsets not contiguous: %+v", table)
	}
	if table[0].Offset%32 != 0 {
		t.Fatalf("data start must honor alignment: %+v", table[0])
	}
}

func TestNotAGGUFFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.gguf")
	if err := os.WriteFile(path, []byte("not a gguf at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ModelSpec(path, 8192, 2); err == nil {
		t.Fatal("must refuse a non-GGUF file")
	}
}

func TestShardedWeightsSummed(t *testing.T) {
	dir := t.TempDir()
	shard1 := filepath.Join(dir, "model-00001-of-00002.gguf")
	shard2 := filepath.Join(dir, "model-00002-of-00002.gguf")
	writeTestGGUF(t, shard1)
	if err := os.WriteFile(shard2, make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := ModelSpec(shard1, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(shard1)
	want := float64(st.Size()+500) / 1e9
	if math.Abs(spec.WeightsGB-want) > 1e-12 {
		t.Fatalf("sharded weights %v != %v", spec.WeightsGB, want)
	}
	if err := os.Remove(shard2); err != nil {
		t.Fatal(err)
	}
	if _, err := ModelSpec(shard1, 1000, 2); err == nil {
		t.Fatal("incomplete shard set must fail closed")
	}
}
