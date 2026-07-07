package vector

import "io"

// VectorIndex — общий интерфейс для векторных индексов. Две реализации с разными
// ролями (обе живые, это НЕ legacy-пара):
//
//   - LeveledVectorStore — движок данных (VSIM.* / WAL / снапшоты): LSM-уровни,
//     сегменты, durability, фильтры/тенанты.
//   - VectorStore — лёгкий одно-графовый индекс без сегментов и durability;
//     в проде держит семантический индекс Pub/Sub-хаба (эфемерные паттерны
//     подписок, умирают вместе с процессом — LSM-машинерия там не нужна).
//
// Общий контракт закреплён конформанс-тестом TestVectorIndex_Conformance
// (index_conformance_test.go) — багфиксы семантики интерфейса должны проходить
// в обеих реализациях.
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
