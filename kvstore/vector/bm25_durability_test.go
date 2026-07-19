package vector

// Тесты шага 5 BM25-спринта: текст переживает рестарт. WAL-блоб (термы, не
// текст) круговым путём и через реплей-choke-point AddDocTerms; снапшот v7 —
// все три типа сегментов; decodeTerms — обратный проход buildSegmentText.

import (
	"bytes"
	"reflect"
	"testing"
)

// TestBM25WALRoundTrip — Serialize/DeserializeVectorWithDoc бит-в-бит, затем
// «реплей»: доки заходят в свежий стор через AddDocTerms (путь OpVSimAddDoc,
// без перетокенизации) — golden обязан сойтись как при живом AddDoc.
func TestBM25WALRoundTrip(t *testing.T) {
	corpus, golden := loadBM25Golden(t)

	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Clear()

	for i, d := range corpus.Docs {
		vec := mkVecN(8, float32(i))
		attrs := Attributes{Cat: map[string]string{"lang": "ru"}}
		terms := bm25CountTerms(bm25Tokenize(d.Text))

		blob := SerializeVectorWithDoc(vec, attrs, terms)
		gotVec, gotAttrs, gotTerms, err := DeserializeVectorWithDoc(blob)
		if err != nil {
			t.Fatalf("%s: decode: %v", d.ID, err)
		}
		if !reflect.DeepEqual(gotVec, vec) || !reflect.DeepEqual(gotTerms, terms) ||
			!reflect.DeepEqual(gotAttrs.Cat, attrs.Cat) {
			t.Fatalf("%s: round-trip разошёлся: vec=%v terms=%v attrs=%v", d.ID, gotVec, gotTerms, gotAttrs)
		}
		// Реплей: термы из «журнала», не из повторной токенизации.
		if err := lvs.AddDocTerms(d.ID, gotVec, gotAttrs, gotTerms); err != nil {
			t.Fatalf("%s: AddDocTerms: %v", d.ID, err)
		}
	}
	bm25AssertGolden(t, lvs, corpus, golden)

	// Пустые терм-списки и пустые attrs тоже круговые (docs без текста).
	blob := SerializeVectorWithDoc(mkVecN(8, 1), Attributes{}, nil)
	_, _, terms, err := DeserializeVectorWithDoc(blob)
	if err != nil || terms != nil {
		t.Fatalf("пустые термы: terms=%v err=%v, ждём nil/nil", terms, err)
	}
}

// TestBM25SnapshotRoundTrip — текст переживает SaveBinary→LoadBinary во всех
// трёх типах сегментов: frozen CSR (dim≤256), frozenSQ8 (UseSQ), hnswFlat
// (dim>256, слой регенерируется buildSegment из entries[].Terms).
func TestBM25SnapshotRoundTrip(t *testing.T) {
	corpus, golden := loadBM25Golden(t)

	cases := []struct {
		name string
		dim  int
		cfg  func() LeveledConfig
	}{
		{"frozen_csr_dim8", 8, bm25TestConfig},
		{"frozen_sq8_dim8", 8, func() LeveledConfig {
			cfg := bm25TestConfig()
			cfg.UseSQ = true
			return cfg
		}},
		{"hnsw_flat_dim300", 300, bm25TestConfig},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := NewLeveledVectorStore(tc.cfg())
			defer src.Clear()
			bm25AddCorpus(t, src, corpus, 0, len(corpus.Docs), tc.dim)
			src.FlushDeltaSync()
			bm25AssertGolden(t, src, corpus, golden) // санити до снапшота

			var buf bytes.Buffer
			if err := src.SaveBinary(&buf); err != nil {
				t.Fatalf("SaveBinary: %v", err)
			}

			dst := NewLeveledVectorStore(tc.cfg())
			defer dst.Clear()
			if err := dst.LoadBinary(&buf); err != nil {
				t.Fatalf("LoadBinary: %v", err)
			}
			bm25AssertGolden(t, dst, corpus, golden) // «рестарт» пережит
		})
	}
}

// TestBM25SnapshotNoText — стор без текстов (чистые вектора) пишет present=0
// блоки и грузится с nil-слоем: zero overhead и нет ложных текстовых выдач.
func TestBM25SnapshotNoText(t *testing.T) {
	src := NewLeveledVectorStore(bm25TestConfig())
	defer src.Clear()
	for i := 0; i < 5; i++ {
		if err := src.Add(string(rune('a'+i)), mkVecN(8, float32(i))); err != nil {
			t.Fatal(err)
		}
	}
	src.FlushDeltaSync()

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	dst := NewLeveledVectorStore(bm25TestConfig())
	defer dst.Clear()
	if err := dst.LoadBinary(&buf); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}
	if res, err := dst.SearchText("чай", 10); err != nil || res != nil {
		t.Fatalf("текстовый поиск по стору без текстов: res=%v err=%v, ждём nil/nil", res, err)
	}
}

// TestBM25DecodeTerms — decodeTerms ⁻¹ buildSegmentText: пер-доковые термы
// восстанавливаются из постингов в точности (уникальны, отсортированы).
func TestBM25DecodeTerms(t *testing.T) {
	corpus, _ := loadBM25Golden(t)
	entries := make([]DeltaEntry, len(corpus.Docs))
	for i, d := range corpus.Docs {
		entries[i] = DeltaEntry{Key: d.ID, Terms: bm25CountTerms(bm25Tokenize(d.Text))}
	}
	st := buildSegmentText(entries)
	dec := st.decodeTerms()
	if len(dec) != len(entries) {
		t.Fatalf("len(dec)=%d, ждём %d", len(dec), len(entries))
	}
	for i := range entries {
		if !reflect.DeepEqual(dec[i], entries[i].Terms) {
			t.Errorf("док %s: decodeTerms=%v, ждём %v", entries[i].Key, dec[i], entries[i].Terms)
		}
	}
	// nil-слой декодится в nil (hnsw-путь снапшота без текстов).
	var nilST *segmentText
	if got := nilST.decodeTerms(); got != nil {
		t.Errorf("nil segmentText: decodeTerms=%v, ждём nil", got)
	}
}
