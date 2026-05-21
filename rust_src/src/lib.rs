use std::alloc::Layout;
use core::arch::wasm32::*;

// ─────────────────────────────────────────────────────────────────────────────
// Memory Management exports for wazero buffer allocation
// ─────────────────────────────────────────────────────────────────────────────

#[no_mangle]
pub unsafe extern "C" fn alloc(size: usize) -> *mut u8 {
    let layout = Layout::from_size_align_unchecked(size, 16);
    std::alloc::alloc(layout)
}

#[no_mangle]
pub unsafe extern "C" fn dealloc(ptr: *mut u8, size: usize) {
    let layout = Layout::from_size_align_unchecked(size, 16);
    std::alloc::dealloc(ptr, layout)
}

// ─────────────────────────────────────────────────────────────────────────────
// WASM SIMD (v128) Distance Kernels
// ─────────────────────────────────────────────────────────────────────────────

#[no_mangle]
pub unsafe extern "C" fn simd_euclidean_distance(a_ptr: *const f32, b_ptr: *const f32, len: usize) -> f32 {
    let mut sum_v = f32x4_splat(0.0);
    let chunks = len / 4;

    for i in 0..chunks {
        let a = v128_load(a_ptr.add(i * 4) as *const v128);
        let b = v128_load(b_ptr.add(i * 4) as *const v128);
        let diff = f32x4_sub(a, b);
        sum_v = f32x4_add(sum_v, f32x4_mul(diff, diff));
    }

    let mut sums = [0.0f32; 4];
    v128_store(sums.as_mut_ptr() as *mut v128, sum_v);
    
    let mut total = sums[0] + sums[1] + sums[2] + sums[3];

    for i in (chunks * 4)..len {
        let diff = *a_ptr.add(i) - *b_ptr.add(i);
        total += diff * diff;
    }

    total
}

#[no_mangle]
pub unsafe extern "C" fn simd_cosine_distance(a_ptr: *const f32, b_ptr: *const f32, len: usize) -> f32 {
    let mut dot_v = f32x4_splat(0.0);
    let mut norm_a_v = f32x4_splat(0.0);
    let mut norm_b_v = f32x4_splat(0.0);

    let chunks = len / 4;

    for i in 0..chunks {
        let a = v128_load(a_ptr.add(i * 4) as *const v128);
        let b = v128_load(b_ptr.add(i * 4) as *const v128);

        dot_v = f32x4_add(dot_v, f32x4_mul(a, b));
        norm_a_v = f32x4_add(norm_a_v, f32x4_mul(a, a));
        norm_b_v = f32x4_add(norm_b_v, f32x4_mul(b, b));
    }

    let mut dots = [0.0f32; 4];
    let mut norm_a = [0.0f32; 4];
    let mut norm_b = [0.0f32; 4];

    v128_store(dots.as_mut_ptr() as *mut v128, dot_v);
    v128_store(norm_a.as_mut_ptr() as *mut v128, norm_a_v);
    v128_store(norm_b.as_mut_ptr() as *mut v128, norm_b_v);

    let mut dot_sum = dots[0] + dots[1] + dots[2] + dots[3];
    let mut norm_a_sum = norm_a[0] + norm_a[1] + norm_a[2] + norm_a[3];
    let mut norm_b_sum = norm_b[0] + norm_b[1] + norm_b[2] + norm_b[3];

    for i in (chunks * 4)..len {
        let a_val = *a_ptr.add(i);
        let b_val = *b_ptr.add(i);

        dot_sum += a_val * b_val;
        norm_a_sum += a_val * a_val;
        norm_b_sum += b_val * b_val;
    }

    if norm_a_sum == 0.0 || norm_b_sum == 0.0 {
        return 1.0;
    }

    let similarity = dot_sum / (norm_a_sum.sqrt() * norm_b_sum.sqrt());
    1.0 - similarity
}
