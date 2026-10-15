# go-ycsb 纯写专项报告：TrainKV vs Badger v4.9.6

- 日期：2026-09-10
- 工具：go-ycsb（本地工作区修改版；trainkv / badger v4 适配器）
- 被测引擎：
  - **trainkv**：本地源码（`replace => ../TrainKV`），LSM + value log
  - **badger** v4.9.6（dgraph-io/badger/v4），LSM + value log

---

## 1. 测试方法

**两个阶段**（对应两种"纯写"口径）：
- `load`：批量写 `recordcount` 条（建底数，无脑追加的最顺风路径）
- `run -P workloads/workload_insert`：在已有数据上**持续插入** `operationcount` 条（有 flush/compaction 参与的真实写稳态）

**统一参数**：threads=16；行大小 1KB（fieldcount=10 × fieldlength=100）；两引擎两两配对、同一轮交替执行以减少时间偏差；每轮独立数据目录，load 前清库。

**测量口径**：仅使用 ops/s（墙钟）。go-ycsb 直方图延迟存在系统性失真（此前已定位），本报告不使用延迟数字。

**配置矩阵**：

| 配置 | 行布局 | 键序 | fsync | 规模 (load/insert) |
|---|---|---|---|---|
| **W1 基准** | 1MB 阈值→全内联 | hashed 随机 | 关 | 100k / 50k |
| **W2 顺序键** | 1MB 阈值→全内联 | ordered 顺序 | 关 | 100k / 50k |
| **W3 vlog 布局** | 32B 阈值→行进 value log | hashed 随机 | 关 | 100k / 50k |
| **W4 fsync** | 1MB 阈值→全内联 | hashed 随机 | 开(sync_writes=true) | 20k / 10k |

---

## 2. 结果（ops/s）

### W1 基准（内联 + 随机键 + 异步写）

| 阶段 | trainkv | badger | A/B |
|---|---|---|---|
| load（批量写 10 万） | 29819 | 23264 | **1.28x** |
| 持续插入（5 万） | 33445 | 22597 | **1.48x** |

### W2 顺序键（其余同 W1）

| 阶段 | trainkv | badger | A/B |
|---|---|---|---|
| load | 33562 | 23444 | **1.43x** |
| 持续插入 | 27275 | 23271 | **1.17x** |

### W3 vlog 布局（阈值 32B，其余同 W1）

| 阶段 | trainkv | badger | A/B |
|---|---|---|---|
| load | 32637 | 22495 | **1.45x** |
| 持续插入 | 34639 | 22943 | **1.51x** |

### W4 fsync（sync_writes=true；2 万 / 1 万）

| 阶段 | trainkv | badger | A/B |
|---|---|---|---|
| load | 168.0 | 148.3 | **1.13x** |
| 持续插入 | 168.4 | 152.5 | **1.10x** |

---

## 3. 观察与结论

1. **异步写（sync=false）下 trainkv 全面领先 1.17–1.51x**，且**持续插入阶段优势最大**（W1 1.48x、W3 1.51x）——即"有存量数据、后台合并真实发生"的稳态下差距比批量 load 更大。
2. **两种写入规模的结论不同**：批量 load 是各引擎最顺风的路径；持续插入才是真实稳态。只测其中一个会失真（例如 W2 中 trainkv 两个阶段差了 23%）。
3. **行布局（内联 vs vlog）**：trainkv 的 vlog 写路径不逊于内联（W3 甚至略优于 W1）；badger 对阈值不敏感（其写路径对 1KB 行差异不显著）。
4. **键序影响**：
   - trainkv 的批量 load 在 ordered 下更快（33.6k vs 29.8k，+13%），符合 LSM 顺序键的预期；
   - 但**持续插入阶段 ordered 反而慢于 hashed**（27.3k vs 33.4k，-18%）——新键集中在单一递增区间可能引发特定的 memtable/flush/vlog 追加模式，值得单独排查；
   - badger 对键序不敏感（23.3k vs 22.6k，噪声范围内）。
5. **fsync 是终极收敛器**：开启后两者都降到 ~150–170 ops/s（约 6ms/次，fsync 主导，group commit 已在 16 线程下摊销），差距收窄到 1.10–1.13x。说明 tempi 异步写时 trainkv 领先的 1.3–1.5x 主要来自内存写管线效率，而非磁盘路径；两者真实落盘成本接近。

## 4. 复现

```bash
# W1/W2/W3 替换 EXTRA 与阈值即可；WORKLOADS="_insert" 触发纯写 workload
RECORDCOUNT=100000 OPERATIONCOUNT=50000 THREADS=16 WORKLOADS="_insert" \
EXTRA_A="-p trainkv.value_threshold=1048576 -p insertorder=hashed" \
EXTRA_B="-p badger.value_threshold=1048576 -p badger.compression=none -p insertorder=hashed" \
bash tool/ab_bench.sh trainkv badger

# W4 加 -p trainkv.sync_writes=true / -p badger.sync_writes=true, 规模降为 2 万/1 万
```

---

## 附录：原始输出行（go-ycsb 摘要）

```
W1: load INSERT 29818.8 vs 23263.5 (1.28x);  _insert INSERT 33444.6 vs 22597.1 (1.48x)
W2: load INSERT 33562.2 vs 23444.2 (1.43x);  _insert INSERT 27274.5 vs 23271.3 (1.17x)
W3: load INSERT 32637.3 vs 22494.5 (1.45x);  _insert INSERT 34639.0 vs 22942.5 (1.51x)
W4: load INSERT   168.0 vs   148.3 (1.13x);  _insert INSERT   168.4 vs   152.5 (1.10x)
```

## 附录 B：测量限制

- 本报告仅用墙钟口径 ops/s；跨轮噪声存在（本会话观测过 ±10–30%），同轮配对的比值最可靠。
- 场景为 WSL2 + 4C/8T + 1KB 行；大 value（>1MB）场景见此前的三引擎报告（vlog 分离优势 3–5x）。
