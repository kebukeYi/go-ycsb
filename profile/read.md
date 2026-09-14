
cpu.prof: 记录的是程序在一段时间内，各个函数消耗 CPU 时间的比例;

allocs.prof：记录程序运行期间所有内存分配的采样，包括已经被 GC 回收的对象;
它回答的是“谁在疯狂分配内存”,优化分配、降低 GC 压力,alloc_space, alloc_objects;

heap.prof：只记录当前仍然存活在堆上的对象的分配采样。它回答的是“谁占着内存不释放？”
排查内存泄漏、内存大户, inuse_space, inuse_objects;

goroutine.pprof  抓取那一刻，进程里所有 goroutine 的调用栈快照;

block.prof (Lock)：告诉你谁在等锁，反映的是等待者的困境;
mutex.prof (Unlock)：告诉你谁持锁太久，反映的是持有者的责任;

----

8080: cpu.prof     model.safeCopy()
8082: allocs.prof  model.safeCopy() 7.35 GB
8081: block.prof   TransactionManager.startTs()
8083: goroutine.pprof  TransactionManager.startTs()
8086: mutex.prof  TransactionManager.startTs()
8084: 
8085: heap.prof   skl/NewArena 64MB

这说明 trainkv 读取路径存在严重的锁竞争瓶颈;

---








