package main

// =============================================================================
// Восстановление на момент (примитив 3 слоя восстановления, 26.07).
//
// Вопрос, на который отвечает режим: «покажи состояние памяти таким, каким оно
// было в момент X» — форензический бэкстоп поверх уже существующей машинерии
// (снапшот + реплей журнала). Нового механизма нет: добавлено условие
// остановки реплея и запрет записи.
//
// ЧЕСТНАЯ ГРАНИЦА, которую нельзя замазывать. Точка, раньше которой каталог
// данных восстановить НЕЛЬЗЯ, определяется снапшотами: они хранят состояние,
// а не историю, и перезаписываются на месте. Поэтому:
//   - если бинарного снапшота векторов ещё нет, журнал покрывает всю историю
//     и целью может быть любой LSN;
//   - если снапшот есть, восстановить можно только НЕ РАНЬШЕ его watermark —
//     всё, что было до, физически перестало существовать как история.
// В этом случае режим отказывается стартовать и называет самый ранний
// достижимый LSN. Восстановление, которое молча отдаёт не то состояние, о
// котором спросили, хуже отсутствия восстановления: на него сошлются.
// Открытая дверь (не сделано): удержание базовых снапшотов расширило бы окно.
// =============================================================================

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"kvstore/kvstore/internal/wal"
)

// walOpName — человекочитаемое имя операции журнала для -wal-inspect.
func walOpName(op byte) string {
	switch op {
	case wal.OpSet:
		return "SET"
	case wal.OpDel:
		return "DEL"
	case wal.OpExpire:
		return "EXPIRE"
	case wal.OpVSimAdd:
		return "VSIM.ADD"
	case wal.OpVSimAddAttrs:
		return "VSIM.ADDATTR"
	case wal.OpVSimAddDoc:
		return "VSIM.ADDDOC"
	case wal.OpVSimAddDocBatch:
		return "VSIM.ADDDOC.BATCH"
	case wal.OpVSimDel:
		return "VSIM.DEL"
	default:
		return fmt.Sprintf("OP(0x%02x)", op)
	}
}

// inspectWAL печатает журнал каталога данных: LSN, операция, ключ, размер.
// Существует ради -restore-to-lsn: назвать момент можно, только увидев его
// номер. Читает файлы и ничего не меняет.
func inspectWAL(dir string, out io.Writer) error {
	snapPath := filepath.Join(dir, "snapshot.wal")
	if snapWatermark, entries, err := wal.ReadFile(snapPath); err == nil && (snapWatermark > 0 || len(entries) > 0) {
		fmt.Fprintf(out, "# snapshot.wal — state as of LSN %d (%d entries; the log below starts after it)\n", snapWatermark, len(entries))
	}
	matches, err := filepath.Glob(filepath.Join(dir, "wal_*.log"))
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		fmt.Fprintf(out, "# no wal_*.log in %s\n", dir)
		return nil
	}
	sort.Strings(matches)
	fmt.Fprintf(out, "%-12s %-18s %s\n", "LSN", "OP", "KEY")
	for _, path := range matches {
		_, entries, err := wal.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		for _, e := range entries {
			fmt.Fprintf(out, "%-12d %-18s %s (%d bytes)\n", e.LSN, walOpName(e.Op), e.Key, len(e.Value))
		}
	}
	return nil
}

// restoreOutDir — каталог, куда восстановленный узел пишет всё своё (новый
// журнал, снапшоты синкера). В обычном режиме это сам каталог данных; в режиме
// восстановления — временный, чтобы оригинал остался нетронутым побайтово.
// Возвращает также функцию уборки.
func restoreOutDir(dataDir string) (string, func(), error) {
	if restoreLSN == 0 {
		return dataDir, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "kvstore-restore-")
	if err != nil {
		return "", nil, err
	}
	return tmp, func() { os.RemoveAll(tmp) }, nil
}

// checkRestoreReachable проверяет, что цель восстановления достижима честно:
// снапшоты покрывают состояние ДО своих watermark, поэтому цель раньше любого
// из них недостижима в принципе. Возвращает ошибку с самым ранним достижимым
// LSN — оператору нужна не «нельзя», а «можно начиная с».
func checkRestoreReachable(vecWatermark, snapWatermark uint64) error {
	if restoreLSN == 0 {
		return nil
	}
	earliest := max(vecWatermark, snapWatermark)
	if restoreLSN >= earliest {
		return nil
	}
	return fmt.Errorf(
		"cannot restore to LSN %d: snapshots already hold state past that point "+
			"(vector snapshot watermark %d, kv snapshot watermark %d). "+
			"The earliest point this data directory can reproduce is LSN %d",
		restoreLSN, vecWatermark, snapWatermark, earliest)
}
