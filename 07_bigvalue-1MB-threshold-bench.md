# 07 三库对比：阈值 1MB 下的大 value（512KB 内联 / 1.5MB 进 vlog）

日期: 2026-09-13/14
约定: **全程 sync_writes=false**；阈值 1MB（TrainKV/badger 显式传入；pebble 无 vlog/阈值概念）；
值为 10 字段 × 51200B ≈ 512KB（非阈值）与 × 153600B ≈ 1.5MB（超阈值）；threads=16（4 vCPU）。
TrainKV = 本地最新工作树（v2.0.5 内容 + 本次基准暴露的三处大 value 修复，见 §5）。

## 1. 配置与口径

| 项 | 值 |
|---|---|
| recordcount | 1,000（512KB 场景 ≈512MB/库；1.5MB 场景 ≈1.5GB/库） |
| operationcount | c=30,000；b=5,000；d=3,000；e=1,000；a=2,000；f=2,000 |
| 执行顺序 | 每库每场景 load 一次，随后 c→b→d→e→a→f（读类在前、写重的在后，避免 vlog 垃圾影响读类） |
| pebble | 显式 `cache_size=256MB`（默认 8MB 会 thrash） |
| badger/TrainKV | 引擎默认缓存；memtable 64MB |
| 数据量 | 负载逻辑写入累计 ≈28.3GB（<30GB 上限）；峰值磁盘占用 18GB（已清理） |

原始日志：/tmp/bench-v4/logs/（112 个文件，32MB）；TSV：/tmp/bench-v4/raw.tsv；脚本 /tmp/bench-v4/run.sh。
表格数值取自每次 run 的**最终** measurement 块（go-ycsb 每 10s 会打印一次累计快照，日志里有多块）。

## 2. 表一：值 512KB（非阈值，内联 LSM）— TOTAL ops/s（主操作 p99）

| 负载 | 主操作 | TrainKV | badger v4 | pebble |
|---|---|---|---|---|
| load | INSERT | **312.7** (173.6ms) | 250.4 (264.5ms) | 93.5 (1.62s) |
| a 50R/50U | READ | **1,145** (185.1ms) | 829.4 (56.0ms) | 217.1 (34.2ms) |
| b 95R/5U | READ | **6,381** (28.9ms) | 2,054 (47.8ms) | 2,192 (42.3ms) |
| c 100R | READ | **10,981** (30.6ms) | 3,027 (74.6ms) | 3,842 (35.1ms) |
| d 95R/5I | READ | **4,741** (41.5ms) | 1,478 (73.9ms) | 2,148 (58.7ms) |
| e 95scan/5I | SCAN | **3,018** (64.0ms) | 25.9 (3.65s) | 119.8 (478ms) |
| f 50R/50RMW | RMW | **2,310** (60.6ms) | 619.5 (176.6ms) | 149.5 (10.2s) |

## 3. 表二：值 1.5MB（超过 1MB 阈值，TrainKV/badger 进 vlog）— TOTAL ops/s（主操作 p99）

| 负载 | 主操作 | TrainKV | badger v4 | pebble* |
|---|---|---|---|---|
| load | INSERT | **102.8** (1.41s) | 88.0 (1.48s) | 7.9 (18.1s) |
| a 50R/50U | READ | **469.9** (154.2ms) | 416.3 (446.5ms) | 20.5 (81.9ms) |
| b 95R/5U | READ | **3,127** (31.8ms) | 3,073 (31.5ms) | 234.5 (57.4ms) |
| c 100R | READ | 5,404 (47.7ms) | **5,570** (41.8ms) | 2,016 (50.3ms) |
| d 95R/5I | READ | 1,534 (69.7ms) | **2,377** (47.7ms) | 184.4 (91.6ms) |
| e 95scan/5I | SCAN | **71.9** (2.82s) | 35.9 (1.87s) | 35.8 (1.28s) |
| f 50R/50RMW | RMW | 600.3 (1.41s) | **937.1** (125.6ms) | 23.3 (20.2s) |

\* pebble 无 vlog：1.5MB 值仍以块形式写 LSM，可视为"纯值变大"的对照。

## 4. 关键观察

1. **512KB 内联场景 TrainKV 七项全部第一**：c 1.1万 ops/s 为 badger 的 3.6 倍、pebble 的 2.9 倍；
   scan（e）3,018 vs badger 25.9（百倍级差距）、pebble 119.8。
2. **超阈值场景格局反转一部分**：vlog 把大 value 从 LSM 拆走后，badger 的读/更新路径反而顺畅
   （d/f 反超 TrainKV，c 以 5,570 vs 5,404 基本打平），TrainKV 仅在 b/e 保持领先；其 d/f 明显慢于内联场景
   （4,741→1,534；2,310→600），说明其**大 value 更新路径（整行重写 + vlog 追加 + 冲突检测）是当前短板**。
3. **pebble 最怕大 value 内联**：1.5MB 场景 load 7.9 ops/s、a 20.5、f 23.3（p99 达 18~20s），
   512KB 场景 e/f 也只有 119.8/149.5——LSM 内的大 value 重写 + 压实放大是根因。
4. **scan 是分水岭**：值越小，TrainKV 的块内条目密度优势越明显；值越大，三者都退化为逐 value 拷贝，
   差距收窄到 2 倍内（71.9 / 35.9 / 35.8）。
5. **事务冲突**：a/f 中 TrainKV 与 badger 各有约 1%~1.8% 的 update 因冲突被拒；pebble 适配器无事务层，0 错误。
   客户端 zipfian 键域 off-by-one 造成约 0.1% 的 READ_ERROR（三库一致，属 go-ycsb 客户端行为）。

## 5. 基准暴露的 TrainKV 缺陷与修复（重要）

首次跑到"值超阈值"场景时 TrainKV 直接崩溃（workload d 首个读操作 SIGSEGV；之后 e/a/f 连库都打不开）。
定位出三个独立问题，均已修复并补回归测试（`regression_bigvalue_test.go`），`go test -race` 全绿：

1. **vlog mmap 扩容竞态（SIGSEGV，本次崩溃主因）**
   `VLogFile.EncodeEntryAt` 在映射不够时调用 `Truncate`→`mremap(MREMAP_MAYMOVE)`，**未持有 `VLogFile.Lock`**；
   读者正持 `RLock` 引用旧映射做 `SafeCopy`。映射被搬移后旧地址失效 → SIGSEGV。
   race detector 直接命中 `file.(*MmapFile).Truncate`(写) vs `VLogFile.Read`(读) 同一 slice header。
   修复：扩容的 Truncate 用 `VLogFile.Lock.Lock()` 互斥（`vlogFile.go`）。
   *why 只在重开后必现*：关库会把活动 vlog 截断到实际长度，重开后映射紧贴数据末尾，第一次追加就 mremap。
2. **崩溃后残留空 WAL 的库无法重开**
   recovery 对空 WAL 执行 `Truncate(0)`：`ftruncate(0)` 成功后 `mremap(new_size=0)` 返回 EINVAL → panic。
   修复：`openMemTable` 仅在 `endOff > 0` 时截断（`lsm/memTable.go`）。
3. **极小阈值（如 1B）下 finTxn 标记进 vlog 导致库无法重开**
   `ShouldWriteValueToLSM` 用严格小于，阈值=1 时 finTxn 的 commitTs 明文也进 vlog，WAL 里只剩 ValuePtr，
   重放 `ParseUint` 失败 → ErrBadTxn。修复：`BitFinTxn` 的 value 强制内联（`db.go`）。
   对 1MB 阈值场景无行为变化（fin 值本来就内联）。

修复后 `RUN=2` 重跑 TrainKV 两个场景：46 个单元 rc=0、stderr 0 字节、全部有结果（修复前 above 场景 d/e/a/f 全部失败）。
`go.mod` 临时加了 `replace github.com/kebukeYi/TrainKV/v2 => ../TrainKV`，用于测本地修复版（`go version -m` 已核验 `=> ../TrainKV (devel)`）。

## 6. 注意事项

- 每格单轮（TrainKV 有修复前后两轮，差异 ±15% 内）；<10~25% 的差距按持平看。
- 16 线程跑在 4 vCPU 上（延续 06 报告口径），p99 含大量排队；大 value 下冷页缓存/GC 也会抬高分位。
- 测试数据已清理（18GB）；保留 /tmp/bench-v4/logs 与 raw.tsv 供复查。
