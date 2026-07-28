// Пакет keyring пока пуст: это ЗАМЕР ЦЕНЫ ДО КОДА (дисциплина «эксперимент до
// реализации с порогами»). Меряется не наша обёртка, а голый примитив
// crypto/aes+GCM на РЕАЛЬНЫХ размерах полезной нагрузки VMEM, чтобы решение
// «что шифровать» (П3) принималось по числу, а не по ощущению.
//
// Размеры взяты из фактического пути записи:
//   - якорь `vmem:<id>` — дословный текст факта (OpSet). Типичный факт памяти
//     агента — одно-два предложения; берём 128/512/2048 Б как вилку;
//   - вектор — 1536 float32 = 6144 Б (dim эмбеддинга по умолчанию);
//   - BM25-термы — стеммированные токены; порядок сотен байт на факт.
//
// ПОРОГ, НАЗНАЧЕННЫЙ ЗАРАНЕЕ (иначе замер бессмысленен): накладные расходы
// шифрования на путь RECALL считаем приемлемыми, если расшифровка K=10
// якорей добавляет < 5% к бюджету одного RECALL. Канон BM25-выдачи —
// SEARCHTEXT 7958 QPS, то есть ~126 мкс на запрос; 5% = 6.3 мкс на 10 якорей
// = 630 нс на якорь. Если не укладываемся — шифруем меньше или иначе.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"
)

func newGCM(b *testing.B) cipher.AEAD {
	b.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		b.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		b.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		b.Fatal(err)
	}
	return gcm
}

var sizes = []struct {
	name string
	n    int
}{
	{"anchor_128B", 128},
	{"anchor_512B", 512},
	{"anchor_2KB", 2048},
	{"terms_512B", 512},
	{"vector_6KB", 6144}, // 1536 × float32
}

func BenchmarkSeal(b *testing.B) {
	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			gcm := newGCM(b)
			pt := make([]byte, s.n)
			rand.Read(pt)
			nonce := make([]byte, gcm.NonceSize())
			rand.Read(nonce)
			dst := make([]byte, 0, len(nonce)+s.n+gcm.Overhead())
			b.SetBytes(int64(s.n))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = gcm.Seal(dst[:0], nonce, pt, nil)
			}
			_ = dst
		})
	}
}

func BenchmarkOpen(b *testing.B) {
	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			gcm := newGCM(b)
			pt := make([]byte, s.n)
			rand.Read(pt)
			nonce := make([]byte, gcm.NonceSize())
			rand.Read(nonce)
			ct := gcm.Seal(nil, nonce, pt, nil)
			dst := make([]byte, 0, s.n)
			b.SetBytes(int64(s.n))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				dst, err = gcm.Open(dst[:0], nonce, ct, nil)
				if err != nil {
					b.Fatal(err)
				}
			}
			_ = dst
		})
	}
}

// BenchmarkRecallDecryptK10 — то, что реально стоит на пути чтения: RECALL
// отдаёт K якорей, каждый надо расшифровать. Один прогон = один RECALL.
func BenchmarkRecallDecryptK10(b *testing.B) {
	const K = 10
	for _, s := range []int{128, 512, 2048} {
		b.Run(map[int]string{128: "anchor_128B", 512: "anchor_512B", 2048: "anchor_2KB"}[s], func(b *testing.B) {
			gcm := newGCM(b)
			pt := make([]byte, s)
			rand.Read(pt)
			nonce := make([]byte, gcm.NonceSize())
			rand.Read(nonce)
			cts := make([][]byte, K)
			for i := range cts {
				cts[i] = gcm.Seal(nil, nonce, pt, nil)
			}
			dst := make([]byte, 0, s)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := 0; j < K; j++ {
					var err error
					dst, err = gcm.Open(dst[:0], nonce, cts[j], nil)
					if err != nil {
						b.Fatal(err)
					}
				}
			}
			_ = dst
		})
	}
}

// BenchmarkUnwrapDEK — цена разворота ключа факта из KEK скоупа (envelope).
// Ключ 32 Б: это то, что придётся делать на КАЖДЫЙ факт, если DEK не кэшируется.
func BenchmarkUnwrapDEK(b *testing.B) {
	gcm := newGCM(b)
	dek := make([]byte, 32)
	rand.Read(dek)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	wrapped := gcm.Seal(nil, nonce, dek, nil)
	dst := make([]byte, 0, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = gcm.Open(dst[:0], nonce, wrapped, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

// BenchmarkNewGCMPerFact — цена, если наивно строить cipher.AEAD на каждый
// факт вместо кэша. Отдельный замер, потому что это самая вероятная ошибка
// реализации, и она на два порядка дороже самого шифрования.
func BenchmarkNewGCMPerFact(b *testing.B) {
	key := make([]byte, 32)
	rand.Read(key)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		block, err := aes.NewCipher(key)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := cipher.NewGCM(block); err != nil {
			b.Fatal(err)
		}
	}
}
