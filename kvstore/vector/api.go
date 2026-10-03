package vector

import "io"

// VectorIndex — контракт векторного индекса, на который опираются сервер,
// TTL-эвиктор и кластерный слой. Реализация — LeveledVectorStore (LSM-уровни,
// сегменты, durability, фильтры/тенанты).
//
// Контракт закреплён конформанс-тестом TestVectorIndex_Conformance
// (index_conformance_test.go): один сценарий на обоих путях публикации
// сегментов — одношардовой дельте и freeze-per-shard.
type VectorIndex interface {
	Add(key string, vec []float32) error
	Delete(key string) bool
	Search(query []float32, K int, dst []VSearchResult) ([]VSearchResult, error)
	SearchFiltered(query []float32, K int, filterFn func(string) bool, dst []VSearchResult) ([]VSearchResult, error)
	ForEach(fn func(key string, vec []float32))
	Get(key string) ([]float32, bool)
	// Info: count — число живых векторов. У LSM-реализации (LeveledVectorStore)
	// upsert поверх frozen-копии до компакции временно завышает count на число
	// затенённых копий; count никогда не занижается. Delete уменьшает count сразу.
	Info() (count, dim, maxLevel int)
	Clear()
	SaveBinary(w io.Writer) error
	LoadBinary(r io.Reader) error
}
