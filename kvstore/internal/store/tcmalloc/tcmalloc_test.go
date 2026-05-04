package tcmalloc

import (
	"fmt"
	"testing"
)

func TestHandle_PackUnpack(t *testing.T) {
	tests := []struct {
		spanID   uint32
		objIndex int
	}{
		{0, 0},
		{1, 5},
		{1000, 63},
		{0xFFFFFFFF, 0},
		{0, 0x7FFFFFFF},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("span%d_obj%d", tt.spanID, tt.objIndex)
		t.Run(name, func(t *testing.T) {
			h := MakeHandle(tt.spanID, tt.objIndex)
			if h.SpanID() != tt.spanID {
				t.Fatalf("SpanID: got %d, want %d", h.SpanID(), tt.spanID)
			}
			if h.ObjIndex() != tt.objIndex {
				t.Fatalf("ObjIndex: got %d, want %d", h.ObjIndex(), tt.objIndex)
			}
		})
	}
}

func TestHandle_Uniqueness(t *testing.T) {
	h1 := MakeHandle(1, 0)
	h2 := MakeHandle(0, 1)
	h3 := MakeHandle(1, 1)
	if h1 == h2 {
		t.Fatal("different spanID/objIndex produced same Handle")
	}
	if h1 == h3 || h2 == h3 {
		t.Fatal("different Handles should not be equal")
	}
}

func TestSizeClassForSize(t *testing.T) {
	tests := []struct {
		size      int
		wantClass int
	}{
		{1, 0},  // 1B → class 0 (32B)
		{32, 0}, // ровно 32B → class 0
		{33, 1}, // 33B → class 1 (64B), не влезает в 32
		{64, 1}, // ровно 64
		{65, 2}, // → class 2 (128B)
		{128, 2},
		{4096, 7},   // максимальный span-класс
		{4097, -1},  // > 4096 → large object
		{10000, -1}, // large
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("size%d", tt.size), func(t *testing.T) {
			got := SizeClassForSize(tt.size)
			if got != tt.wantClass {
				t.Fatalf("SizeClassForSize(%d) = %d, want %d", tt.size, got, tt.wantClass)
			}
		})
	}
}

func TestSizeClass_FitsData(t *testing.T) {
	for size := 1; size <= 4096; size++ {
		sc := SizeClassForSize(size)
		if sc < 0 {
			t.Fatalf("size %d shoud have a class, got -1", size)
		}
		if sizeClasses[sc] < size {
			t.Fatalf("size %d -> class %d (capacity %d): doesn t fit!", size, sc, sizeClasses[sc])
		}
	}
}

func TestSpan_AllocBumpPointer(t *testing.T) {
	data := make([]byte, 128)
	s := NewSpan(data, 32, 0, nil)

	for i := 0; i < 4; i++ {
		buf, idx := s.Alloc()
		if buf == nil {
			t.Fatalf("Alloc #%d returned nil", i)
		}
		if idx != i {
			t.Fatalf("Alloc #%d: idx=%d, want %d", i, idx, i)
		}
		if len(buf) != 32 {
			t.Fatalf("Alloc #%d: len=%d, want 32", i, len(buf))
		}
	}
	buf, idx := s.Alloc()
	if buf != nil {
		t.Fatalf("Alloc on full span shoud return nil")
	}
	if idx != -1 {
		t.Fatalf("full span idx = %d, want -1", idx)
	}
}
