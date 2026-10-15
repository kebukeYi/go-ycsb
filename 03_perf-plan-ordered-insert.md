# TrainKV 顺序键持续插入变慢 —— 性能排查 & IO 优化实战手册

- 日期：2026-09-10
- 背景：纯写专项测试 W2 发现——trainkv 在 `insertorder=ordered` 下的**持续插入**吞吐 27.3k，低于 `hashed` 的 33.4k（-18%）；但 load 阶段 ordered 反而更快（33.6k vs 29.8k）。方向出现反转，需要系统排查。
- 环境：WSL2（kernel 6.18.33）、4C/8T、数据盘 `/dev/sdd`（1TB 虚拟盘，ext4）、Go 1.27。
- 工具可用性（本机已验证）：`iostat / iotop / fio / perf / strace / blktrace / btt / pidstat / vmstat` 全部可用。

> 使用方法：按章节顺序推进；§1 是必做的信号确认，之后按 §5 的"假设→实验"表逐条击破；每节的工具都附了"命令 + 输出怎么读 + 本例看什么"。

---

## 0. 分层排查模型（先建立心智模型）

```
层级	        核心关注点	                  推荐工具 |	          工具本质
应用层	    业务 QPS、写放大、操作比例	      go-ycsb, YCSB	负载生成与统计
Runtime层	Goroutine 堆积、GC、锁竞争、CPU 热点	pprof 六件套, go tool trace	语言级别的采样剖析
系统调用层	频繁 syscall、错误的 I/O 调用	  strace, ltrace	边界追踪
OS 指标层	CPU/内存/IO                    整体饱和度	vmstat, pidstat, iostat, iotop	内核计数器
块设备层	    块设备延迟分解、I/O 调度瓶颈	  blktrace + btt, bpftrace	块层事件追踪
硬件基准层	磁盘裸吞吐、IOPS 标定	          fio, hdparm	压力测试与标定
全栈性能剖析	跨层热点、硬件事件、内核调用栈	  perf, eBPF	采样与追踪
```

原则：**每层先看"占比与量级"，再往下钻**。应用层差 18% 时，先确认这 18% 是花在 CPU 计算、阻塞等待、还是真实 IO；不要一上来就抓块层。

---

## 1. 第 0 步（必做）：信号确认与稳定复现

**为什么先做这个**：本会话多次观测到 ±10~30% 的跨轮漂移（badger 同配置 c 阶段从 88k 到 43k）。27.3k vs 33.4k 的 18% 差异**可能只是噪声**——不先排除，后面所有分析都是空中楼阁。

**做法**：交替跑 hashed / ordered 各 3 轮，每轮独立 load+持续插入，取中位数。

```bash
cd /win_projects/goland_project/go-ycsb
for i in 1 2 3; do
  for order in hashed ordered; do
    RECORDCOUNT=100000 OPERATIONCOUNT=200000 THREADS=16 WORKLOADS="_insert" \
    EXTRA_A="-p trainkv.value_threshold=1048576 -p insertorder=$order" \
    EXTRA_B="-p badger.value_threshold=1048576 -p badger.compression=none -p insertorder=$order" \
    bash tool/ab_bench.sh trainkv badger 2>&1 | tee /tmp/repro-$i-$order.log | grep -E "^_insert|^load"
    rm -rf /tmp/ab-trainkv /tmp/ab-badger
  done
done
```

**判定标准**：
- 3 轮方向一致 且 ordered/hashed 中位数差异 >15% → **真信号**，继续 §2；
- 方向摇摆或差异 <10% → 是噪声，先执行"降低测量噪声"措施再复测：
  - 绑核：`taskset -c 0-3 bash tool/ab_bench.sh ...`（WSL 只有 4 核，避免调度漂移）
  - 关闭后台干扰：确认无其他 go-ycsb/GoLand 索引/Windows 侧大 IO
  - 每轮之间 `sync`，必要时 `echo 3 > /proc/sys/vm/drop_caches`（root 下可试）
  - 单轮时长拉长：`operationcount=200000` 让实测窗口 ≥5s，摊薄启动/收尾抖动

---

## 2. 每轮固定采集清单（后面所有工具都挂在这个流程上）

| 层 | 采什么 | 命令/文件 |
|---|---|---|
| 应用 | OPS、计数 | `tee` 保存 ab 脚本输出 |
| Go CPU | 30s CPU profile | `-p debug.pprof=localhost:6060` + `curl .../profile?seconds=30` |
| Go 阻塞/锁 | block/mutex profile（需先开启采样，见 §3.1） | `curl .../block`、`.../mutex` |
| Go 堆 | heap profile | `curl .../heap` |
| 系统 | 进程 IO 字节数（写放大）、Dirty 页 | `/proc/<pid>/io`、`/proc/meminfo` |
| 磁盘 | iostat / iotop 采样 | 后台 `iostat -x -d sdd 1 > iostat.log` |

一个可直接用的"边跑边采"片段：

```bash
# 启动被测进程后
PID=$(pgrep -f 'go-ycsb run trainkv')
iostat -x -d sdd 1 60 > /tmp/iostat.$(date +%s).log &
cat /proc/$PID/io > /tmp/io.before        # rchar/wchar/read_bytes/write_bytes
# ... 等运行结束 ...
sync; cat /proc/$PID/io > /tmp/io.after   # 进程退出后 /proc/<pid> 消失, 需在退出前采;
```

> 注意：`/proc/<pid>/io` 在进程退出后不可读，长跑时用 `watch -n1 'cat /proc/<pid>/io'` 落盘。

---

## 3. Go Runtime 层工具手册

### 3.1 先开启 block/mutex 采样（当前代码没开）

`/debug/pprof/block` 和 `/mutex` 端点默认**返回空**，必须先设采样率。go-ycsb 和 TrainKV 目前都没设置（已确认），两种开法：

**A. 改 go-ycsb main.go（一次改动，长期受益）**——在 `main()` 开头加：

```go
import "runtime"

// 性能排查开关: 建议用属性控制, 默认关闭避免开销
if globalProps.GetBool("debug.blockprofile", false) {
    runtime.SetBlockProfileRate(1)        // 1 = 采样每个阻塞事件; 学习用可设 1, 常规设 1e6(ns)
    runtime.SetMutexProfileFraction(1)    // 1 = 采样全部锁竞争
}
```
（属性要在 `initialGlobal` 之后读；`run`/`load` 都生效。）

**B. 用 TrainKV 自己的 go test 通道（无需改 go-ycsb）**——`go test` 的 profile 参数会自动开启采样：

```bash
cd /win_projects/goland_project/TrainKV
go test -run='^$' -bench='BenchmarkInsert' -benchtime=30s ./benchmk/ \
  -cpuprofile=/tmp/tk.cpu -memprofile=/tmp/tk.mem \
  -blockprofile=/tmp/tk.block -mutexprofile=/tmp/tk.mutex -trace=/tmp/tk.trace
```
> 若 benchmk 还没有 ordered/hashed 对照的 bench，可先加一个最小骨架（见 §3.6）。

### 3.2 pprof 六件套：抓什么、怎么看、本例关注点

| Profile | 抓取命令（进程内 HTTP） | 回答什么问题 | 本例关注点 |
|---|---|---|---|
| **CPU** | `go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30` | CPU 时间花在哪些函数 | `skl.findNear`（memtable 插入）、`compact.*`（后台合并）、`vlog.*`（写入编码）占比差异 |
| **heap** | `go tool pprof -inuse_space http://localhost:6060/debug/pprof/heap` | 当前存活对象（内存泄漏/膨胀） | memtable arena、block cache、value 缓冲 |
| **allocs** | `go tool pprof -alloc_space .../heap` | 累计分配（GC 压力来源） | 每 op 分配次数对比（ordered vs hashed 是否不同） |
| **block** | `go tool pprof http://localhost:6060/debug/pprof/block` | goroutine **阻塞**在哪（chan/mutex/WaitGroup） | `req.Wait`（写通道）、`markCh` 发送、vlog 文件锁 |
| **mutex** | `go tool pprof http://localhost:6060/debug/pprof/mutex` | 锁竞争严重程度 | `tsLock`、`writeChLock`、`VLogFile.Lock` |
| **goroutine** | `curl http://localhost:6060/debug/pprof/goroutine?debug=2` | 各 goroutine 状态分布（瞬时快照） | 写线程卡在哪个调用（我们上次就是这样抓到 `Request.Wait`） |

**判读命令**：

```bash
# 火焰图式网页 UI(推荐, 支持搜索/过滤)
go tool pprof -http=:8081 /tmp/tk.cpu
# 文本 top(flat=自身耗时, cum=含子调用)
go tool pprof -top -cum -nodecount=30 ./bin/go-ycsb /tmp/ordered.cpu
# 看某个函数由谁调用/调用了谁
go tool pprof -peek='skl.*findNear' ./bin/go-ycsb /tmp/ordered.cpu
# 看某函数内部逐行耗时(定位到代码行)
go tool pprof -list='findSpliceForLevel' ./bin/go-ycsb /tmp/ordered.cpu
```

### 3.3 杀手锏：差分对比（ordered vs hashed）

不要分别看两份 profile 猜差异，直接让 pprof 做差值：

```bash
# 先分别抓: /tmp/hashed.cpu 和 /tmp/ordered.cpu(相同 operationcount、相同跑法)
go tool pprof -diff_base=/tmp/hashed.cpu ./bin/go-ycsb /tmp/ordered.cpu
# 输出中: 正数=ordered 多花的时间(要找的嫌疑), 负数=hashed 多花的时间
(pprof) top -cum
```

同理对 heap/allocs/block/mutex 都做差分——这是定位"18% 差在哪"最直接的证据链。

### 3.4 go tool trace：调度与阻塞的时间线

```bash
go tool trace /tmp/tk.trace     # 自动打开浏览器(或 -http=:8082)
```
重点看三处：**Scheduler latency profile**（线程排队）、**Blocking profile**（阻塞时长分布）、**Goroutine analysis**（按函数统计阻塞次数/时长）。比 pprof 更擅长回答"线程时间都去哪了"（我们之前遇到的"平均延迟 6µs 但每线程节奏 134µs"就是用这类视角才看懂的）。

### 3.5 低开销运行时观测（不抓 profile 也能看）

```bash
GODEBUG=gctrace=1 ./bin/go-ycsb run ...     # GC 频率/停顿, 每行含 heap 大小与 STW 时间
curl -s localhost:6060/debug/pprof/goroutine?debug=1 | head   # 状态计数
```
以及 Go 1.16+ 的 `runtime/metrics`（如需写采集脚本可后续加）。

### 3.6 微基准骨架（TrainKV benchmk，剥离 YCSB 噪声）

```go
func BenchmarkInsertOrdered(b *testing.B) { benchInsert(b, true) }
func BenchmarkInsertHashed(b *testing.B)  { benchInsert(b, false) }

func benchInsert(b *testing.B, ordered bool) {
    dir := b.TempDir()
    db, err, _ := TrainKV.Open(lsm.GetDefaultOpt(dir))
    if err != nil { b.Fatal(err) }
    defer func() { _ = db.Close() }()

    value := make([]byte, 1024)
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        n := uint64(i)
        if !ordered { n = utils.MemHash(n) }   // 关键变量: 键序
        key := []byte(fmt.Sprintf("user%010d", n))
        txn := db.NewTransaction(true)
        _ = txn.Set(key, value)
        if _, err := txn.Commit(); err != nil { b.Fatal(err) }
    }
}
```
跑法（profile 一次到位）：见 §3.1-B 的命令。这样可以 5 秒内切换变量（ordered/hashed、阈值、批量），把"是不是 YCSB 框架噪声"先排除掉。

---

## 4. 系统 IO 层工具手册（含判读教学）

### 4.1 先认识你的磁盘与文件系统

```bash
lsblk -d -o NAME,SIZE,ROTA          # sdd=1TB 虚拟盘(ROTA=1 表示呈现为机械盘, WSL VHD 特性)
df -h /tmp                          # 数据目录在 /dev/sdd(ext4)
cat /proc/diskstats | grep sdd      # 内核累计 IO 统计(各字段含义 man 5 /proc/diskstats)
```
**学习点**：WSL2 的 `/dev/sdd` 是虚拟盘（VHDX），所有 IO 最终落到 Windows 宿主的物理盘。**绝对数字不可与裸机比较，但同机同盘的相对差异完全有效**——这正是我们做 A/B 的场景。

### 4.2 vmstat / pidstat：全局与进程视角

```bash
vmstat 1 10                 # bi/bo = 块设备读/写(kB/s); si/so=swap; r=运行队列; wa=等待IO的CPU
pidstat -d -p <pid> 1       # 进程级 kB_rd/s kB_wr/s; -u 看 CPU; -w 看上下文切换
pidstat -w -p <pid> 1       # cswch/s(主动让出, 等IO/锁) vs nvcswch/s(被抢) — 阻塞型负载看 cswch
```
**本例看点**：ordered 与 hashed 的 `cswch/s`、`kB_wr/s` 是否有系统性差异。

### 4.3 iostat -x：块设备延迟分解（核心工具）

```bash
iostat -x -d sdd 1 30
```
逐列解读（只挑关键列）：

| 列 | 含义 | 怎么看 |
|---|---|---|
| `r/s` `w/s` | 每秒读/写请求数 | 与引擎 OPS 对照，判断是否发生 IO 放大 |
| `rkB/s` `wkB/s` | 每秒读写带宽 | ×时长 ≈ 总量，与数据量对照 |
| `rareq-sz` `wareq-sz` | 平均请求大小(KB) | **LSM 顺序写应远大于 4K**；接近 4K 说明随机写 |
| `await` | 平均完成延迟(含排队, ms) | 延迟敏感场景核心指标；但受合并/调度影响 |
| `aqu-sz` | 平均队列深度 | 衡量是否打满 |
| `%util` | 设备忙比例 | **虚拟盘上会失真**（反映"有请求在飞"而非物理饱和度），WSL 下别当准绳 |
| `f/s` `f_await` | flush 请求数与延迟 | 与 fsync 相关 |

**本例看点**：sync=false 的写入经 page cache 缓冲，iostat 的 `w/s` 会**滞后于应用 OPS**（回写滞后）；若 ordered/hashed 的 `wareq-sz` 或写入总量显著不同 → 直接指向"两序的物理写放大不同"（假设 H3/H5 的证据）。

### 4.4 iotop：谁是 IO 大户（多进程对照）

```bash
iotop -oPa -d 1     # -o 只显示有IO的进程, -P 按进程聚合, -a 累计模式
```
用例：同时跑引擎进程与编译器/IDE 时快速定位干扰源；或确认回写是由 `kworker`（内核回写线程）还是引擎进程自身完成。

### 4.5 /proc/<pid>/io：每进程精确 IO 字节数 → **每 op 写放大**

```bash
PID=$(pgrep -f 'go-ycsb run trainkv')
cat /proc/$PID/io
# rchar, wchar      = 经系统调用的读写字节(含 page cache)
# read_bytes, write_bytes = 真正落到块层的字节(★ 关键)
```
**计算方法**：`(write_bytes_after - write_bytes_before) / 操作数 = 每 op 物理写入字节`。
- 例：1KB 行、内联布局，理想每 op ≈ 1KB + WAL ≈ 2KB；若实测 5KB → 写放大 2.5x（compaction/重复 flush）。
- **务必**在采样窗口内 `sync` 一次，否则 write_bytes 滞后（脏页还没回写）。
- 两序对比：如果 ordered 的"每 op 物理写入"显著高于 hashed → 根因在**后台合并量**，直接去查 compaction 调度（§5-H3）。

### 4.6 strace：系统调用视角（看懂"引擎到底在对内核说什么"）

```bash
# 1) 统计各系统调用次数/耗时占比(排查模式)
strace -f -c -e trace=write,pwrite64,fsync,fdatasync,openat,ftruncate,fallocate \
  ./bin/go-ycsb run trainkv -P workloads/workload_insert -p trainkv.dir=/tmp/tk -p operationcount=20000 2>&1 | tail -20

# 2) 看单次 fsync 的真实延迟(-T 打印每个调用耗时)
strace -f -T -e trace=fsync,fdatasync -o /tmp/fsync.trace ./bin/go-ycsb run ... 
grep fsync /tmp/fsync.trace | head -5      # 行尾 <0.006123> 即 6ms

# 3) 观察写入模式: 大块顺序 write 还是大量 4K pwrite?
strace -f -e trace=write,pwrite64 -o /tmp/io.trace ./bin/go-ycsb run ...
awk '/pwrite|write/ {print $NF}' /tmp/io.trace | sort | uniq -c | sort -rn | head
```
**注意**：`-f` 跟踪 16 线程 + 全量输出开销很大，只用于短跑（2 万 op）或过滤后使用。
**学习点**：这是"应用→文件系统"的边界；写放大、fsync 次数、O_DIRECT 与否都能在这里看见。

### 4.7 fio：磁盘能力标定（给引擎数字一个参照系）

先标定"这块盘能跑多快"，再看引擎用了多少。**统一在 /tmp 下建测试文件**（与引擎同盘）：

```bash
# A. 顺序 1M 大块写 —— 带宽上限
fio --name=seq1m --filename=/tmp/fio-test --size=4G --rw=write --bs=1M \
    --ioengine=libaio --iodepth=16 --direct=1 --runtime=30 --time_based

# B. 4K 随机写 —— IOPS 上限
fio --name=rand4k --filename=/tmp/fio-test --size=4G --rw=randwrite --bs=4k \
    --ioengine=libaio --iodepth=64 --direct=1 --runtime=30 --time_based

# C. 每次写后 fsync —— 模拟 sync=true 引擎的真实下限
fio --name=fsync4k --filename=/tmp/fio-test --size=1G --rw=write --bs=4k \
    --ioengine=psync --fsync=1 --runtime=30 --time_based

# D. 1K 小块顺序写 —— 最接近 YCSB 的 WAL/小记录模式
fio --name=seq1k --filename=/tmp/fio-test --rw=write --bs=1k --size=1G \
    --ioengine=psync --direct=1 --runtime=30 --time_based

rm -f /tmp/fio-test
```
**怎么用这些数字**（本机 WSL 预期量级：seq1m 几百 MB/s~1GB/s；rand4k 数十 k IOPS；fsync4k 几百次/s，即每 fsync ~几 ms）：
- 引擎 `sync=false` 时，OPS 应主要受 CPU/内存路径限制（几十 k/s），若 OPS 掉到 fio-C 的量级 → 说明 fsync 在热路径上；
- 引擎 `sync=true` 实测 168 ops/s ≈ fio-C 的量级 → 完全被 fsync 支配（与我们 W4 结论吻合，坐实了"异步差距才是引擎差异"）。

### 4.8 perf：CPU 与内核栈视角

```bash
# 1) 计数: CPU 周期/指令/上下文切换/缺页(先确认支持哪些事件)
perf stat -p <pid> -- sleep 10
# WSL2 同为虚拟机: cycles/instructions 等硬件事件可能显示 <not supported>, 此时用软件事件:
perf stat -e task-clock,context-switches,page-faults,cpu-migrations -p <pid> -- sleep 10

# 2) 采样: 带调用栈找热点(含内核态)
perf record -F 999 -g -p <pid> -- sleep 20
perf report --stdio --sort comm,dso,symbol | head -30
# 3) 实时 top
perf top -p <pid>
# 4) 系统调用追踪(KPtrace 风格)
perf trace -p <pid>
```
**本例看点**：perf 能看到 `futex`/`sched`/`ext4` 内核函数的占比；如果 ordered 的内核态时间显著更高（例如 `flush`/`writeback` 路径），与 iostat/strace 交叉验证。

### 4.9 blktrace + btt：块层延迟分解（进阶，本机已验证可用）

```bash
cd /tmp
blktrace -d /dev/sdd -o bt -w 30 &        # 采 30 秒(WSL 虚拟盘同样可采)
sleep 2 && cd /win_projects/goland_project/go-ycsb && ./bin/go-ycsb run ... &
wait
cd /tmp
blkparse -i bt -d bt.bin                  # 合并各 CPU trace
btt -i bt.bin                             # 输出 Q2G/G2I/I2D/D2C 四段延迟分解
```
四个阶段的教学含义：
- **Q2G**（Queue→Get）：排队时间（设备繁忙或调度）
- **G2I**（Get→Issue）：驱动准备
- **I2D**（Issue→Done）：设备服务（真实盘速）
- **D2C**（Done→Complete）：完成回调（软中断/CPU 忙）
本机是虚拟盘，绝对延迟无意义，但 ordered/hashed 两序的四段占比**相对变化**有效。裸机 linux 上这是定位"延迟到底花在排队还是设备"的标准工具。

### 4.10 页面缓存与写回：解释"sync=false 时数据何时落盘"

```bash
watch -n1 "grep -E 'Dirty|Writeback' /proc/meminfo"   # Dirty=待回写脏页, Writeback=正在回写
cat /proc/sys/vm/dirty_ratio /proc/sys/vm/dirty_background_ratio \
    /proc/sys/vm/dirty_expire_centisecs /proc/sys/vm/dirty_writeback_centisecs
```
**学习点**：`sync=false` 的引擎只保证"进了内核 page cache"，真实落盘由回写线程按上述参数异步完成。所以：
- 写吞吐测试时，若数据量 < 内存（本机 7GB，100MB 数据集完全在缓存里），**物理磁盘几乎没参与**——这就是为什么"异步写"数字漂亮；
- 压真实 IO 必须：数据量 > 内存、或开启 `sync_writes`、或跑完 `sync` 观察 Dirty 页归零耗时（`time sync`，能直观看到积压了多少）。

---

## 5. 假设 → 实验 → 预期信号（针对本次 ordered 变慢）

| # | 假设 | 实验 | 预期信号 |
|---|---|---|---|
| H1 | **纯噪声** | §1 交替 3 轮 | 方向不一致/差异 <10% → 结案 |
| H2 | 内存表插入路径差异（顺序键在 skiplist 尾部不停追加 vs 随机插入） | CPU 差分 profile；`-peek skl.*` | `skl.findNear/findSpliceForLevel` 的差值显著为正 |
| H3 | 后台 flush/compaction 工作量不同（键区间集中 vs 分散） | `/proc/pid/io` 每 op write_bytes；CPU diff 看 `compact.*`；跑完统计数据目录文件数/大小（`ls -la`, `du`，MANIFEST 层结构） | ordered 每 op 物理写明显更多/更少 |
| H4 | 锁/goroutine 交接差异 | block/mutex profile + `go tool trace` | `req.Wait`/`markCh`/`tsLock` 阻塞差值 |
| H5 | WAL 写入模式差异 | `strace -c -e write,pwrite64,fsync`；iostat | 系统调用次数/单次大小不同 |
| H6 | vlog 轮转差异（阈值 32B 时） | `ls` 数据目录 vlog 文件数；strace openat/ftruncate | 轮转频率不同 |

**额外离线证据（零成本）**：每次跑完对比数据目录快照——
```bash
ls -la /tmp/ab-trainkv | head -30; du -sh /tmp/ab-trainkv
# 对比 ordered/hashed 两次的 SST 数量、vlog 大小、文件编号增长速度
```
如果两序的数据布局产物量级不同，H3 基本就坐实了。

---

## 6. 修复方向候选（先证实假设再动手）

1. **compaction/flush 策略**（H3 命中时）：为顺序键场景调整 L0 触发阈值与层选择；LCN/range 分区；避免小键区间反复与宽区间合并。
2. **memtable 追加优化**（H2 命中时）：顺序键的 skiplist 尾插可加"尾部缓存指针"；arena 预分配。
3. **写通道批处理**：当前 `SyncGroupCommitWait` 仅在 `SyncWrites=true` 时启用；可评估异步模式下也做微批（牺牲少量延迟换吞吐）。
4. **键构造路径**：`fmt.Sprintf` 在热路径（两条路径都会走到），可换成手写格式化/缓冲复用——注意 hashed 还多一次 Hash64 却更快，说明键构造不是主因，先别动。
5. **数据布局**：vlog 与 SST 的键排序一致性（阈值 32B 布局下）。

---

## 7. 学习资料

- Brendan Gregg：*Systems Performance (2nd ed)*；USE 方法；Linux 性能工具全景图（brendangregg.com/linuxperf.html）
- Go 官方：Profiling Go Programs（blog.golang.org）；`runtime/pprof` 文档；`go tool trace` 教程
- LSM 专题：RocksDB Wiki（Write Amplification / Compaction / Leveled-Compaction）；O'Neil 等《The Log-Structured Merge-Tree》
- IO 工具：`fio` 文档（HOWTO）；`blktrace` User Guide（含 Q2G/G2I/I2D/D2C 讲解）；`man 5 /proc/diskstats`
- WSL2 特性：虚拟盘与宿主磁盘的关系、`%util`/硬件计数器失效边界

---

## 8. 两天执行清单

**Day 1（先证伪再定位）**
1. §1 交替 3 轮复现（绑定 `taskset`，单轮 20 万 op）→ 确认信号
2. 若成立：开 §3.1-A 采样开关 → 抓 ordered/hashed 的 CPU/block/mutex/allocs 四份 profile
3. §4.5 每 op 物理写入对比 + §4.6 strace 系统调用普查 + iostat 采样
4. §3.3 差分分析 → 按 §5 表格落位假设（H2/H3/H4 谁命中）

**Day 2（修复与验证）**
1. 按 §6 对应方向实现候选修复（小步，可回滚）
2. §3.6 微基准先验证，再回 go-ycsb 复测（同 §1 口径）
3. 用 §4.2–4.5 确认修复后的写放大与阻塞分布改善

---

## 9. 执行记录（2026-09-10）：排查提前关闭 —— 原差异为噪声

按 §1 执行了复现确认：trainkv 单引擎、ordered/hashed 交替各 3 轮，每轮 100k load + 200k 持续插入（1MB 内联阈值，同步写关闭）。

| 轮次 | 键序 | load (ops/s) | 持续插入 200k (ops/s) |
|---|---|---|---|
| 1 | hashed | 30321 | 33064 |
| 1 | ordered | 30553 | 32251 |
| 2 | hashed | 32932 | 33274 |
| 2 | ordered | 30371 | 33368 |
| 3 | hashed | 32032 | 29562 |
| 3 | ordered | 28656 | 32371 |

- **中位数**：load hashed 32032 / ordered 30371（-5.2%，方向与 W2 相反）；持续插入 hashed 33064 / ordered 32371（-2.1%）。
- **判定**：差异远小于 15% 阈值且方向不一致；同一顺序的不同轮次间波动即有 ±11%（hashed 插入 29562 ↔ 33274）。按 §1 规则，**H1 成立——W2 观测到的 -18% 是单轮采样噪声，不是真实退化**。
- 结论：无需进入 H2–H6 的 profiling/IO 分析；**不做任何"修复"**（避免为不存在的问题引入复杂度）。
- 方法论收获：存储基准的单轮数字不可作为结论（本会话多次出现 ±10%~30% 漂移）；用"交替顺序 + ≥3 轮 + 中位数 + 方向一致性"四件套再决定是否值得深挖。
- 若未来出现**可复现**的写路径退化（≥3 轮方向一致且差异 >15%），从 §3.1 开启采样并按 §5 假设表推进。
