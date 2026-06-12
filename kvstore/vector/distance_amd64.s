#include "textflag.h"

// func euclideanAVX2(a, b []float32) float32
//
// Квадрат евклидова расстояния: sum((a[i] - b[i])^2)
// FMA: VSUBPS + VFMADD231PS заменяет VSUBPS + VMULPS + VADDPS
// Все AVX2 CPU поддерживают FMA3 (Haswell 2013+).
TEXT ·euclideanAVX2(SB), NOSPLIT, $0-52
    MOVQ a_base+0(FP), AX
    MOVQ a_len+8(FP), BX
    MOVQ b_base+24(FP), CX

    VXORPS Y0, Y0, Y0
    XORQ SI, SI

    MOVQ BX, DX
    ANDQ $-8, DX

loop8:
    CMPQ SI, DX
    JGE remainder_start

    VMOVUPS (AX)(SI*4), Y1
    VMOVUPS (CX)(SI*4), Y2

    VSUBPS Y2, Y1, Y1          // Y1 = a - b
    VFMADD231PS Y1, Y1, Y0     // Y0 += Y1 * Y1 = Y0 + (a-b)^2

    ADDQ $8, SI
    JMP loop8

remainder_start:
    VEXTRACTF128 $1, Y0, X1
    VADDPS X1, X0, X0
    VHADDPS X0, X0, X0
    VHADDPS X0, X0, X0

remainder_loop:
    CMPQ SI, BX
    JGE done

    MOVSS (AX)(SI*4), X1
    MOVSS (CX)(SI*4), X2
    SUBSS X2, X1
    VFMADD231SS X1, X1, X0     // X0 += (a-b)^2

    INCQ SI
    JMP remainder_loop

done:
    MOVSS X0, ret+48(FP)
    VZEROUPPER
    RET

// func dotProductAVX2(a, b []float32) float32
//
// Скалярное произведение: sum(a[i] * b[i])
// FMA: VFMADD231PS заменяет VMULPS + VADDPS
TEXT ·dotProductAVX2(SB), NOSPLIT, $0-52
    MOVQ a_base+0(FP), AX
    MOVQ a_len+8(FP), BX
    MOVQ b_base+24(FP), CX

    VXORPS Y0, Y0, Y0
    XORQ SI, SI

    MOVQ BX, DX
    ANDQ $-8, DX

loop8_dot:
    CMPQ SI, DX
    JGE remainder_start_dot

    VMOVUPS (AX)(SI*4), Y1
    VMOVUPS (CX)(SI*4), Y2

    VFMADD231PS Y2, Y1, Y0     // Y0 += Y1 * Y2

    ADDQ $8, SI
    JMP loop8_dot

remainder_start_dot:
    VEXTRACTF128 $1, Y0, X1
    VADDPS X1, X0, X0
    VHADDPS X0, X0, X0
    VHADDPS X0, X0, X0

remainder_loop_dot:
    CMPQ SI, BX
    JGE done_dot

    MOVSS (AX)(SI*4), X1
    MOVSS (CX)(SI*4), X2
    VFMADD231SS X2, X1, X0     // X0 += X1 * X2

    INCQ SI
    JMP remainder_loop_dot

done_dot:
    MOVSS X0, ret+48(FP)
    VZEROUPPER
    RET
