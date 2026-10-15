# 实战案例：用差分 CPU profile + block/mutex profile 分析 workloada 读吞吐衰减

- 日期：2026-09-10
- 场景：TrainKV 纯读（workloadc）163k ops/s ↔ 混合（workloada 50R/50U）读 25.6k ops/s，**读吞吐衰减 84%**
- 目标：练习「block profile + 差分 CPU profile」工具链，定位衰减来源
- 前置：go-ycsb 已加开关 `-p debug.blockprofile=true`（默认关闭）：开启后 `runtime.SetBlockProfileRate(1)` + `SetMutexProfileFraction(1)`，`/debug/pprof/block`、`/mutex` 才有数据
- 采集数据：`/tmp/practice/{c,a,c50k}.{cpu,block,mutex,heap}`（可随时用文末命令重放）

---

## 1. 实验设置

```bash
# 数据：10 万行 1KB（引擎默认阈值 32B → 数据走 vlog），同步写关闭
./bin/go-ycsb load trainkv -P workloads/workloada -p recordcount=100000 -p threads=16 -p trainkv.dir=/tmp/tk-practice

# ① 纯读基线（full speed）
./bin/go-ycsb run trainkv -P workloads/workloadc -p operationcount=8000000 -p threads=16 \
  -p trainkv.dir=/tmp/tk-practice -p debug.pprof=localhost:6070 -p debug.blockprofile=true &
curl -s "localhost:6070/debug/pprof/profile?seconds=20" -o c.cpu
curl -s "localhost:6070/debug/pprof/block?seconds=15"   -o c.block
curl -s "localhost:6070/debug/pprof/mutex?seconds=10"   -o c.mutex

# ② 混合负载
./bin/go-ycsb run trainkv -P workloads/workloada -p operationcount=3500000 -p threads=16 \
  -p trainkv.dir=/tmp/tk-practice -p debug.pprof=localhost:6071 -p debug.blockprofile=true &
# 同样抓 a.cpu / a.block / a.mutex

# ③ 对齐吞吐的纯读（限速到与混合同量级）
./bin/go-ycsb run trainkv -P workloads/workloadc -p operationcount=2000000 -p threads=16 \
  -p target=50000 -p trainkv.dir=/tmp/tk-practice -p debug.pprof=localhost:6072 &
curl -s "localhost:6072/debug/pprof/profile?seconds=20" -o c50k.cpu
```

实测吞吐：纯读 **163k**；混合 **54k**（读 25.6k + 更新 25.6k）；限速纯读 **50k**（按 RATE 精确）。

---

## 2. block profile：先看"谁在阻塞"

```bash
go tool pprof -top -sample_index=delay ./bin/go-ycsb a.block
# → 15s 窗口累计阻塞延迟 131s，97.8% 是 runtime.selectgo

# 关键动作：看 selectgo 的调用者(不看调用者等于没看)
go tool pprof -peek='runtime.selectgo' -sample_index=delay ./bin/go-ycsb a.block
```

selectgo 调用者分解（15s 窗口）：

| 调用者 | 延迟 | 性质 |
|---|---|---|
| `lsm.StartFlushMemTable` | 28.6s | **空闲**：后台刷盘 goroutine 等任务 |
| `opencensus.view.worker` | 20.0s | **空闲**：指标采集线程（链接了 badger 带来的） |
| `client.Run.func1` | 19.9s | **空闲**：measurement 定时器 goroutine |
| `LimitMark.processOn` | 15.7s | 半空闲：mark 管线消费 goroutine 等标记 |
| `net/http/pprof.serveDeltaProfile` | 15.0s | **采样器自己**在等（剖面工具造成的） |
| `LevelsManger.getTxnDoneIndexFromCh` | 14.6s | **空闲**：compaction 等 txn 水位 |
| `TrainKV.handleWriteCh` | 11.9s | 半空闲：单写管线 goroutine 等请求 |
| `LimitMark.WaitForIndexDone` | **1.58s** | **真实业务等待**：读事务等提交水位 |
| `Request.Wait`（WaitGroup） | **2.89s** | **真实业务等待**：写事务等落批 |

> **陷阱 #1**：block profile 把"空闲 goroutine 等任务"也计入阻塞延迟，不加分辨会得出"全在阻塞"的错误结论。必须用 `-peek` 看调用者，并按"业务路径 vs 后台空闲"分类。
> **结论**：真实业务阻塞（水位等待 + 写批等待）合计 ~4.5s/15s，折算读 op ≈ 4µs/次——**读衰减主因不是"线程互相阻塞"**。

---

## 3. mutex profile：排除锁竞争

```bash
go tool pprof -top -sample_index=delay ./bin/go-ycsb a.mutex
go tool pprof -top -sample_index=delay ./bin/go-ycsb c.mutex
```

| | 总竞争延迟(10s 窗口) | 主要来源 |
|---|---|---|
| workloada | **~40ms** | `LimitMark.processOn` 唤醒、`doneCommit` |
| workloadc | ~25ms | `LimitMark.Begin/Done` 通道唤醒 |

> **陷阱 #2**：锁等待不在 block profile 里，必须单独看 mutex profile；反过来 mutex profile 也包含"唤醒者"成本（runtime.unlock）。
> **结论**：锁竞争可忽略（毫秒级）——排除 `tsLock/writeChLock/VLogFile.Lock` 争抢假设。

---

## 4. 差分 CPU profile：两次差分，一次踩坑一次正确

### 4.1 直接差分（错误示范）

```bash
go tool pprof -top -diff_base=c.cpu ./bin/go-ycsb a.cpu
# 结果: 正差异只有 skl.getKey +2.7s; 大量负数(Table.Search -3.1s, RowCodec.Decode -4.4s...)
```

> **陷阱 #3**：纯读窗口跑了 3.26M 次读，混合窗口只跑了 1.08M 次 op——**绝对差分把"工作量差异"混进了"单次成本差异"**，负数多只是说明混合模式读得少，不能说明读变便宜了。

### 4.2 对齐吞吐后再差分（正确姿势）

把纯读用 `-p target=50000` 限速到同一量级，再差：

```bash
go tool pprof -top -nodecount=22 -diff_base=c50k.cpu ./bin/go-ycsb a.cpu
```

**workloada 比限速纯读多出来的 CPU（20s 窗口）**：

| 排名 | 函数 | 差值 | 解读 |
|---|---|---|---|
| 1 | `runtime.futex` | **+3.02s** | goroutine 停/唤醒的握手成本（写通道、req.Wait 唤醒、水位等待、mark 通道） |
| 2 | `skl.(*skipNode).getKey` | **+2.69s** | 混合模式下 memtable 被持续写入，读的 memtable 查找要遍历活跃跳表 |
| 3 | `runtime.memmove` | +0.89s | 写侧编码/拷贝流量 |
| 4 | `runtime.selectgo` | +0.46s flat | 通道 select（水位等待等） |
| 5 | `cmpbody` / `Int31n`(+子) / `lock2`/`unlock2`/`casgstatus` | 各 +0.2~0.6s | 键比较、更新值生成、调度器状态切换 |
| — | `Table.Search`/`blockIterator.loadIndex`/`cacheShard.get`/`RowCodec.Decode` | 负数 | 混合模式实际执行读次数少 → 读路径总量反而低 |

> **陷阱 #4**：`target` 限速本身有开销（timer 唤醒），c50k 的每 op CPU 比全速纯读还高（13.7µs vs 8.6µs），对齐实验要把这部分污染计入误差。

---

## 5. 量化：每 op CPU（窗口总量 ÷ 操作数）

| 场景 | CPU 总量(20s 窗口) | 占比 | 窗口内操作 | 每 op CPU |
|---|---|---|---|---|
| 纯读全速 163k | 28.18s | 141% | ~3.26M 读 | **8.6µs/读** |
| 纯读限速 50k | 13.75s | 69% | ~1.0M 读 | 13.7µs/读（含限速器开销） |
| 混合 54k | 26.27s | 131% | ~1.08M op | **24.3µs/op** |

结合 §2/§3（真实阻塞 ~4µs/读、锁竞争可忽略）：混合模式**既没有 CPU 打满（1.31 核 / 4 核），也没有明显阻塞**，但吞吐只有 54k。也就是说：读衰减 = **每 op 的同步握手成本（futex/select 唤醒）× 高频 op 数** 叠加 **写通道单车道节奏牵引**（读写速率实测完全同步在 25.6k），而非某一个"卡点"。

---

## 6. 结论与下一步

1. **可确证**：读衰减不是锁竞争、不是业务线程互相阻塞、不是 CPU 饱和。
2. **可量化**：混合负载额外 CPU 主要花在 futex 唤醒（+3.0s）与活跃 memtable 查找（+2.7s）上；每 op CPU 从 8.6µs（读）升到 24.3µs（混合 op）。
3. **下一步（留给下次练习）**：用 `go tool trace` 看 **Scheduler latency**（goroutine 可运行但等 CPU 的排队时间）与阻塞时间线，验证"runnable 排队"在剩余时间中的占比；若成立，优化方向是减少每 op 的 channel 握手（mark 管线批量化、减少唤醒次数）与写通道批处理，而非动锁。

## 7. 本次学到的工具要点（清单）

1. block/mutex profile 需要显式开启采样率（`SetBlockProfileRate` / `SetMutexProfileFraction`），否则端点是空的。
2. block profile ≠ 锁竞争；锁竞争只看 mutex profile。锁等待不会出现在 block profile。
3. block profile 的"阻塞延迟"包含空闲 goroutine 等任务，必须 `-peek` 看调用者再分类，否则 97% 的数字会误导你。
4. 剖面采样器自身（pprof delta handler）、指标线程、定时器 goroutine 都会出现在 profile 里，做差分时会互相抵消，但单看时会虚高。
5. 差分 profile 前必须对齐工作量：要么限速（计入限速器开销），要么按"每 op 归一化"。
6. `-sample_index=delay`（时长）与 `contentions`（次数）是两个视角，定位优先看 delay，评估频率看 contentions。
7. 吞吐数字与 profile 结论要交叉验证：本次"无锁竞争 + 无阻塞 + CPU 未满"的组合，指向同步握手与调度层，这是下一层工具（trace）的课题。

---

## 8. 续篇（2026-09-11）：go tool trace 验证「调度排队」

承接 §5 的悬案：block/mutex/CPU 三层都排除后，剩下的嫌疑是 **goroutine 可运行但排队等 CPU 的延迟（scheduler latency）**——这层只有 `go tool trace` 看得到。

### 8.1 无浏览器环境怎么采/怎么分析（关键技巧）

```bash
# 采集: 直接走 pprof HTTP 端点(runtime trace 内置支持 seconds)
curl -s "http://localhost:6074/debug/pprof/trace?seconds=6" -o a.trace

# 非交互分析(无浏览器时用):
go tool trace -pprof=sched   a.trace > a.sched    # 调度延迟 profile → 交给 pprof 看
go tool trace -pprof=sync    a.trace > a.sync     # 同步阻塞 profile
go tool trace -pprof=syscall a.trace > a.syscall  # 系统调用阻塞 profile
go tool trace -d=footprint   a.trace              # 事件总览(教学/体检)
# 有浏览器时: go tool trace -http=:8083 a.trace → View trace / Goroutine analysis 等页面
```
采集开销与体积：混合负载 6 秒 trace = 19MB（纯读 7MB）；**trace 期间吞吐不可用**（探针开销大），只用于定性定位。

### 8.2 调度延迟 profile：结果与"归属方向"陷阱

| 场景 | 总调度延迟(6s 窗口) | 折合每 op |
|---|---|---|
| workloada 混合 54k ops/s | **5.94s** | ~18.3µs/op |
| workloadc 纯读 163k reads/s | 3.78s | ~3.9µs/read |

flat 排名：`runtime.chansend1` 占 **94~99%**。注意 sched profile 的归属方向是**"把别人唤醒的那个调用栈"**（不是等待者自己的栈）——顺着 cum 看到：

- `Transaction.commitAndSendToDB → newCommitTs` 3.06s（写事务提交时 `commitMark.Begin`/`startMark.Done` 的 markCh 发送）
- `Transaction.Discard` 2.17s（读事务结束时 `startMark.Done`）
- `NewTransaction → startTs` 0.21s（开启时 `startMark.Begin`）

即：**所有"排队"都指向同一件事——每 op 往 `LimitMark.markCh` 发 2~4 个标记，唤醒单线程 `processOn` 管线；而 processOn 在混合负载下 98% 的时间处于"可运行但等 CPU"状态**（纯读下也有 63%）。生产者把 processOn 反复推入运行队列，它却长期抢不到 P——系统为此付出每 op 18µs 的调度延迟。

### 8.3 交叉验证

- **trace-sync profile**：6s 内累计阻塞 ~35s，`selectgo` 23.5s + `chanrecv1` 10.9s 为主（包含 handleWriteCh 的 5.1s）——与 block profile 结论一致：大多是"等任务的空闲等待"，业务等待占比小。
- **trace-syscall profile**：仅 39ms（pprof HTTP 本身）——**热路径上没有任何系统调用阻塞**，与 sync=false（写入走缓冲）一致；如果开了 sync_writes，这个 profile 里会立刻出现成片的 fsync。
- **footprint（6 秒窗口事件量）**：
  - `GoUnblock` 902,952 / `GoBlock` 902,957 → **~15 万次/秒的阻塞-唤醒切换**（54k ops/s，约 2.8 次/op）；
  - `GoStart` 1,059,108 → 17.6 万次/秒的 goroutine 上 CPU；
  - `GoCreate` 150,769 → **2.5 万个/秒的新 goroutine 创建**（后续已归因，见第 9 节：99.996% 来自水位通知 `go func`，与 `go writeRequest` 无关）。

### 8.4 trace 练习的结论

1. §5 的"调度排队"假设**成立且已量化**：混合负载每 op 18.3µs 的调度延迟（纯读 3.9µs），全部归因于 mark 管线的 channel 唤醒。
2. 读吞吐衰减的完整链条至此清楚：**每 op 2~4 次 markCh 发送 → 唤醒风暴（150k 次/秒 block/unblock + 17.6 万次/秒上 CPU）→ processOn 单管线长期饥饿、系统调度开销高企 → 读写 op 一起被拖慢，读写速率被牵到同一水平（25.6k）**。它是"高频同步握手成本"，不是锁、不是阻塞、不是磁盘。
3. 优化方向（优先级排序）：
   - **批量化/去通道化 mark 管线**：Begin/Done 先在本地累积，定期或按需刷入；或改用原子水位（atomic done-index）+ 条件唤醒，消除 processOn 单点与 15 万次/秒唤醒；
   - **减少不必要唤醒**：读事务的 `startMark.Begin/Done` 在多数场景只用于版本回收边界，可评估合并/延迟提交；
   - **写通道批处理去 goroutine 化**：`handleWriteCh` 每批 `go writeRequest(...)` 的 2.5 万/s 协程创建可改为 worker 池。

   > **修正（见第 9 节实测归因）**：该分支在实测负载下 6 秒触发 **0** 次，优先级下调；2.5 万/s 的协程创建 99.996% 来自 `limiterMark.go:173` 的水位通知 `go func`——它才是要改的目标（并入 ① 的"去杂项"）。
4. 工具经验：trace 的 sched profile 归属方向易误读（看 waker 栈）；无浏览器用 `-pprof=`/`-d=footprint` 照样完成分析；trace 自身开销大，只做定性定位，量化仍回归 block/mutex/CPU + 吞吐口径。

---

## 9. 补充（2026-09-11）：goroutine 创建归因 —— 修正优化项 ③

**动机**：footprint 显示混合负载有 2.5 万个/秒的 goroutine 创建，需要确认来源（`db.go:270` 的 `go writeRequest` 还是 `limiterMark.go:173` 的水位通知 `go func`）。

**对照实验**：
| trace 场景 | 6 秒内 GoCreate |
|---|---|
| workloadc 纯读 | **16** |
| workloada 混合 | **156,983**（26.2k/s） |

**无浏览器下的归因方法**：runtime trace 的 parsed 输出里，goroutine 创建表现为 `StateTransition ... NotExist->Runnable` 事件，其后紧跟创建者调用栈；用段模式 awk 按创建者栈分类：

```bash
go tool trace -d=parsed a2.trace | awk -v RS="" '
  /NotExist->Runnable/ { n++;
    if ($0 ~ /limiterMark/) lim++;
    if ($0 ~ /writeRequest/) wr++;
  }
  END { printf "total=%d limiterMark=%d writeRequest=%d\n", n, lim, wr }'
```

**结果**：`total=156983  limiterMark=156977 (99.996%)  writeRequest=0`。

**解读**：
1. goroutine 创建几乎全部来自 `limiterMark.go:173`——水位每次推进（≈每笔提交一次）就 `go func(){ doneIndexCh <- curIndex }` 通知 compactor；
2. `handleWriteCh → go writeRequest(reqs)` **触发 0 次**：收集协程"攒批→内联写盘→释放令牌"交替执行，执行 select 抢令牌时令牌总是空闲的，dispatch 分支需要"上一批还在写时又攒好一批"这个重叠场景，单收集协程结构下几乎不发生（该分支接近死代码）；
3. 纯读几乎零创建的原因：没有写入 → `nextTxnTs` 不变 → 读事务 startTs 恒定、水位推进一次即停 → 通知协程不再产生。

**结论（修正优化清单）**：
- ③「handleWriteCh 每批起协程改 worker 池」的前提不成立（分支未触发），优先级下调为"代码清理"级别；
- 真正要改的是水位通知：`doneIndexCh` 改为容量 1 + 只保留最新值（合并通知），或由 processOn 直接做非阻塞发送（`select ... default`），彻底消除 26k/s 的 spawn —— 归入 ① 的"去杂项"。

---

## 10. 方案 3 落地记录（2026-09-11）：水位通知共享原子化

**改动（TrainKV，均已按方案 3 的最终形态实现）**：

| 文件 | 改动 |
|---|---|
| `utils/limiterMark.go` | `Init/processOn` 参数 `chan uint64` → `*atomic.Uint64`；水位推进时**直接 Store 到共享原子**，删除 `go func(){ doneIndexCh <- curIndex }` 通知协程 |
| `lsm/option.go` | `TxnDoneIndexCh chan uint64` → `TxnDoneIndex *atomic.Uint64` |
| `lsm/levelsManger.go` | 删除 `txnDoneIndex` 本地镜像字段、`stopCh/stopWG`、`getTxnDoneIndexFromCh()` 消费协程；`getDiscardTs()` 改为**按需读共享原子** |
| `db.go` | 新增 `txnDoneIndex atomic.Uint64` 字段；`Opt.TxnDoneIndex = &db.txnDoneIndex`（替代建通道） |
| `txn.go` | `startMark.Init(tm.closer, options.TxnDoneIndex)` |
| `lsm/compact_test.go` / `last_level_compact_test.go` | 测试改用共享测试原子 `testTxnDoneIndex` |

净效果：**一条通道 + 一个消费协程 + 每次水位推进一个通知协程全部消失**；水位由生产者直接发布，compactor 按需读取（还消除了原先的通知滞后）。

**正确性验证**：`go test -race -count=1 .` 通过（40.7s）；`go vet` 通过；load + workloada/b/c/e 冒烟全部正常、零失败。

**性能验证（同一 trace 口径，workloada，6 秒窗口）**：

| 指标 | 改前 (a2) | 改后 (a4) | 变化 |
|---|---|---|---|
| **GoCreate** | 156,983 | **6** | **-100%** |
| GoStart | 1,102,841 | 882,961 | **-20%** |
| GoUnblock / GoBlock | 940,513 / 940,516 | 877,816 / 877,819 | -6.7% |
| 混合吞吐 ops/s | 54,085 | **61,230** | **+13.2%** |
| 读 ops/s | 27,023 | **30,632** | **+13.4%** |
| 写 ops/s | 27,140 | 30,610 | +12.8% |
| sched 总量 / 每 op | 5,935ms / ~18.3µs | 6,130ms / **~16.7µs** | 每 op **-9%** |

解读：spawn 路径清零后，少掉的是 26k/s 的"创建-调度-销毁"环节，吞吐直接 +13%；sched 总量因吞吐变高而绝对值略增，但**每 op 调度延迟下降 9%**。剩余 sched 延迟的 96% 仍是 `chansend1`（newCommitTs 累计 3.08s），即 markCh 的那 2~4 次/op 唤醒——那要等 ① 的批量化/原子化来削。

**踩坑记录（重要）**：第一次"改后"实测数据毫无变化——排查发现 `go.mod` 的 `replace => ../TrainKV` 又被环境移除了（require 被升到已发布的 v2.0.3），二进制实际链的是发布版旧代码。
- 检测方法：`go version -m bin/go-ycsb | grep -A1 kebukeYi`，必须看到 `=> ../TrainKV (devel)`；可再用 `go tool nm bin/go-ycsb | grep processOn.func1.1` 确认为 0（新符号不存在）。
- 教训：**改完引擎后先验证构建来源，再跑基准**，否则可能在测一个根本没改的二进制。

**回滚**：`cd ../TrainKV && git checkout -- <上述文件>` 即可；文件均为未提交状态。

---

## 11. 优化 ① 第一步落地（2026-09-11）：commitMark 原子化（B1，已生效）

**改动**：
- 新增 `utils/atomicMark.go`：`AtomicMark`——快路径(顺序完成)一次 CAS 推进水位，乱序完成压小顶堆(互斥锁慢路径)，等待者"锁内注册 + 双检水位"防止丢唤醒；`Begin` 仅记录 lastIndex；`SetDoneIndex` 用于启动播种。
- `txn.go`：`commitMark` 由 `LimitMark` 切换为 `AtomicMark`；closer 计数 2→1；
- `db.go`：commitMark 播种改 `SetDoneIndex`（原 `Done` 依赖堆跳越语义，原子版改为直接播种）。

**验证**：`go test -race` 通过；load + workloada/b/c/e 冒烟正常。

**trace 对比（同口径 6 秒窗口，workloada）**：

| 指标 | 基线 a2 | 方案3 a4 | **B1 a5** |
|---|---|---|---|
| GoCreate | 156,983 | 6 | 6 |
| GoUnblock / GoBlock | 940,513 / 940,516 | 877,816 / 877,819 | **503,293 / 503,297** |
| GoStart | 1,102,841 | 882,961 | **511,631** |
| 混合吞吐 ops/s | 54,085 | 61,230 | **76,155 (+40.8%)** |
| 读 ops/s | 27,023 | 30,632 | **38,043 (+40.8%)** |
| 写 ops/s | 27,140 | 30,610 | **38,123 (+40.0%)** |
| 每 op 调度延迟 | ~18.3µs | ~16.7µs | **~11.3µs** |

剩余 sched 延迟主要来自 startMark 的 `chansend1`（Begin + Done，2 次/op）。

---

## 12. 优化 ① 第二步尝试：startMark 原子化（B2）—— 回归，已回退

**设计**：`utils.SeqMark`——每个事务 `Begin()` 领全局唯一序号，`Done(seq, ts)` 上报自己的 startTs；水位对外发布为"已连续结束事务的最大 startTs"，无 channel、无通知协程。

**实测结果（回归）**：

| 场景 | B1（LimitMark startMark） | B2（SeqMark） |
|---|---|---|
| load T16 | 31.7k | 20.0k |
| load T1 | 33.2k | 20.6k (p50 4µs→28µs) |
| workloada 混合（trace 口径） | 76.2k | 40.3k |

**二分回退验证**：仅把 startMark 切回 LimitMark（B1 状态），load T1 立即恢复 33.2k → **B2 是回归来源，确认**。

**观测与初判**：B2 单线程 profile 中 `runtime.futex` 占 40% CPU（`notewakeup → startm`，约 16µs/op 的 OS 线程唤醒开销），而 SeqMark 自身函数 CPU 占比极小——更像是"把水位跟踪/发布放回事务提交热路径（且 mirror 由提交线程直接写）"改变了 goroutine 阻塞模式、触发了额外的 M 停/唤醒 churn；机制尚未完全定论。

**处置**：保留 B1，回退 B2（`utils/seqMark.go` 保留为"原子化样例"参考，未启用；`Transaction` 中相关字段已清理）。

**教训（写入方法论）**：
1. 把"异步管线"换成"同步原子结构"时，热点路径上的同步发布/共享结构可能引入 OS 级开销（线程唤醒），不是纯用户态 ns 级成本；
2. 任何此类改动先做 **单线程 vs 多线程**快速对照（几秒钟就能暴露串行化/回归）；
3. **二分回退**是定位回归最快的手段（本例一次回退即锁定）。

---

## 13. 附录：B2 样例代码存档（utils/seqMark.go，未启用，已从引擎移除）

```go
package utils

import (
	"sync"
	"sync/atomic"
)

// SeqMark 是 startMark 的原子化实现:
// 每个事务通过 Begin() 领一个全局唯一序号, 结束时 Done(seq, ts) 上报自己读的时间戳水位;
// 水位对外发布为"已连续结束的事务中最大的 startTs"(单调不回退), 供 compaction 判断
// 哪些旧版本可以安全回收。
//
// 设计要点:
//   - 与 LimitMark 按 ts 计数不同, 这里按唯一 seq 跟踪, 因此 Begin/Done 天然一一对应,
//     无需在共享 ts 上做引用计数;
//   - 快路径(顺序完成)仅一次 CAS + 两次原子 store; 乱序完成才进入互斥锁慢路径;
//   - 无 channel、无 processOn 通知协程、无 goroutine 唤醒。
type SeqMark struct {
	Name        string
	nextSeq     atomic.Uint64 // Begin 分配的序号(从 1 开始);
	doneSeq     atomic.Uint64 // 已连续结束的序号水位;
	publishedTs atomic.Uint64 // 对外发布的事务水位(已完成事务的最大 startTs);
	mirror      *atomic.Uint64

	mu          sync.Mutex
	pendingHeap u64Heap           // 乱序完成的 seq 小顶堆;
	pendingTs   map[uint64]uint64 // seq -> startTs
	heapLen     atomic.Int32
}

func (m *SeqMark) Init(mirror *atomic.Uint64) {
	m.mirror = mirror
	m.pendingTs = make(map[uint64]uint64)
}

// Begin 分配一个全局唯一序号; 纯原子自增, 无唤醒;
func (m *SeqMark) Begin() uint64 {
	return m.nextSeq.Add(1)
}

// SetDoneIndex 播种水位(启动/重放后调用, 此时无在途事务);
func (m *SeqMark) SetDoneIndex(ts uint64) {
	m.publish(ts)
}

// GetDoneIndex 返回对外水位(已完成事务的最大 startTs), 语义与 LimitMark.GetDoneIndex 一致;
func (m *SeqMark) GetDoneIndex() uint64 {
	return m.publishedTs.Load()
}

// Done 上报序号完成: 顺序完成走 CAS 快路径, 乱序完成压堆等待吸收;
func (m *SeqMark) Done(seq uint64, ts uint64) {
	for {
		d := m.doneSeq.Load()
		if seq != d+1 {
			break
		}
		if m.doneSeq.CompareAndSwap(d, seq) {
			m.publish(ts)
			m.absorb()
			return
		}
	}
	m.mu.Lock()
	m.pendingHeap.push(seq)
	m.pendingTs[seq] = ts
	m.heapLen.Store(int32(len(m.pendingHeap)))
	m.mu.Unlock()
}

// absorb 吸收堆中紧跟着连续结束的序号; 堆为空时零开销返回;
func (m *SeqMark) absorb() {
	if m.heapLen.Load() == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.pendingHeap) > 0 {
		d := m.doneSeq.Load()
		if m.pendingHeap[0] != d+1 {
			break
		}
		seq := m.pendingHeap.pop()
		m.doneSeq.Store(d + 1)
		m.publish(m.pendingTs[seq])
		delete(m.pendingTs, seq)
	}
	m.heapLen.Store(int32(len(m.pendingHeap)))
}

// publish 发布对外水位; ts 随 seq 单调不减, 因此发布值天然不回退;
func (m *SeqMark) publish(ts uint64) {
	m.publishedTs.Store(ts)
	if m.mirror != nil {
		m.mirror.Store(ts)
	}
}
```
