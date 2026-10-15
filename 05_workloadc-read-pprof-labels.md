# 05 workloadc 纯读 + 关键路径 pprof 标签（TrainKV）

日期: 2026-09-11
引擎: TrainKV（本地源码, `replace => ../TrainKV`）
数据目录: `/tmp/wc-trainkv`（100,000 条 × ~1KB）
Profile/日志目录: `/tmp/prof-workloadc/`
约定: **全程 sync_writes=false**（load 与 run 都显式传 `-p trainkv.sync_writes=false`）

---

## 1. 本次代码改动：Get/Put/Scan 注入 pprof 标签

文件: `db/trainkv/db.go`

- 预构建标签集（避免每 op 重新分配）：
  - `Read` → `op=get`；`Update` / `Insert` → `op=put`；`Delete` → `op=delete`；`Scan` → `op=scan`
- 统一入口：

```go
var (
	labelGet    = pprof.Labels("op", "get")
	labelPut    = pprof.Labels("op", "put")
	labelDelete = pprof.Labels("op", "delete")
	labelScan   = pprof.Labels("op", "scan")
)

func withLabels(ctx context.Context, labels pprof.LabelSet, f func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	pprof.Do(ctx, labels, func(context.Context) { f() })
}
```

- 每个方法拆成"带标签的公开方法 + 私有实现"（`Read/read`、`Update/update`、…）。
  原因：若把原方法体直接塞进闭包，闭包内的 `return nil, err` 只会从闭包返回，命名返回值拿不到，容易埋雷。
- 标签是**常驻**的（没有开关）。单 op 是常数级小开销；下面两次 run 的吞吐差（281.9k → 224.5k）主要来自
  `debug.blockprofile=true`（`SetBlockProfileRate(1)` 逐事件采样 + mutex 全采样），不是标签本身。
- 兼容性说明（Go 1.27 源码确认）：
  - **CPU / goroutine** profile 会记录用户标签（`runtimeProfile.Label`）；
  - **block / mutex / heap** 不记录（`stackProfile.Label()` 恒返回 nil、`MemProfileRecord` 无 labels 字段）。
  - 因此按 `op` 切分只看 CPU/goroutine；block/mutex 靠调用栈归因。

---

## 2. 测试配置

```bash
# load（100k 条）
./bin/go-ycsb load trainkv -P workloads/workloadc -p dropdata=true \
  -p recordcount=100000 --threads 16 \
  -p trainkv.dir=/tmp/wc-trainkv -p trainkv.sync_writes=false

# run：采集场次（15M 次纯读，开 block/mutex 采样，profile 与报告同场）
./bin/go-ycsb run trainkv -P workloads/workloadc -p recordcount=100000 \
  -p operationcount=15000000 --threads 16 \
  -p trainkv.dir=/tmp/wc-trainkv -p trainkv.sync_writes=false \
  -p debug.blockprofile=true
```

`threadcount=16`（4 vCPU），requestdistribution=zipfian，readallfields=true。

---

## 3. 原生结果（go-ycsb 原始输出）

### 3.1 采集场次（`debug.blockprofile=true`，与 profile 同场）

```
Run finished, takes 1m6.807701584s
READ   - Takes(s): 66.8, Count: 14999907, OPS: 224524.0, Avg(us): 66, Min(us): 3, Max(us): 40095, 50th(us): 5, 90th(us): 13, 95th(us): 86, 99th(us): 1847, 99.9th(us): 4779, 99.99th(us): 11255
READ_ERROR - Takes(s): 65.7, Count: 93, OPS: 1.4, Avg(us): 29, Min(us): 1, Max(us): 2159, 50th(us): 2, 90th(us): 3, 95th(us): 23, 99th(us): 275, 99.9th(us): 2159, 99.99th(us): 2159
TOTAL  - Takes(s): 66.8, Count: 14999907, OPS: 224523.1, Avg(us): 66, Min(us): 3, Max(us): 40095, 50th(us): 5, 90th(us): 13, 95th(us): 86, 99th(us): 1847, 99.9th(us): 4779, 99.99th(us): 11255
```

| 指标 | 值 |
|---|---|
| 吞吐 | **224,524 read ops/s**（15,000,000 op 共 66.8s） |
| Avg / p50 / p90 | 66µs / 5µs / 13µs |
| p95 / **p99** | 86µs / **1,847µs** |
| p99.9 / p99.99 | 4,779µs / 11,255µs |
| Max | 40,095µs |

### 3.2 无 profile 干扰对照（1M op，同线程数）

```
READ   - Takes(s): 3.5, Count: 999992, OPS: 281890.2, Avg(us): 52, Min(us): 3, Max(us): 13719,
         50th(us): 5, 90th(us): 11, 95th(us): 43, 99th(us): 1503, 99.9th(us): 3495, 99.99th(us): 6343
```

| 口径 | 吞吐 | p95 | p99 | p99.9 |
|---|---|---|---|---|
| 无 profiling（3.2） | 281,890 | 43µs | 1,503µs | 3,495µs |
| 全量 block/mutex 采样（3.1） | 224,524 | 86µs | 1,847µs | 4,779µs |
| 采样开销 | **-20.4%** | ×2.0 | +23% | +37% |

结论：**P99 及以上的数字要标注是否开了 profiling**；讨论引擎尾延迟时以 3.2 口径为准（或关掉 `debug.blockprofile` 再跑）。

### 3.3 P99 尾延迟专项观察

1. **分布形状是"5µs 主体 + ~2ms 长尾"**：p50 5µs → p95 86µs，但 p99 直接到 1.8ms（p95→p99 跳 21 倍），p99.9 4.8ms。
   典型读路径（内存命中）在 10µs 内，尾部有 1/100 的请求要多花两个数量级时间。
2. **尾巴随并发显著放大（4 vCPU 上跑 16 线程）**：

   | 口径（均无 profiling） | 吞吐 | p99 | Max |
   |---|---|---|---|
   | 单线程（见 4.2 的对照记录） | 134,568 | **32µs** | 11,431µs |
   | 16 线程 | 281,890 | **1,503µs** | 13,719µs |

   单线程 p99 只有 32µs，16 线程放大到 1.5ms（>40 倍），吞吐却只涨 2.1 倍——尾部主要来自
   多线程下的排队/调度竞争，而非单次读路径本身。结合 04 号报告的结论（mark 管线每 op 同步握手、
   goroutine 可运行但等 CPU），P99 尾部的放大机制预期落在 CPU/block/goroutine 三份 profile 里，等你分析。
3. 注意 go-ycsb 直方图是客户端口径且有已知偏差（此前结论：绝对值偏乐观 5~10x），比较不同引擎/配置时
   看相对关系更可靠。

---

## 4. 本次顺带定位的两个问题（都不是 TrainKV 的 bug）

### 4.1 READ_ERROR 93/15M ≈ 6e-6：go-ycsb keyrange 的闭区间 off-by-one

证据链：

- `pkg/workload/core.go` zipfian 分支：`keyrangeUpperBound = insertStart + insertCount + expectedNewKeys`。
  workloadc 的 `insertProportion=0` → 上限 = 0 + 100000 + 0 = **100000**（打印也确认 `keyrange of [0 100000]`）。
- `pkg/generator/scrambled_zipfian.go`：`itemCount = max - min + 1`，`n = min + Hash(n)%itemCount` →
  取值是 **闭区间 [0, 100000]**。
- 而 load 只写入 key 0..99999（`Counter` 从 insertStart 起自增 insertCount 次）。
- 所以读到的 key=100000 必然 `ErrKeyNotFound`，被 `DbWrapper` 记成 `READ_ERROR`（错误详情不透传）。
  zipfian 下最小热门 rank 的概率极低，正好是 ~1e-5 量级，与实测 8/1M、93/15M 吻合。

规避办法（要跑"零错误"的纯读）：load 时 `recordcount=N+1`、run 时 `recordcount=N`，让 0..N 全部存在。

### 4.2 重要：`-p threads=N` 一直是静默失效的（旧报告口径需要修正）

- go-ycsb 只认 `threadcount` 属性（`pkg/client/client.go: threadCount := c.p.GetInt(prop.ThreadCount, 1)`），
  `-p threads=16` 是**没人读的属性**，实际按默认 **1 线程**跑。
- 实测证据：
  - `-p threads=16` 的 run，properties 打印里 grep 不到 `"threadcount"`（计数 0）；
  - 同配置只有 134,568 ops/s，而 `--threads 16` 是 281,890 ops/s；
  - 真 16 线程 load = 108,577 ops/s，旧文档里的 "T16 load 31.7k" 与之相差 3.4 倍（=单线程水平）。
- **影响范围**：01~04 号报告与练习命令里的 `-p threads=16` 实际都是单线程数据；
  - 引擎间对比（trainkv vs badger/pebble）**结论仍成立**——双方同参数同口径；
  - 但"T16/T1 线程扩展性"的推论要被推翻（两边其实都是 1 线程），B2 回归的判定不受影响（单线程对照仍是有效对照）。
- 已修复：`tool/ab_bench.sh` 改用 `--threads "$THREADS"`，并在头注释写明该陷阱。
  （`-p threadcount=N` 同样有效；`--threads N` 是官方 flag。）

### 4.3 日志杂音

`doCompact: unable to fill tables` 出现 1 次（100k 小数据集下 compaction 选不到足够 table 的提示），非错误。

---

## 5. Profile 文件与采集入口

目录 `/tmp/prof-workloadc/`：

| 文件 | 说明 |
|---|---|
| `cpu.pprof` (88KB) | 20s 窗口 CPU；`op` 标签覆盖 **36.47s/54.20s = 67.3%，全部为 `get`**（其余为运行时/后台未打标） |
| `goroutine.pprof` / `goroutine2.pprof` / `goroutine.txt` | goroutine 快照（proto ×2 + debug=1 文本），标签同样可见 |
| `block.pprof` (4KB) | `debug.blockprofile=true`（rate=1）采集，含阻塞事件与栈 |
| `mutex.pprof` (14KB) | 锁竞争全采样 |
| `heap.pprof` / `allocs.pprof` | inuse / 累计分配 |
| `threadcreate.pprof` | OS 线程创建栈 |
| `wc-run.log` | 3.1 的完整原生输出（含 properties 打印） |
| `wc-threadsbug.log` | 4.2 的 `-p threads=16` 单线程对照 run |

采集命令（run 在后台跑的同时另开终端）：

```bash
curl -sS -o cpu.pprof     "http://127.0.0.1:6060/debug/pprof/profile?seconds=20"
curl -sS -o heap.pprof    "http://127.0.0.1:6060/debug/pprof/heap"
curl -sS -o allocs.pprof  "http://127.0.0.1:6060/debug/pprof/allocs"
curl -sS -o block.pprof   "http://127.0.0.1:6060/debug/pprof/block"
curl -sS -o mutex.pprof   "http://127.0.0.1:6060/debug/pprof/mutex"
curl -sS -o goroutine.txt "http://127.0.0.1:6060/debug/pprof/goroutine?debug=1"
```

分析入口（标签用法）：

```bash
go tool pprof -tags /tmp/prof-workloadc/cpu.pprof                    # 标签分布
go tool pprof -tagfocus=op=get -top /tmp/prof-workloadc/cpu.pprof    # 只看读路径
go tool pprof -tagshow=op -top /tmp/prof-workloadc/cpu.pprof         # 输出带 op 列
go tool pprof -sample_index=delay -top /tmp/prof-workloadc/block.pprof
go tool pprof -top /tmp/prof-workloadc/mutex.pprof
go tool pprof -sample_index=alloc_space -top /tmp/prof-workloadc/allocs.pprof
go tool pprof -sample_index=inuse_space -top /tmp/prof-workloadc/heap.pprof
```

---

## 6. 清理

- 数据：`rm -rf /tmp/wc-trainkv`
- Profile 与日志：`rm -rf /tmp/prof-workloadc`

---

## 7. 纯读结果深度分析（补充）

### 7.1 线程扩展曲线（每档 5M 次读，全程关闭 profiling，同一份数据）

| threads | ops/s | 相对 T1 | 实际用核(user+sys/墙钟) | CPU/op | p50 | p95 | p99 | p99.9 | Avg |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 136,758 | 1.00x | 1.42 | 10.5µs | 4µs | 9µs | 32µs | 136µs | 5µs |
| 4 | 257,441 | 1.88x | 2.66 | 10.4µs | 5 | 20 | 193 | 743 | 12 |
| 8 | 306,028 | 2.24x | 2.78 | 9.2 | 5 | 23 | 619 | 1,599 | 22 |
| 16 | 313,251 | 2.29x | 2.79 | 9.0 | 5 | 26 | **1,429** | 3,327 | 47 |

- **T4（1 线程/核）只有 47% 的线性扩展效率**：4×136.8k=547k，实测 257k。
- **T8→T16 吞吐持平（+2%）**，p99 却从 619µs 涨到 1,429µs —— 加线程只买到了尾部。
- **用核数封顶 ~2.8/4**：吞吐上限不是算力不足，而是存在等待/串行环节。
- **尾部放大**：p99 从 32µs 涨到 1,429µs（×44），p99.9 ×24。T16 下均值 47µs 里约 85% 来自最慢的 5%
  （按分位估算：bottom-95% 均值 ~6µs → top-5% 均值 ~826µs）。

日志：`/tmp/prof-workloadc/scale-t{1,4,8,16}.log`。

### 7.2 三个对照实验

1. **harness 自身上限（basic 空 DB，不做存储）**：T1 1.03M / T4 1.55M / T16 1.30M ops/s（≈0.97µs/op）。
   → 测量框架（key 生成 + 全局测量锁 + worker 循环）上限 ~1.5M，**TrainKV 的 313k 不是框架限制**；
   且空 DB 在 16 线程同样回退（1.55M→1.30M），说明 16:4 超配自身有成本。
   日志：`basic-t{1,4,16}.log`。
2. **GC**：`GODEBUG=gctrace=1`，97 次 GC/6.8s（约 70ms 一轮），单次 STW 0.15~0.22ms，GC CPU 占比 1%；
   `GOGC=400` 对照无改善（289.6k vs 313.3k，-7.5% 属噪声）。
   → GC/STW 可以解释 p99.9 的 ms 级偶发，**不是吞吐平台的主因**；但分配量可观：每 GC 周期分配 ~70MB /
   2.07 万 op ≈ **3.4KB/op ≈ 1GB/s**。日志：`gctrace-t16.log`、`gogc400-t16.log`。
3. **历史单线程跨引擎**（01 报告，50k op 短跑，实为单线程）：trainkv 97–108k、pebble 107–114k、badger 43–53k。
   → 单线程读路径 TrainKV 不虚甚至略优；**多线程跨引擎对比目前是空白**（旧数据全是 1 线程），如需对比要重跑。

### 7.3 事实链与嫌疑排序

每 op CPU 恒定在 9~10.5µs（harness 空转仅 ~1µs），4 核理论上限 ~400k，实测 313k 且只用了 2.8 核
→ 少掉的产能和暴涨的尾部都指向"与并发度相关的等待点"：

1. **startMark 标记管线（头号嫌疑）**：每个读事务 2 次 `markCh` 投递（Begin + Done，缓冲区 100），
   全进程**单个** `processOn` 协程串行消费（堆 + map 维护水位）。这是唯一横跨所有读线程的串行环节；
   04 报告 trace 也已在同路径看到每 op ~18µs 的调度等待。→ 验证点：CPU profile 未打标的 32.7%、
   block profile 的 `chansend1`、goroutine 快照里 processOn 的运行/阻塞状态。
2. **16:4 超配**：harness 空转也有同样回退；p99 从 T4 的 193µs 涨到 T16 的 1,429µs。
3. **分配/GC 尾巴**：3.4KB/op、每 70ms 一次 STW 0.2ms —— 量级只够解释 p99.9 的 ms 级抖动。
4. `tsLock` / `commitMark`：纯读下走无竞争/原子快路径，量级小。

### 7.4 建议

- 扩展性基准固定用 `--threads 4`（1 线程/核）；T8/T16 只当"超配压力场景"看。
- 报 P99 必须标注是否开了 profiling（本次开关差 -20% 吞吐 / +23% p99）。
- 若要验证嫌疑 1：把 `markCh` 容量放大 / Begin-Done 批量化 / startMark 离路化，任一改动后重跑
  1/4/16 曲线做同口径 A/B（T4 是最灵敏的一档）。

---

## 8. 优化落地 ②：解码单趟化（去中间 map）+ 行键去 Sprintf（2026-09-12）

### 8.1 先纠正一个判断：`model.SafeCopy` 删不掉

profile 里 `model.SafeCopy`（allocs 7.38GB / CPU cum 4.4%）看着像"冗余拷贝"，但查解码实现发现：
go-ycsb 的 `RowCodec.Decode` 走 `DecodeRow` → `decodeBytes`，返回的是**输入 row 的子切片**
（`pkg/util/row.go:114` `return remain[n:], remain[:n], nil`，不拷贝）。也就是说返回给 go-ycsb 的 map 里
10 个 field 切片**直接指向引擎 `db.go:129` 那块 SafeCopy 内存** —— SafeCopy 是这份数据的底层存储，
删掉它 map 就会悬垂（vlog mmap 会被 rollover/GC 重新映射）。

→ **拷贝不可去**（用 `map[string][]byte` 做接口返回值时，至少一次拷贝 + 一块 owned 内存是硬需求）；
真正的浪费是 `DecodeRow` 的**中间 `map[int64][]byte`**：allocs 5.80GB（28%），CPU 6.25s（11.5%，
其中 `mapassign_fast64` 一项 4.56s）——纯属二次搬运。

### 8.2 改动（3 个文件）

1. `pkg/util/core.go`：`Decode` 改**单趟解析**——栈上定长表（`maxDecodeCols=64`）按列号直接记 value 切片，
   位图判存在，省掉中间 map 与二次查找；字段数/列号超界回退 `decodeLegacy`（原实现），
   未知字段名/重复字段/空行等语义与旧实现逐字节一致。
2. `db/trainkv/db.go`：`getRowKey` 从 `fmt.Sprintf` 改为直接拼 `[]byte`（每 op 省一次字符串分配）。
3. `pkg/util/core_test.go`：新增随机等价性测试（新旧路径逐字段对比；覆盖 nil/空行/重复/未知字段/
   超界列号/fieldcount=100 回退路径）。

### 8.3 实测（workloadc，100k×1KB 数据，sync_writes=false，交替 A/B，3M ops/轮）

| threads | baseline ops/s | 优化后 ops/s | Δ | CPU/op | p99 |
|---|---|---|---|---|---|
| 1 | 130.1k / 124.5k | 158.8k / 145.7k | **+19.6%** | 11.3µs → 9.4µs (-17%) | 35 → 30µs |
| 4 | 238.3/267.9/250.2/227.1k（中位 244.0k） | 293.2/257.9/206.8/269.9k（中位 263.9k） | +8.2%（方差大，见注） | 10.9µs → 9.6µs (-12%) | 166~196 → 168~177µs |
| 16 | 277.1k / 257.6k | 325.7k / 305.6k | **+18.0%** | 10.8µs → 9.0µs (-17%) | 1546/1518 → 1311/1346µs |

- CPU/op = (user+sys)/墙钟·总 op 数，是比吞吐更稳的口径：三档一致下降 12~17%，与 profile 预测的
  "省掉 11.5% CPU" 吻合。
- 注：T4 四轮方差达 206k~293k（±20%），方向不完全一致；T1/T16 每轮交替方向一致。
- allocs（同口径 per-op）：**3.66KB/op → 2.78KB/op（-24%）**；`DecodeRow` 的 5.80GB 归零，
  `SafeCopy` 5.11GB 保留（它现在是 map 的 backing buffer）。
- CPU 占比：`RowCodec.Decode` cum **20.6% → 14.3%**（其中 DecodeRow 子项 11.5% 完全消失）。

### 8.4 验证

- `go test -race -count=1 ./pkg/util/` 通过（含新增等价性测试）；
- scan 路径冒烟：workloade 3000 ops（SCAN 2843 次）正常；
- 共享适配器冒烟：badger load/run 20000 ops 正常（仅出现已知的 keyrange off-by-one READ_ERROR）；
- 二进制 provenance：`=> ../TrainKV (devel)`。

基线/新二进制与分析材料：`/tmp/prof-workloadc/go-ycsb-base`（基线）、
`ab-go-ycsb{,-base}-t{1,4,16}-r{1..4}.log`、`new-cpu.pprof`、`new-allocs.pprof`。

**结论**：这一步是"每 op 固定成本"的削减（T1/T16 +18~20%，CPU/op -17%，分配量 -24%）；
下一层瓶颈仍是 §7.3 的 startMark 管线（T4 以上的扩展性/尾延迟）。

---

## 9. 优化落地 ③：markCh 容量 100 → 8192（2026-09-12）

### 9.1 动机

§7 的 block/mutex profile 显示 `startTs`（每 op 取 tsLock + 锁内 `startMark.Begin` channel 发送）是最大阻塞点。
先做"零语义改动"的容量实验：`TrainKV/utils/limiterMark.go:68` `make(chan mark, 100)` → `8192`
（每个 mark 48B，约 400KB）。两个二进制的常量已用 `go tool objdump` 核实（`$0x64` vs `$0x2000`）。

### 9.2 实测（workloadc，3M ops/轮，两轮交替，无 profiling）

| threads | cap100 | cap8192 | Δ | CPU/op | p99 |
|---|---|---|---|---|---|
| 1 | 147.3k / 161.7k（中位 154.5k） | 168.1k / 171.4k（中位 169.8k） | +9.9% | 9.6µs → 8.8µs | 35/29 → 29/27µs |
| 4 | 311.8k / 301.9k（中位 306.9k） | 412.5k / 363.6k（中位 388.1k） | **+26.5%** | 9.0µs → 8.4µs | 155/170 → **24/67µs** |
| 16 | 328.1k / 332.8k（中位 330.5k） | 415.6k / 472.0k（中位 443.8k） | **+34.3%** | 8.7µs → 8.4µs | 1298/1276 → **513/43µs** |

- 用核数（吞吐 × CPU/op）：T16 从 **2.87 核 → 3.73 核**（4 核里少浪费了 ~0.9 核）——容量调大后，
  原本耗在"等待"上的 CPU 被用到了真实工作上。
- 累计效果（vs §7 优化前的 baseline 中位）：T1 127.3k → 169.8k（**+33%**）、
  T4 244.0k → 388.1k（**+59%**）、T16 267.4k → 443.8k（**+66%**）。
- 开 profiling 的口径也一致：224.5k（15M 场）→ 332.7k（10M 场），约 +48%（窗口不同，仅作参考）。

### 9.3 机制确认（block profile 对比，按每 op 归一）

| 指标 | cap100 | cap8192 |
|---|---|---|
| `chansend1` 占总阻塞延迟 | 0.78%（2.95s/377s） | **0.0023%**（1.26ms/55.6s） |
| `Mutex.Lock` 占总阻塞延迟 | 85.0% | 95.4% |
| `selectgo`（processOn 空等） | 14.2% | 4.6% |
| `startTs` 的 mutex 等待/op（窗口粗算） | ~55µs | **~34µs** |
| goroutine 瞬时快照卡在 `Mutex.Lock` | 14/36 | 15/33（仍排队，但 chansend 0 人） |
| 发送阻塞/op | ~0.5µs | ~0.001µs |

**修正 §7 的一个低估**：block profile 把"持有者在 send 上被挂起"的时间记在 `chansend1`（只占 0.78%），
而由此引发的**等待者**延迟全部记在 `Mutex.Lock`（85%）——所以看占比会严重低估"锁内阻塞"的破坏力。
真正决定性的是本次实验：消除锁内发送阻塞后，**每 op mutex 等待降 ~40%，吞吐 +26~34%，p99 降 3~30 倍**。

结论：容量不是"只值 10%"，它是这条路径上当前最大的单点收益，**保留 8192**。
残余的 95.4% 阻塞延迟是纯粹的锁获取排队（16 线程挤 4 核的唤醒/调度延迟），对应下一步：
按 ts 引用计数（纯读稳态 0 消息/op）+ 把临界区缩到原子操作，再评估 tsLock 本身能否从读路径移除。

race 测试：`go test -race -count=1 .`（TrainKV 根包）通过。
材料：`/tmp/prof-workloadc/go-ycsb-cap100`（对照二进制）、`cap8192-*.pprof`、`cap-*-t*-r*.log`。

---

## 10. 优化落地 ④：tsLock 读写锁实验 + 修掉两个 AtomicMark 死锁（2026-09-12）

### 10.1 改动：`tsLock` 改 `sync.RWMutex`

`TrainKV/txn.go`：纯读 `startTs()` 走 `RLock/RUnlock`，写事务 `startTs()`、`incrementNextTs()`、
`newCommitTs()` 仍走 `Lock/Unlock`。顺序性依赖"Begin 的 channel 发送仍在锁内完成"（锁释放前
任何提交都无法推进 nextTxnTs，FIFO 的 markCh 不会出现越过未登记 ts 的水位推进）。
二进制已用 `objdump` 核实：读路径为内联 `LOCK XADDL`(readerCount) + `rUnlockSlow`。

### 10.2 纯读 A/B（干净数据集，交替两轮，3M ops/轮）

| threads | Mutex（cap8192） | RWMutex | Δ | p99 |
|---|---|---|---|---|
| 1 | 165.9k / 166.0k | 166.9k / 166.3k | ~0% | 29µs → 28µs |
| 4 | 374.4k / 378.8k | 375.9k / 398.8k | +3% | 66~80µs → **22/23µs** |
| 16 | 424.3k / 425.1k | 455.2k / 437.1k | **+5%** | 87~506µs → **21/21µs** |

纯读下读写锁的收益主要在**尾延迟**（p99 从数百 µs~1.3ms 压到 21µs，跨轮稳定），吞吐小幅 +5%。

### 10.3 混合负载 A/B（workloada，2M ops/轮，4 组含交换顺序控制）

| threads | 组 | Mutex | RWMutex | Δ |
|---|---|---|---|---|
| 4 | r1/r2（Mutex 先跑） | 106.5k / 106.2k | 103.3k / 98.5k | RWMutex -3~-7% |
| 4 | r3/r4（RWMutex 先跑） | 102.7k / 108.5k | 105.7k / 100.3k | 方向不一致 |
| 16 | r1/r2（Mutex 先跑） | 135.8k / 132.4k | 124.1k / 126.5k | RWMutex -4~-9% |
| 16 | r3/r4（RWMutex 先跑） | 137.7k / 136.0k | 123.4k / 125.7k | RWMutex -8~-10% |

**T16 混合负载上 Mutex 稳定快 6~10%**（交换顺序后依旧），原因：混合负载下提交频繁，写锁与读锁
模式切换 + 写优先会让新读者排队，开销高于普通 Mutex。

**结论/权衡**：RWMutex 是"纯读更优、混合更差"的交换。若基准以 workloadc 为准可保留；
若以混合负载（更接近真实）为准，建议这一处回退成 Mutex，纯读的收益改由"按 ts 引用计数"方案拿
（它不引入读写锁的模式切换成本）。改动隔离在 `txn.go` 的 startTs，回退为一行。

### 10.4 期间发现并修复：AtomicMark 两个水位死锁（这才是本轮最重要的产出）

跑混合负载时进程卡死：4 个 worker 全部阻塞在 `commitMark.WaitForIndexDone`、CPU 0%、写管线空转。
goroutine 现场显示大家等同一个永不到达的水位（`index=104332`）。定位到 B1 的 `AtomicMark` 两个竞态：

1. **压堆后不再吸收**（`Done` 慢路径）：若 `Done(x)` 读到 doneIndex 在快路径 CAS 之前、
   而 push 发生在快路径 absorb 之后（快路径当时看到堆为空），x 永远留在 pending 堆里，
   水位停住且不会再有 Done 来推动 → 全库死等。
   压力测试 20000 轮**第 16 轮即复现**（`doneIndex=10, want 16`）。
2. **漏唤醒**（`Done` 快路径）：`absorb()` 可能把水位从 x 连推到 x+n，但 `wakeWaiters(x)`
   只用吸收前的旧水位 → 落在 (x, x+n] 的等待者永远不被唤醒；负载跑到 operationcount 末尾时
   没有新 Done 补救 → Count 冻结（线上两次都停在 ~99%）。

修复：慢路径 push 后补一次 `absorb()+wakeWaiters(GetDoneIndex())`；快路径 wake 改用吸收后的最终水位。
新增两个回归测试（`utils/atomicMark_test.go`），修复前后分别失败/通过，`-race` 全绿。

**为什么以前没暴露**：早期所有混合负载测试受 `-p threads=N` 静默失效影响，实际是单线程跑，
同一时刻只有一个提交在调 Done，天然无 CAS/push 竞态。修正线程参数后（多 worker 并发提交）立刻踩中。

修复后混合负载 8 轮（2M ops/轮，T4/T16）**全部正常跑完**，吞吐 T16 123~138k、T4 98~108k。
材料：`go-ycsb-rw-fixed2` / `go-ycsb-mutex-fixed2`（对照）、`mix3-*`/`mix4-*` 日志、`stall-goroutine.txt`。

### 10.5 附带观察

混合负载把数据集写脏后（数百万次 update 的版本 + vlog 增长），纯读 T16 从 ~44 万降到 ~28 万 ops/s。
后续纯读基准务必先重载干净数据集（`load ... -p dropdata=true`）。

---

## 11. 优化落地 ⑤：写锁请求减半（startTs 统一走读锁，2026-09-12）

### 11.1 改动与依据

`TrainKV/txn.go`：`startTs(update)` 不再按 `update` 分支取锁——**所有事务发号统一用 `RLock`**。
依据：startTs 只做"读 nextTxnTs + Begin 登记"，不改共享状态；唯一递增 nextTxnTs 的
`newCommitTs()`/`incrementNextTs()` 仍走写锁，配合"Begin 在锁内发送"的 FIFO 顺序性，语义不变。
效果：每个 update 的写锁请求从 2 次（startTs + newCommitTs）降为 1 次。

验证：`objdump` 确认 startTs 只剩内联 `LOCK XADD`(RLock/RUnlock) + `rUnlockSlow`、无 `RWMutex.Lock`；
`newCommitTs` 仍为 `Lock/Unlock`；`go test -race -count=1 . ./utils/` 通过。

### 11.2 混合负载 A/B（workloada，2M ops，T16，**每轮先重载数据集**，交替顺序）

| 对比 | 轮次 | ops/s | voluntary ctx switches |
|---|---|---|---|
| rw-full vs rw-halved | 3 对 | 124.1/128.1、128.3/123.5、126.4/131.7（千） | full ~90.5k → halved **~71.4k（-20%）** |
| Mutex vs rw-halved | 2 对 | 131.0/123.5、128.0/132.6（千） | Mutex ~58.7k，rw-halved ~73.7k |

- **上下文切换**：Mutex ~57~60k ＜ rw-halved ~70~75k ＜ rw-full ~90~91k（每轮都复现，稳定单调）。
- **吞吐**：三种变体在混合负载下互差 ≤5~6% 且方向随轮次翻转 → **落在噪声内**。
  ⚠️ 修正 §10.3 的结论：之前"混合负载 Mutex 快 6~10%"来自"不重载数据集、逐轮累积写脏"造成的漂移；
  改为每轮重载等状态后，差距消失。`tsLock` 改动对混合吞吐的实际影响是"无可测量差异"。

### 11.3 纯读回归（workloadc，2M ops，T16，同场交替）

| 轮次 | rw-full | rw-halved |
|---|---|---|
| i1（含冷启动） | 372.1k / p99 27µs | 450.0k / p99 26µs |
| i2 | 408.6k / p99 28µs | 415.3k / p99 22µs |

纯读无退化（i2 同口径 +1.6%，尾延迟持平略优），保留 RWMutex 的收益（p99 21~28µs vs Mutex 版 87~506µs）。

### 11.4 结论

**保留 rw-halved**：写锁请求减半使上下文切换再降 ~20%（相对 rw-full），混合负载吞吐无可测损失，
纯读尾延迟收益完整保留。残余的 ~73k 次切换仍高于 Mutex 的 ~58k，对应下一步"按 ts 引用计数"方案
（取消每 op 的锁内 Begin/唤醒，纯读稳态 0 消息）。

材料：`/tmp/prof-workloadc/go-ycsb-rw-full`、`half-*.log/.time`、`half2-*.log/.time`、`halfread-*.log`。

---

## 12. 优化落地 ⑥：startMark 按 ts 引用计数（2026-09-12）

### 12.1 设计

把"每个事务各发一条 Begin/Done"聚合为"每个 ts 一对"：

- 新增 `tsCounter{ts, refs atomic.Int64}`；`curCnt` 指向当前发号 ts 的计数器，`retired` 存放已退役但仍有活跃事务的 ts；
- `startTs()`：inc 计数，**仅 0→1** 时在锁内 `startMark.Begin(ts)`；
- `doneStart()`：dec 计数，**仅 1→0** 时 `startMark.Done(ts)`（若该 ts 已退役，顺手从 retired 删除）；
- `newCommitTs()` 在 `nextTxnTs++` 后调用 `retireStartTs(新ts)`：旧计数若仍有持有者则移入 retired，由最后一个结束者补发 Done。

正确性依据：事务只会发到"当前 ts"上（持读锁期间 `nextTxnTs`/`curCnt` 是一致快照，写路径在同一把写锁内成对更新，
不一致即 panic 快速失败）；Begin 仍在锁内发送，FIFO 顺序性不破；watermark 只在计数归零时才可能前进，
"水位不越过活跃读者"的约束由计数本身保证。纯读场景所有事务共享同一 ts → **稳态 0 条水位消息**。

实现过程中修掉一个自伤：热路径上误用了 `CondPanicf(cond, fmt, args ...any)`——变参装箱逃逸，每 op 一次堆分配；
改为 `if 不变量被破坏 { panic(fmt.Sprintf(...)) }`（仅异常路径构造消息）。

回归测试：`txn_refcount_test.go`（同一 ts 共享计数、退役清理、水位推进）+ `go test -race -count=1 . ./utils/` 全绿。

### 12.2 纯读 A/B（workloadc，2M ops/轮，交替两轮）

| 线程 | rw-halved | refcount | Δ | ctx switches |
|---|---|---|---|---|
| 16 | 424.8k / 451.2k | **492.7k / 476.1k** | +16.0% / +5.5%（均值 **+10.6%**） | 12.8k~16.8k → **6.9k~8.1k** |
| 4 | 403.6k / 431.0k | **444.2k / 452.2k** | +10.1% / +4.9%（均值 **+7.4%**） | 22.0k~23.8k → 21.3k~22.9k |

p99 也略有改善（25µs → 19~22µs）。累计效果（vs §7 前的原始 baseline）：T16 267.4k → ~484k（**+81%**）、
T4 244.0k → ~448k（**+84%**）。

### 12.3 混合负载 A/B（workloada，2M ops，T16，每轮重载，交替顺序，3 轮）

| 轮次 | rw-halved | refcount | Δ |
|---|---|---|---|
| 1 | 138.4k | 128.9k | -6.8% |
| 2 | 122.6k | 126.8k | +3.4% |
| 3 | 131.7k | 129.6k | -1.6% |

均值 -1.8%、方向随轮次翻转 → **打平（噪声内）**。注意修正前（`CondPanicf` 每 op 分配未修）混合是 -5~9%，
说明那个自伤主要伤在写路径（commit 每 op 一次 `doneStart`）。

### 12.4 结论

**保留**：纯读 +7~10%、上下文切换近乎腰斩，混合无可测差异；每 op 的共享同步从
"读锁 + channel 发送"降为"读锁 + 一次原子加减"。残余开销是 `tsLock` 的 RLock/RUnlock 两个原子
（readerCount 缓存行），如需进一步可评估"原子读 ts + 失效校验"的无锁发号，但收益空间已明显收窄。

材料：`/tmp/prof-workloadc/go-ycsb-rw-halved`（对照）、`ref2-read-*.log/.time`、`ref2-mix-*.log/.time`。
