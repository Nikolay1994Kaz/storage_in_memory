//go:build !experimental

package main

// Прод-сборка: вместо реального WASM-адаптера подставляется nopCompute
// (см. compute_nop.go) — internal/compute и wazero в бинарь не линкуются.
func newComputeEngine(computeDeps) computeRuntime { return nopCompute{} }
