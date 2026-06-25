#include "textflag.h"

// =============================================================================
// SQ8 AVX2 distance kernels (ADC — Asymmetric Distance Computation)
//
// Stack frame: 100 байт аргументов (4 slice-headers × 24 + 4 float32 ret):
//   query:    base+0(FP),  len+8(FP),  cap+16(FP)  (go slice = 24 bytes)
//   codes:    base+24(FP), len+32(FP), cap+40(FP)
//   sqMin:    base+48(FP), len+56(FP), cap+64(FP)
//   sqScale:  base+72(FP), len+80(FP), cap+88(FP)
//   ret:      +96(FP)  (float32, 4 байта)
//
// 8 размерностей за итерацию:
//   VPMOVZXBD — 8 uint8 → 8 int32 (zero-extend)
//   VCVTDQ2PS — 8 int32 → 8 float32
//   VFMADD213PS — approx = min + code*scale
//   VFMADD231PS — sum += diff*diff (euclidean) / sum += query*approx (dot)
// =============================================================================

// func euclideanSQ8AVX2(query []float32, codes []uint8, sqMin, sqScale []float32) float32
TEXT ·euclideanSQ8AVX2(SB), NOSPLIT, $0-100
    MOVQ  query_base+0(FP), AX
    MOVQ  query_len+8(FP), DI
    MOVQ  codes_base+24(FP), BX
    MOVQ  sqMin_base+48(FP), CX
    MOVQ  sqScale_base+72(FP), DX

    // zero-extend множитель для uint8: коды [0..255] → float32 напрямую через VPMOVZXBD.
    // Но VPMOVZXBD в plan9 требует специнского синтаксиса для broadcasting.
    // Используем VPERMILPS+VPSHUFB? Нет — проще: VPMOVZXBD работает с XMM→YMM в Go asm.
    //
    // Go plan9 asm: VPMOVZXBD берёт XMM-источник (16 байт), но нам нужно 8 bytes→8 int32.
    // Берём 8 bytes как низ XMM: VLDDQU не нужен — используем VPBROADCASTD-трюк?
    // Проще: читать 8 bytes через MOVD+PINSRD в scalar, но теряем SIMD.
    //
    // Альтернатива: читать по 32 bytes (8 uint8×4 канала)? Нет, dim=128 → 16 итераций.
    //
    // Правильный путь для plan9: VPMOVZXBD с XMM-источником (8 байт fit в low XMM).
    // VPMOVZXBD ymm, xmm/m64 — здесь m64 это 8 байт. В plan9: VPMOVZXBD (BX)(SI*1), Y4
    // — читает 8 байт, расширяет до 8 int32 в YMM. Это корректный синтаксис Go asm.

    VXORPS Y0, Y0, Y0           // sum = 0
    XORQ   SI, SI               // i = 0

    MOVQ DI, R8
    ANDQ $-8, R8                // R8 = floor(dim/8)*8

loop8_eucl:
    CMPQ SI, R8
    JGE  remainder_eucl

    VMOVUPS (AX)(SI*4), Y1      // Y1 = query[i:i+8] (8 float32)

    VPMOVZXBD (BX)(SI*1), Y4    // 8 uint8 → 8 int32 (zero-extend, YMM)
    VCVTDQ2PS  Y4, Y4           // 8 int32 → 8 float32

    VMOVUPS (CX)(SI*4), Y3      // Y3 = sqMin[i:i+8]
    VMOVUPS (DX)(SI*4), Y2      // Y2 = sqScale[i:i+8]
    VFMADD213PS Y3, Y2, Y4      // Y4 = Y2*Y4 + Y3 = scale*code + min = approx

    VSUBPS   Y4, Y1, Y5         // Y5 = query - approx = diff
    VFMADD231PS Y5, Y5, Y0      // Y0 += diff*diff

    ADDQ $8, SI
    JMP  loop8_eucl

remainder_eucl:
    // Horizontal sum Y0 → X0 (8 floats → 1)
    // Y0 = [f0..f7], нужно sum(f0..f7) в X0
    VEXTRACTF128 $1, Y0, X1     // X1 = high 4 floats
    VADDPS X1, X0, X0           // X0 = low+high (4 floats)
    VHADDPS X0, X0, X0          // X0 = [s01, s23, s01, s23]
    VHADDPS X0, X0, X0          // X0 = [s0123, ...] (broadcast scalar)

remainder_loop_eucl:
    CMPQ SI, DI
    JGE  done_eucl

    MOVSS   (AX)(SI*4), X1       // query[i]
    MOVBQZX (BX)(SI*1), R9       // codes[i] uint8 → R9 (zero-extend)
    CVTSQ2SS R9, X4             // int64 → float32
    MOVSS   (CX)(SI*4), X3       // sqMin[i]
    MOVSS   (DX)(SI*4), X2       // sqScale[i]
    MULSS   X2, X4               // code*scale
    ADDSS   X3, X4               // + min = approx
    SUBSS   X4, X1               // diff = query - approx
    VFMADD231SS X1, X1, X0       // sum += diff*diff

    INCQ SI
    JMP  remainder_loop_eucl

done_eucl:
    MOVSS X0, ret+96(FP)
    VZEROUPPER
    RET


// func dotProductSQ8AVX2(query []float32, codes []uint8, sqMin, sqScale []float32) float32
TEXT ·dotProductSQ8AVX2(SB), NOSPLIT, $0-100
    MOVQ  query_base+0(FP), AX
    MOVQ  query_len+8(FP), DI
    MOVQ  codes_base+24(FP), BX
    MOVQ  sqMin_base+48(FP), CX
    MOVQ  sqScale_base+72(FP), DX

    VXORPS Y0, Y0, Y0
    XORQ   SI, SI

    MOVQ DI, R8
    ANDQ $-8, R8

loop8_dot:
    CMPQ SI, R8
    JGE  remainder_dot

    VMOVUPS (AX)(SI*4), Y1      // query[i:i+8]

    VPMOVZXBD (BX)(SI*1), Y4    // 8 uint8 → 8 int32
    VCVTDQ2PS  Y4, Y4           // → float32

    VMOVUPS (CX)(SI*4), Y3      // sqMin
    VMOVUPS (DX)(SI*4), Y2      // sqScale
    VFMADD213PS Y3, Y2, Y4      // approx = min + code*scale

    VFMADD231PS Y1, Y4, Y0      // sum += query * approx

    ADDQ $8, SI
    JMP  loop8_dot

remainder_dot:
    VEXTRACTF128 $1, Y0, X1
    VADDPS X1, X0, X0
    VHADDPS X0, X0, X0
    VHADDPS X0, X0, X0

remainder_loop_dot:
    CMPQ SI, DI
    JGE  done_dot

    MOVSS   (AX)(SI*4), X1
    MOVBQZX (BX)(SI*1), R9       // codes[i] uint8 → R9
    CVTSQ2SS R9, X4             // → float32
    MOVSS   (CX)(SI*4), X3
    MOVSS   (DX)(SI*4), X2
    MULSS   X2, X4
    ADDSS   X3, X4               // approx
    VFMADD231SS X1, X4, X0       // sum += query*approx

    INCQ SI
    JMP  remainder_loop_dot

done_dot:
    MOVSS X0, ret+96(FP)
    VZEROUPPER
    RET
