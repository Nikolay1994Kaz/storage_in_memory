package vector

import "io"

// VectorIndex — общий интерфейс для векторных индексов (VectorStore и LeveledVectorStore).
type VectorIndex interface {
	Add(key string, vec []float32) error
	Delete(key string) bool
	Search(query []float32, K int, dst []VSearchResult) ([]VSearchResult, error)
	SearchFiltered(query []float32, K int, filterFn func(string) bool, dst []VSearchResult) ([]VSearchResult, error)
	ForEach(fn func(key string, vec []float32))
	Get(key string) ([]float32, bool)
	Info() (count, dim, maxLevel int)
	Clear()
	SaveBinary(w io.Writer) error
	LoadBinary(r io.Reader) error
}
