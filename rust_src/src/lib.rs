use std::alloc::Layout;
use std::collections::{HashMap, HashSet, BinaryHeap};
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

// ─────────────────────────────────────────────────────────────────────────────
// PCG-based fast pseudo-random number generator for zero-dependency level gen
// ─────────────────────────────────────────────────────────────────────────────

pub struct Lcg {
    state: u64,
}

impl Lcg {
    pub fn new(seed: u64) -> Self {
        Self { state: seed }
    }

    pub fn next_u32(&mut self) -> u32 {
        self.state = self.state.wrapping_mul(1664525).wrapping_add(1013904223);
        (self.state >> 32) as u32
    }

    pub fn next_f64(&mut self) -> f64 {
        let val = self.next_u32();
        (val as f64) / (u32::MAX as f64)
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// HNSW Internal Data Structures and Implementations
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Clone)]
pub struct RustNode {
    pub id: u64,
    pub vector: Vec<f32>,
    pub level: usize,
    pub neighbors: Vec<Vec<u64>>, // neighbors[level]
}

pub struct RustHNSW {
    pub nodes: HashMap<u64, RustNode>,
    pub entry_point_id: u64,
    pub max_level: usize,
    pub m: usize,
    pub m0: usize,
    pub ml: f64,
    pub ef_construction: usize,
    pub dim: usize,
    pub metric: u32, // 0 = Euclidean, 1 = Cosine
    pub rng: Lcg,
}

#[derive(Copy, Clone, Debug)]
struct CandidateItem {
    id: u64,
    dist: f32,
}

impl PartialEq for CandidateItem {
    fn eq(&self, other: &Self) -> bool {
        self.dist == other.dist
    }
}

impl Eq for CandidateItem {}

impl Ord for CandidateItem {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        // Min-heap order: smaller distance has HIGHER priority
        other.dist.partial_cmp(&self.dist).unwrap_or(std::cmp::Ordering::Equal)
    }
}

impl PartialOrd for CandidateItem {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}

#[derive(Copy, Clone, Debug)]
struct ResultItem {
    id: u64,
    dist: f32,
}

impl PartialEq for ResultItem {
    fn eq(&self, other: &Self) -> bool {
        self.dist == other.dist
    }
}

impl Eq for ResultItem {}

impl Ord for ResultItem {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        // Max-heap order: larger distance has HIGHER priority
        self.dist.partial_cmp(&other.dist).unwrap_or(std::cmp::Ordering::Equal)
    }
}

impl PartialOrd for ResultItem {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}

impl RustHNSW {
    pub fn new(dim: usize, metric: u32, seed: u64) -> Self {
        let m = 16;
        let m0 = 32;
        let ml = 1.0 / (m as f64).ln();
        Self {
            nodes: HashMap::new(),
            entry_point_id: 0,
            max_level: 0,
            m,
            m0,
            ml,
            ef_construction: 200,
            dim,
            metric,
            rng: Lcg::new(seed),
        }
    }

    fn calculate_distance(&self, a: &[f32], b: &[f32]) -> f32 {
        unsafe {
            match self.metric {
                0 => simd_euclidean_distance(a.as_ptr(), b.as_ptr(), self.dim),
                1 => simd_cosine_distance(a.as_ptr(), b.as_ptr(), self.dim),
                _ => panic!("unknown metric"),
            }
        }
    }

    fn random_level(&mut self) -> usize {
        let rand_val = 1.0 - self.rng.next_f64();
        // Avoid log(0)
        let rand_val = if rand_val <= 0.0 { 1e-10 } else { rand_val };
        (-rand_val.ln() * self.ml) as usize
    }

    fn max_neighbors(&self, level: usize) -> usize {
        if level == 0 {
            self.m0
        } else {
            self.m
        }
    }

    fn search_layer(&self, query: &[f32], entry_id: u64, ef: usize, level: usize) -> Vec<(u64, f32)> {
        let mut visited = HashSet::new();
        let mut candidates = BinaryHeap::new(); // Min-heap of CandidateItem
        let mut results = BinaryHeap::new();    // Max-heap of ResultItem

        let entry_node = &self.nodes[&entry_id];
        let entry_dist = self.calculate_distance(query, &entry_node.vector);

        visited.insert(entry_id);

        candidates.push(CandidateItem { id: entry_id, dist: entry_dist });
        results.push(ResultItem { id: entry_id, dist: entry_dist });

        while let Some(closest) = candidates.pop() {
            let farthest_result = results.peek().unwrap();

            if closest.dist > farthest_result.dist {
                break;
            }

            let node = &self.nodes[&closest.id];
            if level < node.neighbors.len() {
                for &neighbor_id in &node.neighbors[level] {
                    if visited.insert(neighbor_id) {
                        let neighbor_node = &self.nodes[&neighbor_id];
                        let neighbor_dist = self.calculate_distance(query, &neighbor_node.vector);

                        let farthest_result = results.peek().unwrap();

                        if neighbor_dist < farthest_result.dist || results.len() < ef {
                            candidates.push(CandidateItem { id: neighbor_id, dist: neighbor_dist });
                            results.push(ResultItem { id: neighbor_id, dist: neighbor_dist });

                            if results.len() > ef {
                                results.pop();
                            }
                        }
                    }
                }
            }
        }

        let mut sorted_results = Vec::new();
        while let Some(item) = results.pop() {
            sorted_results.push((item.id, item.dist));
        }
        sorted_results.reverse();
        sorted_results
    }

    pub fn insert(&mut self, id: u64, vec: Vec<f32>) {
        let level = self.random_level();

        let new_node = RustNode {
            id,
            vector: vec,
            level,
            neighbors: vec![Vec::new(); level + 1],
        };

        if self.nodes.is_empty() {
            self.nodes.insert(id, new_node);
            self.entry_point_id = id;
            self.max_level = level;
            return;
        }

        self.nodes.insert(id, new_node);

        let mut ep = self.entry_point_id;

        // Phase 1: Descent
        for lc in (level + 1..=self.max_level).rev() {
            let results = self.search_layer(&self.nodes[&id].vector, ep, 1, lc);
            if let Some(&(best_id, _)) = results.first() {
                ep = best_id;
            }
        }

        // Phase 2: Connect and Prune
        let start_level = std::cmp::min(level, self.max_level);
        for lc in (0..=start_level).rev() {
            let results = self.search_layer(&self.nodes[&id].vector, ep, self.ef_construction, lc);

            let m_max = self.max_neighbors(lc);
            let count = std::cmp::min(results.len(), m_max);
            let neighbor_ids: Vec<u64> = results.iter().take(count).map(|&(r_id, _)| r_id).collect();

            self.nodes.get_mut(&id).unwrap().neighbors[lc] = neighbor_ids.clone();

            for &neighbor_id in &neighbor_ids {
                let neighbor = self.nodes.get_mut(&neighbor_id).unwrap();
                neighbor.neighbors[lc].push(id);

                if neighbor.neighbors[lc].len() > m_max {
                    self.prune_neighbors(neighbor_id, lc, m_max);
                }
            }

            if let Some(&(best_id, _)) = results.first() {
                ep = best_id;
            }
        }

        if level > self.max_level {
            self.entry_point_id = id;
            self.max_level = level;
        }
    }

    fn prune_neighbors(&mut self, node_id: u64, level: usize, max_count: usize) {
        let node_vec = self.nodes[&node_id].vector.clone();
        let neighbor_ids = &self.nodes[&node_id].neighbors[level];

        let mut items = Vec::new();
        for &nid in neighbor_ids {
            let dist = self.calculate_distance(&node_vec, &self.nodes[&nid].vector);
            items.push((nid, dist));
        }

        items.sort_by(|a, b| a.1.partial_cmp(&b.1).unwrap_or(std::cmp::Ordering::Equal));

        if items.len() > max_count {
            items.truncate(max_count);
        }

        let pruned_ids: Vec<u64> = items.iter().map(|&(nid, _)| nid).collect();
        self.nodes.get_mut(&node_id).unwrap().neighbors[level] = pruned_ids;
    }

    pub fn delete(&mut self, id: u64) -> bool {
        if !self.nodes.contains_key(&id) {
            return false;
        }

        let node = &self.nodes[&id];
        let level_limit = node.level;
        let neighbors_by_level = node.neighbors.clone();

        for level in 0..=level_limit {
            let neighbors = &neighbors_by_level[level];
            let m_max = self.max_neighbors(level);

            for &neighbor_id in neighbors {
                if let Some(neighbor) = self.nodes.get_mut(&neighbor_id) {
                    if let Some(pos) = neighbor.neighbors[level].iter().position(|&x| x == id) {
                        neighbor.neighbors[level].remove(pos);
                    }

                    for &other_id in neighbors {
                        if other_id == neighbor_id {
                            continue;
                        }
                        if neighbor.neighbors[level].contains(&other_id) {
                            continue;
                        }
                        neighbor.neighbors[level].push(other_id);
                    }
                }

                let current_len = self.nodes[&neighbor_id].neighbors[level].len();
                if current_len > m_max {
                    self.prune_neighbors(neighbor_id, level, m_max);
                }
            }
        }

        for (&n_id, n) in self.nodes.iter_mut() {
            if n_id == id {
                continue;
            }
            for lvl in 0..=n.level {
                if let Some(pos) = n.neighbors[lvl].iter().position(|&x| x == id) {
                    n.neighbors[lvl].remove(pos);
                }
            }
        }

        self.nodes.remove(&id);

        if id == self.entry_point_id {
            if self.nodes.is_empty() {
                self.entry_point_id = 0;
                self.max_level = 0;
            } else {
                let mut new_max_level = -1;
                let mut new_ep = 0;
                for (&nid, n) in &self.nodes {
                    if n.level as isize > new_max_level {
                        new_max_level = n.level as isize;
                        new_ep = nid;
                    }
                }
                self.entry_point_id = new_ep;
                self.max_level = new_max_level as usize;
            }
        }

        true
    }

    pub fn search(&self, query: &[f32], k: usize, ef_search: usize) -> Vec<(u64, f32)> {
        if self.nodes.is_empty() {
            return Vec::new();
        }

        let mut ef = ef_search;
        if ef < k {
            ef = k;
        }

        let mut ep = self.entry_point_id;

        for lc in (1..=self.max_level).rev() {
            let results = self.search_layer(query, ep, 1, lc);
            if let Some(&(best_id, _)) = results.first() {
                ep = best_id;
            }
        }

        let mut results = self.search_layer(query, ep, ef, 0);

        if results.len() > k {
            results.truncate(k);
        }

        results
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// C-compatible WASM ABI exports
// ─────────────────────────────────────────────────────────────────────────────

#[no_mangle]
pub unsafe extern "C" fn wasm_hnsw_new(dim: u32, metric: u32, seed: u64) -> *mut RustHNSW {
    let hnsw = Box::new(RustHNSW::new(dim as usize, metric, seed));
    Box::into_raw(hnsw)
}

#[no_mangle]
pub unsafe extern "C" fn wasm_hnsw_free(ptr: *mut RustHNSW) {
    if !ptr.is_null() {
        let _ = Box::from_raw(ptr);
    }
}

#[no_mangle]
pub unsafe extern "C" fn wasm_hnsw_insert(ptr: *mut RustHNSW, id: u64, vec_ptr: *const f32) {
    let hnsw = &mut *ptr;
    let vec = std::slice::from_raw_parts(vec_ptr, hnsw.dim).to_vec();
    hnsw.insert(id, vec);
}

#[no_mangle]
pub unsafe extern "C" fn wasm_hnsw_delete(ptr: *mut RustHNSW, id: u64) -> u32 {
    let hnsw = &mut *ptr;
    if hnsw.delete(id) {
        1
    } else {
        0
    }
}

#[no_mangle]
pub unsafe extern "C" fn wasm_hnsw_search(
    ptr: *mut RustHNSW,
    query_ptr: *const f32,
    k: u32,
    ef_search: u32,
    out_ids_ptr: *mut u64,
    out_dists_ptr: *mut f32,
) -> u32 {
    let hnsw = &*ptr;
    let query = std::slice::from_raw_parts(query_ptr, hnsw.dim);
    let results = hnsw.search(query, k as usize, ef_search as usize);

    for (i, &(id, dist)) in results.iter().enumerate() {
        *out_ids_ptr.add(i) = id;
        *out_dists_ptr.add(i) = dist;
    }

    results.len() as u32
}

#[no_mangle]
pub unsafe extern "C" fn wasm_hnsw_clear(ptr: *mut RustHNSW) {
    let hnsw = &mut *ptr;
    hnsw.nodes.clear();
    hnsw.entry_point_id = 0;
    hnsw.max_level = 0;
}

#[no_mangle]
pub unsafe extern "C" fn wasm_hnsw_info(
    ptr: *mut RustHNSW,
    out_nodes_ptr: *mut u32,
    out_dim_ptr: *mut u32,
    out_max_level_ptr: *mut u32,
) {
    let hnsw = &*ptr;
    *out_nodes_ptr = hnsw.nodes.len() as u32;
    *out_dim_ptr = hnsw.dim as u32;
    *out_max_level_ptr = hnsw.max_level as u32;
}
