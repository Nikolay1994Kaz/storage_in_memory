// Package ship реализует асинхронную доставку durability-набора (WAL-сегменты +
// снапшоты) на удалённое хранилище — continuous WAL-shipping в стиле Litestream.
//
// Архитектурный выбор: шиппер работает ПОВЕРХ каталога данных, без хуков в
// движок. Это возможно благодаря двум инвариантам файлового layout'а:
//
//   - wal_*.log — строго append-only (ротация создаёт НОВОЕ имя, старое имя
//     никогда не переписывается) → дошипливаем хвост от последнего offset'а;
//   - snapshot.wal / graph_leveled.bin — иммутабельны после атомарного rename
//     (tmp+rename+fsync dir) → грузим целиком под версионированным ключом.
//
// Consistency-модель restore-точки — та же, что у живого бэкапа (docs/BACKUP.md):
// crash-consistent набор. Частичная хвостовая запись WAL отбрасывается на
// replay по CRC. Манифест ссылается только на полностью загруженные объекты,
// поэтому каждый опубликованный манифест — валидная точка восстановления.
package ship

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Remote — минимальный контракт объектного хранилища. Ключи — относительные
// пути в POSIX-стиле ("wal/<file>/<offset>.chunk"). Реализации: файловая
// директория (file://) и S3-совместимый бакет (s3://).
type Remote interface {
	// Put полностью записывает объект. Перезапись существующего ключа допустима
	// (используется только для MANIFEST.json; данные-объекты иммутабельны).
	Put(ctx context.Context, key string, data []byte) error
	// Get возвращает содержимое объекта. Отсутствующий ключ → ErrNotExist.
	Get(ctx context.Context, key string) ([]byte, error)
	// List возвращает все ключи с данным префиксом (рекурсивно).
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete удаляет объект. Отсутствующий ключ — не ошибка (идемпотентность
	// ретеншна: повторный проход после частичного сбоя не должен падать).
	Delete(ctx context.Context, key string) error
}

// ErrNotExist сигнализирует об отсутствии объекта (аналог os.ErrNotExist).
var ErrNotExist = fmt.Errorf("ship: object does not exist")

// OpenRemote разбирает URL хранилища и возвращает бэкенд.
//
//	file:///abs/path            — директория (NFS/rclone-mount/локальные тесты)
//	s3://bucket/prefix?endpoint=https://minio:9000&region=us-east-1
//	                            — S3-совместимый бакет; креды из env
//	                              (KVSTORE_S3_ACCESS_KEY/SECRET_KEY или
//	                              AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)
func OpenRemote(rawURL string) (Remote, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ship: bad url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "file":
		// file:///abs/path → Host пуст, Path = /abs/path.
		if u.Host != "" || u.Path == "" {
			return nil, fmt.Errorf("ship: file URL must be file:///abs/path, got %q", rawURL)
		}
		return newFileRemote(u.Path)
	case "s3":
		return newS3Remote(u)
	default:
		return nil, fmt.Errorf("ship: unsupported scheme %q (want file:// or s3://)", u.Scheme)
	}
}

// joinKey склеивает сегменты ключа, отбрасывая пустые.
func joinKey(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('/')
		}
		b.WriteString(p)
	}
	return b.String()
}

// readAll — io.ReadAll с оборачиванием ошибки ключом (диагностика оператору).
func readAll(r io.Reader, key string) ([]byte, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ship: read %s: %w", key, err)
	}
	return b, nil
}
