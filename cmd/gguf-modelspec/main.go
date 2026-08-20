// Command gguf-modelspec derives a partitioner ModelSpec from a GGUF header.
//
// Go replacement of tools/gguf_modelspec.py (Python purge). Reads ONLY the
// header metadata; never loads tensors.
//
// Usage:
//
//	gguf-modelspec /path/to/model.gguf [max_context] [kv_bytes]
//	gguf-modelspec --tensors /path/to/model.gguf
//
// kv_bytes: 2 = f16 KV, 1 = q8 KV.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/Custok/sofmat/internal/gguf"
)

func main() {
	args := make([]string, 0, len(os.Args)-1)
	tensors := false
	for _, a := range os.Args[1:] {
		if a == "--tensors" {
			tensors = true
		} else {
			args = append(args, a)
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gguf-modelspec [--tensors] <model.gguf> [max_context] [kv_bytes]")
		os.Exit(1)
	}
	if tensors {
		table, err := gguf.TensorTable(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		out, _ := json.MarshalIndent(table, "", " ")
		fmt.Println(string(out))
		return
	}
	maxContext, kvBytes := 8192, 2
	if len(args) > 1 {
		if v, err := strconv.Atoi(args[1]); err == nil {
			maxContext = v
		}
	}
	if len(args) > 2 {
		if v, err := strconv.Atoi(args[2]); err == nil {
			kvBytes = v
		}
	}
	spec, err := gguf.ModelSpec(args[0], maxContext, kvBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("architecture: %s\n", spec.Architecture)
	fmt.Printf("n_layers: %d\n", spec.NLayers)
	fmt.Printf("weights_gb: %.1f\n", spec.WeightsGB)
	fmt.Printf("kv_cache_gb: %.1f\n", spec.KVCacheGB)
	if spec.MaxContextTrained > 0 {
		fmt.Printf("max_context_trained: %d\n", spec.MaxContextTrained)
	}
	fmt.Printf("kv_assumptions: ctx=%d, kv_bytes=%d\n", maxContext, kvBytes)
}
