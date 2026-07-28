// ЗАМЕР ЦЕНЫ ДО КОДА для П5а («снапшот под конвертом»), та же дисциплина, что
// в internal/keyring/cost_bench_test.go: выбор пути делается по числу.
//
// РАЗВИЛКА. Конверт (EV1) уже стоит на границе персистентности и покрывает
// WAL, а значит и отгруженный архив. Он НЕ покрывает graph_leveled.bin: тот
// пишется колоночно (WriteGraphTo + writeSegMeta + writeSegText), сегменты
// нарезаны по времени вставки, поэтому в одном сегменте лежат РАЗНЫЕ скоупы —
// один конверт на сегмент невозможен, а ключ у нас на скоуп. Два пути:
//
//	(B) VMEM-факты не кладутся в graph_leveled.bin вовсе, а едут в snapshot.wal
//	    записями OpVSimAddDoc — там конверт и штатный пропуск при
//	    ErrKeyDestroyed УЖЕ работают (main.go:applyEntry). Цена: старт
//	    восстанавливает их реплеем, то есть строит HNSW заново.
//	(G) Резать формат graph_leveled.bin на группы по скоупу и запечатывать
//	    каждую. Старт остаётся быстрым, но это правка самого нежного места
//	    durability плюс смена версии формата.
//
// ПОРОГ, НАЗНАЧЕННЫЙ ЗАРАНЕЕ. Типовой объём памяти агента — 10k фактов
// (личная память на :6381 — единицы, демо-корпуса — тысячи). Путь (B)
// принимается, если на 10k фактах реплей добавляет к старту ≤ 2 с: столько
// не меняет операционную практику, сервер и так читает снапшот и хвост WAL.
// Если дороже — цена оплачивается ОДИН раз в реализации (G), а не при каждом
// старте у каждого пользователя.
//
// ⭐ИЗМЕРЕНО 28.07.2026 (i7-9750H), порог НЕ ПРОЙДЕН, путь (B) отвергнут:
//
//	facts   dim   LoadBinary    replay   replay/load   snapshot
//	 1000    64          6ms     173ms         27.0×      0.7МБ
//	10000    64      33–76ms    4.9s        64–150×      6.7МБ
//	 1000   768        390ms     379ms          1.0×      3.5МБ
//	10000   768      51.661s   47.239s          0.9×     34.9МБ
//
// Вилка в строке 10k×64 — разброс двух прогонов (33 мс и 76 мс на загрузке,
// кэш файловой системы); реплей воспроизводится стабильно ~4.9 с. Решение
// стоит на порядке величины, а не на конкретном числе, поэтому разброс на
// вывод не влияет — и записан, чтобы никто не считал одно из чисел каноном.
//
// Читать так: при dim ≤ csrDimThreshold (256) — а это ВЕСЬ типичный VMEM,
// placeholder-вектор ступени 0 имеет dim=32 — бинарный снапшот быстрее реплея
// в 64×, и платить 4.9 с при каждом старте нельзя. Строка dim=768 к выбору
// отношения не имеет и означает другое: для fp32 dim>256 сегмент пишется как
// segTypeHNSWFlat, и его ЗАГРУЗКА сама перестраивает HNSW (leveled_store.go,
// segTypeHNSWFlat в LoadBinary) — потому и 51 с. Это отдельная известная цена
// формата, не следствие шифрования.
//
// Замер щадил путь (B): боевой deltaMax при dim≤64 равен 50 000, то есть в бою
// 10k фактов так же осели бы в ОДНОЙ дельте с построением графа.
//
// Кейс 10000×768 в прогоне не оставлен (100 с на прогон); его числа выше
// измерены однажды и к решению не относятся.
//
// Что тест охраняет ТЕПЕРЬ. Не порог (решение принято), а его ПРЕДПОСЫЛКУ:
// если реплей когда-нибудь станет сопоставим с бинарной загрузкой, основание
// выбора исчезнет, и П5а надо пересматривать. Тест скажет об этом сам.
//
// Меряется ровно то, что делает восстановление: LoadBinary против N вызовов
// AddDocTerms (main.go:517, путь реплея OpVSimAddDoc).
package vector

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"testing"
	"time"
)

func replayCostConfig() LeveledConfig {
	// Конфигурация боевая, а не тестовая: цена реплея = цена построения HNSW,
	// и на заниженном efConstruction замер соврал бы в нашу пользу.
	return LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       10000,
		Fanout:         8,
		EfSearch:       100,
		M:              32,
		EfConstruction: 400,
		NumBuilders:    2,
	}
}

// replayCostFact — детерминированный факт масштаба реальной записи VMEM:
// вектор, атрибуты (scope/source/тип + числовые оси времени) и ~30 термов.
func replayCostFact(i, dim int) (key string, vec []float32, attrs Attributes, terms []TermTF) {
	key = fmt.Sprintf("01J0000000000000000000%04d", i)

	h := fnv.New64a()
	fmt.Fprintf(h, "vec:%d", i)
	state := h.Sum64()
	vec = make([]float32, dim)
	for j := range vec {
		state += 0x9E3779B97F4A7C15
		z := state
		z ^= z >> 30
		z *= 0xBF58476D1CE4E5B9
		vec[j] = float32(int32(z>>32)) / float32(1<<31)
	}

	attrs = Attributes{
		Cat: map[string]string{
			vmemAttrScope:  fmt.Sprintf("scope-%d", i%8), // 8 скоупов, как у живого стора
			vmemAttrSource: fmt.Sprintf("source-%d", i%4),
			"type":         "fact",
		},
		Num: map[string]float64{
			"valid_from": float64(1750000000 + i),
			"importance": 0.5,
		},
	}

	terms = make([]TermTF, 0, 30)
	for t := 0; t < 30; t++ {
		terms = append(terms, TermTF{Term: fmt.Sprintf("term%05d", (i*7+t*13)%5000), TF: 1})
	}
	return key, vec, attrs, terms
}

// replayCostBuild наполняет стор N фактами тем же вызовом, которым идёт реплей.
func replayCostBuild(tb testing.TB, n, dim int) *LeveledVectorStore {
	tb.Helper()
	lvs := NewLeveledVectorStore(replayCostConfig())
	for i := 0; i < n; i++ {
		key, vec, attrs, terms := replayCostFact(i, dim)
		if err := lvs.AddDocTerms(key, vec, attrs, terms); err != nil {
			tb.Fatalf("AddDocTerms(%d): %v", i, err)
		}
	}
	return lvs
}

// TestSnapshotReplayCost печатает таблицу и ПРОВАЛИВАЕТСЯ, если путь (B) не
// укладывается в назначенный заранее порог. Провал здесь — не баг, а ответ
// «дешёвый путь не подходит, делать (G)»; поэтому сообщение говорит это прямо.
func TestSnapshotReplayCost(t *testing.T) {
	if testing.Short() {
		t.Skip("замер цены восстановления: долгий, гоняется без -short")
	}

	// minRatioAt10k — во сколько раз реплей обязан оставаться дороже бинарной
	// загрузки, чтобы решение «путь G» сохраняло основание. Измерено 64×;
	// сторожим с большим запасом, потому что стеречь надо смену ПОРЯДКА
	// величины, а не колебания железа.
	const minRatioAt10k = 5.0

	cases := []struct {
		n   int
		dim int
	}{
		{1000, 64},  // BoW-демо
		{10000, 64}, // основание решения проверяется здесь
		{1000, 768}, // реальные эмбеддинги (dim > csrDimThreshold)
	}

	t.Logf("%8s %6s %14s %14s %10s %12s", "facts", "dim", "LoadBinary", "replay", "replay/load", "snapshot")
	for _, c := range cases {
		src := replayCostBuild(t, c.n, c.dim)
		src.FlushDeltaSync()

		var buf bytes.Buffer
		if err := src.SaveBinary(&buf); err != nil {
			t.Fatalf("SaveBinary: %v", err)
		}
		snapSize := buf.Len()
		src.Clear()

		// (G): загрузка колоночного снапшота как есть.
		dst := NewLeveledVectorStore(replayCostConfig())
		startLoad := time.Now()
		if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
			t.Fatalf("LoadBinary: %v", err)
		}
		loadDur := time.Since(startLoad)
		dst.Clear()

		// (B): восстановление реплеем — ровно то, что делает applyEntry для
		// OpVSimAddDoc, факт за фактом.
		startReplay := time.Now()
		rep := replayCostBuild(t, c.n, c.dim)
		replayDur := time.Since(startReplay)
		rep.Clear()

		t.Logf("%8d %6d %14s %14s %10.1f× %11.1fМБ",
			c.n, c.dim, loadDur.Round(time.Millisecond), replayDur.Round(time.Millisecond),
			float64(replayDur)/float64(loadDur), float64(snapSize)/(1024*1024))

		if c.n == 10000 && float64(replayDur)/float64(loadDur) < minRatioAt10k {
			t.Errorf("предпосылка выбора пути исчезла: реплей 10k фактов (dim=%d) = %s "+
				"против LoadBinary %s, отношение %.1f× < %.1f×. Путь G (запечатывать сам "+
				"формат снапшота) выбран ИМЕННО потому, что реплей на старте на порядки "+
				"дороже; если это больше не так — П5а надо пересмотреть в пользу дешёвого "+
				"пути, а не оставлять сложность по инерции",
				c.dim, replayDur.Round(time.Millisecond), loadDur.Round(time.Millisecond),
				float64(replayDur)/float64(loadDur), minRatioAt10k)
		}
	}
}
