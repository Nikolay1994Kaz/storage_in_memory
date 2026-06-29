package vector

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"unsafe"
)

// =============================================================================
// Durability колоночного слоя — сериализация TenantCatalog + segmentAttrs в
// бинарный снапшот (формат v2). Пишется после графа каждого frozen/SQ-сегмента.
//
// Формат блока меты:
//   catalog: present(1B); если 1 → bruteThreshold(i32), nRanges(u32),
//            [tenant(u64) start(i32) end(i32) kind(1B)]×N (по возрастанию Start)
//   attrs:   present(1B); если 1 →
//              nCat(u32): [name(str) nVals(u32) val(str)×V nCodes(u32) codes(u32)×C]
//              nNum(u32): [name(str) nVals(u32) vals(f64)×V]
// str = len(u32) + UTF-8 bytes. codes/vals пишутся bulk (host-endian, как векторы).
// =============================================================================

func writeStr(w io.Writer, s string) error {
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(len(s)))
	if _, err := w.Write(u4[:]); err != nil {
		return err
	}
	if len(s) > 0 {
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
	}
	return nil
}

func readStr(r io.Reader) (string, error) {
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return "", err
	}
	n := binary.LittleEndian.Uint32(u4[:])
	if n == 0 {
		return "", nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func putU32(w io.Writer, v uint32) error {
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], v)
	_, err := w.Write(u4[:])
	return err
}

func getU32(r io.Reader) (uint32, error) {
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(u4[:]), nil
}

// writeSegMeta сериализует тенант-каталог и колоночный слой сегмента.
func writeSegMeta(w io.Writer, cat *TenantCatalog, attrs *segmentAttrs) error {
	// ── catalog ──
	if cat == nil {
		if _, err := w.Write([]byte{0}); err != nil {
			return err
		}
	} else {
		if _, err := w.Write([]byte{1}); err != nil {
			return err
		}
		if err := putU32(w, uint32(cat.bruteThreshold)); err != nil {
			return err
		}
		ranges := cat.All() // отсортированы по Start (детерминизм)
		if err := putU32(w, uint32(len(ranges))); err != nil {
			return err
		}
		var buf [21]byte
		for _, rg := range ranges {
			binary.LittleEndian.PutUint64(buf[0:8], rg.Tenant)
			binary.LittleEndian.PutUint32(buf[8:12], uint32(rg.Start))
			binary.LittleEndian.PutUint32(buf[12:16], uint32(rg.End))
			buf[16] = byte(rg.Kind)
			if _, err := w.Write(buf[:17]); err != nil {
				return err
			}
		}
	}

	// ── attrs ──
	if attrs == nil {
		if _, err := w.Write([]byte{0}); err != nil {
			return err
		}
		return nil
	}
	if _, err := w.Write([]byte{1}); err != nil {
		return err
	}

	// Категориальные колонки (детерминированный порядок имён).
	catNames := make([]string, 0, len(attrs.cat))
	for name := range attrs.cat {
		catNames = append(catNames, name)
	}
	sort.Strings(catNames)
	if err := putU32(w, uint32(len(catNames))); err != nil {
		return err
	}
	for _, name := range catNames {
		if err := writeStr(w, name); err != nil {
			return err
		}
		dict := attrs.dict[name]
		if err := putU32(w, uint32(len(dict.vals))); err != nil {
			return err
		}
		for _, v := range dict.vals {
			if err := writeStr(w, v); err != nil {
				return err
			}
		}
		codes := attrs.cat[name].codes
		if err := putU32(w, uint32(len(codes))); err != nil {
			return err
		}
		if len(codes) > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&codes[0])), len(codes)*4)
			if _, err := w.Write(b); err != nil {
				return err
			}
		}
	}

	// Числовые колонки.
	numNames := make([]string, 0, len(attrs.num))
	for name := range attrs.num {
		numNames = append(numNames, name)
	}
	sort.Strings(numNames)
	if err := putU32(w, uint32(len(numNames))); err != nil {
		return err
	}
	for _, name := range numNames {
		if err := writeStr(w, name); err != nil {
			return err
		}
		vals := attrs.num[name].vals
		if err := putU32(w, uint32(len(vals))); err != nil {
			return err
		}
		if len(vals) > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&vals[0])), len(vals)*8)
			if _, err := w.Write(b); err != nil {
				return err
			}
		}
	}
	return nil
}

// readSegMeta десериализует тенант-каталог и колоночный слой сегмента (формат v2).
func readSegMeta(r io.Reader) (*TenantCatalog, *segmentAttrs, error) {
	var flag [1]byte

	// ── catalog ──
	if _, err := io.ReadFull(r, flag[:]); err != nil {
		return nil, nil, err
	}
	var cat *TenantCatalog
	if flag[0] == 1 {
		bt, err := getU32(r)
		if err != nil {
			return nil, nil, err
		}
		nr, err := getU32(r)
		if err != nil {
			return nil, nil, err
		}
		cat = &TenantCatalog{ranges: make(map[uint64]TenantRange, nr), bruteThreshold: int(bt)}
		var buf [17]byte
		for i := uint32(0); i < nr; i++ {
			if _, err := io.ReadFull(r, buf[:17]); err != nil {
				return nil, nil, err
			}
			tenant := binary.LittleEndian.Uint64(buf[0:8])
			cat.ranges[tenant] = TenantRange{
				Tenant: tenant,
				Start:  int(int32(binary.LittleEndian.Uint32(buf[8:12]))),
				End:    int(int32(binary.LittleEndian.Uint32(buf[12:16]))),
				Kind:   IndexKind(buf[16]),
			}
		}
	}

	// ── attrs ──
	if _, err := io.ReadFull(r, flag[:]); err != nil {
		return nil, nil, err
	}
	if flag[0] == 0 {
		return cat, nil, nil
	}
	sa := &segmentAttrs{
		cat:  make(map[string]*categoricalColumn),
		num:  make(map[string]*numericColumn),
		dict: make(map[string]*attrDict),
	}

	nCat, err := getU32(r)
	if err != nil {
		return nil, nil, err
	}
	for i := uint32(0); i < nCat; i++ {
		name, err := readStr(r)
		if err != nil {
			return nil, nil, err
		}
		nVals, err := getU32(r)
		if err != nil {
			return nil, nil, err
		}
		dict := &attrDict{code: make(map[string]uint32, nVals), vals: make([]string, nVals)}
		for c := uint32(0); c < nVals; c++ {
			v, err := readStr(r)
			if err != nil {
				return nil, nil, err
			}
			dict.vals[c] = v
			dict.code[v] = c
		}
		nCodes, err := getU32(r)
		if err != nil {
			return nil, nil, err
		}
		codes := make([]uint32, nCodes)
		if nCodes > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&codes[0])), int(nCodes)*4)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, nil, err
			}
		}
		sa.cat[name] = &categoricalColumn{codes: codes}
		sa.dict[name] = dict
	}

	nNum, err := getU32(r)
	if err != nil {
		return nil, nil, err
	}
	for i := uint32(0); i < nNum; i++ {
		name, err := readStr(r)
		if err != nil {
			return nil, nil, err
		}
		nVals, err := getU32(r)
		if err != nil {
			return nil, nil, err
		}
		vals := make([]float64, nVals)
		if nVals > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&vals[0])), int(nVals)*8)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, nil, err
			}
		}
		sa.num[name] = &numericColumn{vals: vals}
	}

	if err := validateSegMeta(cat, sa); err != nil {
		return nil, nil, err
	}
	return cat, sa, nil
}

// validateSegMeta — лёгкая проверка целостности (длины колонок согласованы).
func validateSegMeta(cat *TenantCatalog, sa *segmentAttrs) error {
	n := -1
	for name, col := range sa.cat {
		if n < 0 {
			n = len(col.codes)
		} else if len(col.codes) != n {
			return fmt.Errorf("attr durability: рассинхрон длины колонки %q (%d != %d)", name, len(col.codes), n)
		}
	}
	for name, col := range sa.num {
		if n < 0 {
			n = len(col.vals)
		} else if len(col.vals) != n {
			return fmt.Errorf("attr durability: рассинхрон длины числовой колонки %q (%d != %d)", name, len(col.vals), n)
		}
	}
	return nil
}
