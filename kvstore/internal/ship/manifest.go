package ship

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Раскладка объектов под префиксом:
//
//	MANIFEST.json                — копия последнего манифеста (точка входа restore)
//	manifests/<gen 16-hex>.json  — история манифестов (ретеншн держит последние N)
//	snap/<fp>/snapshot.wal       — KV-снапшот, иммутабелен (fp = fingerprint файла)
//	snap/<fp>/graph_leveled.bin  — векторный снапшот, иммутабелен
//	wal/<имя файла>/<offset 16-hex>.chunk — байтовые куски append-only WAL
//
// Все данные-объекты иммутабельны (ключ включает fingerprint/offset), поэтому
// одновременное чтение старого манифеста и загрузка новых объектов не
// конфликтуют. Перезаписывается только MANIFEST.json — атомарной заменой
// (file://: tmp+rename; S3: PUT целиком).
const (
	manifestLatestKey = "MANIFEST.json"
	manifestDirKey    = "manifests/"
	snapDirKey        = "snap/"
	walDirKey         = "wal/"
)

// Chunk — непрерывный кусок WAL-файла [Offset, Offset+Size).
type Chunk struct {
	Key    string `json:"key"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

// WALFile — состояние доставки одного wal_*.log: сколько байт загружено и
// какими кусками. Куски строго смежные: offset следующего = offset+size
// предыдущего (валидируется на restore).
type WALFile struct {
	Name   string  `json:"name"`
	Size   int64   `json:"size"` // сумма Size всех Chunks = загруженный префикс файла
	Chunks []Chunk `json:"chunks"`
}

// FileRef — ссылка на иммутабельный снапшот-объект.
// Fingerprint = "size-mtimeNano" локального файла на момент загрузки: по нему
// шиппер после рестарта понимает, что снапшот уже загружен (идемпотентность).
type FileRef struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	Fingerprint string `json:"fingerprint"`
}

// Manifest — одна консистентная restore-точка: снапшоты + список WAL-файлов.
// Инвариант публикации (держит shipper): все WAL-файлы, кроме последнего
// (активного), загружены полностью. Иначе частичный старый WAL рядом со свежим
// снапшотом воскрешал бы на restore ключи, удалённые в его недошипленном хвосте.
type Manifest struct {
	Gen             uint64    `json:"gen"`
	CreatedUnixNano int64     `json:"created_unix_nano"`
	Snapshot        *FileRef  `json:"snapshot,omitempty"` // snapshot.wal
	Graph           *FileRef  `json:"graph,omitempty"`    // graph_leveled.bin
	WALs            []WALFile `json:"wals"`
}

func manifestKey(gen uint64) string {
	return fmt.Sprintf("%s%016x.json", manifestDirKey, gen)
}

// parseManifestGen извлекает генерацию из ключа manifests/<hex>.json.
func parseManifestGen(key string) (uint64, bool) {
	name := strings.TrimPrefix(key, manifestDirKey)
	name = strings.TrimSuffix(name, ".json")
	if len(name) != 16 {
		return 0, false
	}
	gen, err := strconv.ParseUint(name, 16, 64)
	if err != nil {
		return 0, false
	}
	return gen, true
}

// publishManifest пишет манифест под номером генерации и обновляет копию
// MANIFEST.json. Порядок важен: сперва иммутабельная история, потом «указатель».
func publishManifest(ctx context.Context, r Remote, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("ship: marshal manifest: %w", err)
	}
	if err := r.Put(ctx, manifestKey(m.Gen), data); err != nil {
		return err
	}
	return r.Put(ctx, manifestLatestKey, data)
}

// loadLatestManifest читает актуальный манифест. Сначала MANIFEST.json; если
// его нет/битый — самый старший из manifests/ (устойчивость к падению между
// двумя Put'ами publishManifest). Возвращает nil без ошибки, если хранилище пусто.
func loadLatestManifest(ctx context.Context, r Remote) (*Manifest, error) {
	if data, err := r.Get(ctx, manifestLatestKey); err == nil {
		var m Manifest
		if jerr := json.Unmarshal(data, &m); jerr == nil {
			return &m, nil
		}
		// Битый MANIFEST.json — падаем на историю, не на ошибку.
	} else if !errors.Is(err, ErrNotExist) {
		return nil, err
	}

	keys, err := r.List(ctx, manifestDirKey)
	if err != nil {
		return nil, err
	}
	var best string
	var bestGen uint64
	for _, k := range keys {
		if gen, ok := parseManifestGen(k); ok && (best == "" || gen > bestGen) {
			best, bestGen = k, gen
		}
	}
	if best == "" {
		return nil, nil // пустое хранилище — первый запуск
	}
	data, err := r.Get(ctx, best)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("ship: manifest %s corrupt: %w", best, err)
	}
	return &m, nil
}

// liveObjects собирает множество ключей, на которые ссылается манифест.
func (m *Manifest) liveObjects() map[string]struct{} {
	live := map[string]struct{}{manifestKey(m.Gen): {}}
	if m.Snapshot != nil {
		live[m.Snapshot.Key] = struct{}{}
	}
	if m.Graph != nil {
		live[m.Graph.Key] = struct{}{}
	}
	for _, w := range m.WALs {
		for _, c := range w.Chunks {
			live[c.Key] = struct{}{}
		}
	}
	return live
}

// sortWALs держит файлы в порядке replay (лексикографический = хронологический,
// имена содержат timestamp — так же сортирует ReadAllWALs/recovery).
func sortWALs(w []WALFile) {
	sort.Slice(w, func(i, j int) bool { return w[i].Name < w[j].Name })
}
