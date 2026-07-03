package vector

import (
	"errors"
	"math"
)

// MaxKeyLen — максимальная длина ключа вектора в байтах. Ключ длиннее не
// помещается в uint16-поле длины бинарного снапшота (frozen/frozen_sq/
// snapshot_binary/leveled_store — 5 мест PutUint16) и молча усекается при
// сохранении, ломая формат. Отсекаем на входе.
const MaxKeyLen = 65535

// MaxSearchK — верхний кап на K в поиске. Search раздувает окно поиска до K
// (efSearch = max(EfSearch, K), leveled_store.go), поэтому K без капа = OOM
// одним маленьким запросом (DoS). Кап отсекает до аллокации кандидатных куч.
const MaxSearchK = 100_000

var (
	// ErrEmptyVector — вектор нулевой длины. Пустой вектор не несёт размерности:
	// на первой вставке deltaMax(dim=0) даёт деление на ноль (crash-loop через
	// отравленный WAL). Отсекаем до записи в WAL.
	ErrEmptyVector = errors.New("empty vector")
	// ErrInvalidVectorValue — NaN/Inf в компоненте. Отравляет калибровку SQ8
	// целого сегмента и портит расстояния HNSW.
	ErrInvalidVectorValue = errors.New("vector contains NaN or Inf")
	// ErrEmptyKey — пустой ключ. В дельте "" — сентинел tombstone (delta.go),
	// поэтому вектор с пустым ключом молча считался бы удалённым.
	ErrEmptyKey = errors.New("empty key")
	// ErrKeyTooLong — ключ длиннее MaxKeyLen (см. коммент к константе).
	ErrKeyTooLong = errors.New("key too long")
)

// ValidateVector проверяет вектор перед вставкой: непустой и без NaN/Inf.
// Чистая функция — зовётся и на границе команды (до записи в WAL), и в сторе
// (defense-in-depth: реплей отравленного WAL и прочие вызывающие получают
// ошибку вместо паники/тихой порчи).
func ValidateVector(vec []float32) error {
	if len(vec) == 0 {
		return ErrEmptyVector
	}
	for _, v := range vec {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return ErrInvalidVectorValue
		}
	}
	return nil
}

// ValidateKey проверяет ключ вектора: непустой и не длиннее MaxKeyLen.
func ValidateKey(key string) error {
	if key == "" {
		return ErrEmptyKey
	}
	if len(key) > MaxKeyLen {
		return ErrKeyTooLong
	}
	return nil
}
