package vector

import (
	"strconv"
	"testing"
	"time"
)

// TestFlushDeltaSyncDoesNotFlushLaterWrites — запись, сделанная ПОСЛЕ
// FlushDeltaSync, остаётся в дельте.
//
// Регресс: завершив сброс, FlushDeltaSync толкал каскад слияний сигналом
// координатору (triggerCompact). Координатор по этому сигналу сначала
// забирает текущую дельту, а в ней уже лежат записи, сделанные после
// FlushDeltaSync, — они уезжали лишним мини-сегментом. Upsert ключа сразу
// после сброса оказывался во втором сегменте, и старая копия всплывала в
// текстовом поиске там, где сегменты гасят друг друга только дедупом хитов.
func TestFlushDeltaSyncDoesNotFlushLaterWrites(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	for round := 0; round < 10; round++ {
		for i := 0; i < 50; i++ {
			key := "k" + strconv.Itoa(round) + "-" + strconv.Itoa(i)
			if err := lvs.Add(key, compatVec(i)); err != nil {
				t.Fatal(err)
			}
		}
		lvs.FlushDeltaSync()
		if err := lvs.Add("hot", compatVec(round)); err != nil {
			t.Fatal(err)
		}
		// Даём координатору время отработать сигнал, если он был.
		time.Sleep(20 * time.Millisecond)
		if n := lvs.Stats().DeltaLen; n != 1 {
			t.Fatalf("раунд %d: в дельте %d записей, ожидалась 1 — запись после FlushDeltaSync уехала в отдельный сегмент", round, n)
		}
	}
}
