package zset

import (
	"fmt"
	"testing"

	"kvstore/kvstore/internal/btree"
)

// ── Микро-бенчмарки для разбора узких мест ──────────────────

// BenchmarkZAddBreakdown — где теряется время в ZADD
func BenchmarkZAddBreakdown(b *testing.B) {
	reg, store := newTestRegistry()

	b.Run("1_BPTree_Insert_only", func(b *testing.B) {
		tree := reg.getOrCreate("bench_insert").tree
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tree.Insert(float64(i), fmt.Sprintf("item:%d", i))
		}
	})

	b.Run("2_KV_Set_only", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("__zidx:bench:%d", i)
			store.Set(0, key, encodeScore(float64(i)))
		}
	})

	b.Run("3_reverseKey_alloc", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = reverseKey("benchset", fmt.Sprintf("item:%d", i))
		}
	})

	b.Run("4_fmt_Sprintf_only", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = fmt.Sprintf("item:%d", i)
		}
	})

	b.Run("5_ZAdd_full", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			reg.ZAdd(0, "bench_full", float64(i), fmt.Sprintf("item:%d", i))
		}
	})
}

// BenchmarkRangeBreakdown — где теряется время в RangeSearch/ZRANGEBYSCORE
func BenchmarkRangeBreakdown(b *testing.B) {
	reg, _ := newTestRegistry()
	for i := 0; i < 100000; i++ {
		reg.ZAdd(0, "bench", float64(i), fmt.Sprintf("item:%d", i))
	}
	tree := reg.getOrCreate("bench").tree

	b.Run("1_BPTree_RangeSearch_raw", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			start := float64(i % 99000)
			tree.RangeSearch(start, start+100)
		}
	})

	b.Run("2_ZRangeByScore_with_copy", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			start := float64(i % 99000)
			reg.ZRangeByScore("bench", start, start+100)
		}
	})

	b.Run("3_MembersInRange_with_map", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			start := float64(i % 99000)
			reg.MembersInRange("bench", start, start+100)
		}
	})

	b.Run("4_syncMap_Load_only", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			reg.get("bench")
		}
	})

	b.Run("5_RangeCollectHashes_optimized", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			start := float64(i % 99000)
			tree.RangeCollectHashes(start, start+100)
		}
	})

	b.Run("6_MembersInRangeHashed", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			start := float64(i % 99000)
			reg.MembersInRangeHashed("bench", start, start+100)
		}
	})

	b.Run("7_HashMember_inline", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			btree.HashMember("product:12345")
		}
	})
}
