# chain-metrics-go

本地链上代币指标计算与查询服务。输入是区块链的转账事件流（普通转账、
发行 Mint、销毁 Burn），服务增量维护每个代币的流通指标，支持按块高查询
一致的历史快照，并在乱序、重复、分叉重组、崩溃重启下保持指标正确。

## 数据模型

- `Event`：`{TokenID, Type(Transfer/Mint/Burn), From, To, Amount, Block, Index}`。
  事件在块内由 `Index` 唯一确定链上规范顺序；`(Block, Index)` 是事件身份。
  `From`/`To` 为空表示零地址（Mint 的来源、Burn 的去向）。
- `Block`：`{Number, Hash, ParentHash, Events}`，通过 `ParentHash` 串成链。

## 指标语义

每个代币在每个已发布块维护一个不可变 `TokenMetrics`：

- `CirculatingSupply`：流通供应 = 累计 Mint − 累计 Burn。
- `HolderCount`：余额严格大于 0 的去重地址数。
- `TransferCount`：该代币累计事件数（含 Mint/Burn/Transfer）。
- `ActiveAddresses`：活跃地址窗口内，参与过任意价值变动（含 Mint 接收方、
  Burn 发起方、转账双方，零地址除外）的去重地址数。
- `TransferVolume`：转账量窗口内，仅 `Transfer` 类型事件的金额之和。
- `TierCounts`：余额分层桶计数，随每次余额变动实时更新（离开旧桶、进入新桶）。
- `TopHoldings`：持仓 Top-N，按余额降序、地址升序打破并列。

两个滚动窗口大小（`ActiveWindowBlocks`、`VolumeWindowBlocks`）与分层定义
（`Tiers`）、`TopN` 均通过 `MetricConfig` 可配置。窗口语义为“截止当前块、
最近 N 个已发布块”（含当前块）。

余额不会变为负数：同一块内事件严格按 `Index` 顺序应用；若出现资金不足的
转账，发送方余额被钳制为 0（防御畸形输入），状态不会被污染。

## 可见性规则（查询一致性）

- 块只有成为 canonical 链的 tip 时才“发布”，发布动作一次性安装整个
  `Snapshot{BlockNumber, BlockHash, Tokens}`。
- 引擎用单个 `sync.RWMutex` 串行化摄入与查询：查询拿到的是快照值的深拷贝，
  因此任何时刻返回的快照都对应某个已发布块，不会出现部分指标属于不同块。
- 查询不触发任何重算，只读已发布快照，复杂度与链长度无关。
- 内存保留 `[head-HistoryBlocks, head]` 共 `HistoryBlocks+1` 个快照；更早块高
  不再可查。刚完成深层重组、链头继续前进时，分叉点快照可能短暂落在滚动窗口
  之下一个高度，该快照由状态信封内嵌状态继续提供查询，直到链头再次推进。

## 乱序与重复

- 块内按 `(TokenID, Index)` 去重；同一块重复投递（相同 Hash）整体忽略。
- 父块未知的块进入 `pendingBlocks` 缓冲，祖先到达后按序级联激活。
- 散事件可经 `PushEvent` 先到，按 `(Block, Token, Index)` 缓冲，待对应
  `PushBlock` 时合流去重；同一事件绝不被计入两次。
- 迟到/重复的低块高事件不会改变更高块高已发布的快照。

## 分叉重组与回滚边界

- 所有收到的块都在一棵以 Hash 索引的块树中按“克隆父状态 + 应用本块”派生。
- 采用最长链规则：分叉分支只有在高度严格超过当前 canonical tip 时才被接受；
  等高分叉保留但不切换。
- **回滚深度精确边界**：设 `R = Config.HistoryBlocks`，引擎声明支持的最大重组
  深度为 `R`，即可回滚到的最老共同祖先高度为 `head - R`（链头不足 `R` 时从
  创世起算）。分叉点恰好等于该高度（重组深度恰好 R）时重组**必须成功**；
  分叉点严格低于该高度时**明确拒绝**。该边界不存在文档未声明的更小值。
- 拒绝通过可区分的哨兵错误返回，调用方可用 `errors.Is` 判断：
  - `ErrReorgTooDeep`：分叉点低于 `head - HistoryBlocks`，超出回滚范围。错误
    信息附带具体的分叉高度、最老支持高度与当前链头。
  - `ErrNoCommonAncestor`：新块无法连接到任何保留的祖先。
  拒绝是原子的：不改变链头、不改变任何已发布快照、不写入任何指向新分支的
  落盘数据；拒绝后继续摄入合法块与查询不受影响，重启后依然停留在原链。
- 接受的重组按崩溃安全协议执行（见下），结果与从共同祖先开始的全新重放
  逐块一致（由 `RebuildFromScratch` + `VerifyAgainstRebuild` 校验）。
- 分叉分支的块允许任意顺序到达，包括“尾部块先于中间块”。父块未知的块进入
  有界孤儿缓冲，中间块到齐后级联构建并一次性切换；重复投递（相同 Hash）与
  块内重复事件（相同 `(TokenID, Index)`）幂等。晚到的低块高事件不会改变更高
  块高的已发布快照。

## 增量维护与全量校验

- 摄入路径只做增量更新；`RebuildFromScratch(blocks, cfg)` 提供从创世全量
  重放的参考实现；`Engine.VerifyAgainstRebuild` 逐项比较每个仍可查块高的
  快照，返回首个不一致高度。

## 崩溃恢复模型

持久化目录（`Config.DataDir`）包含：

- `blocks.log`：canonical 块事件的 WAL，追加后 fsync；线性推进与重组都通过
  “临时文件 + fsync + 原子 rename + 目录 fsync”重写。
- `head.json`：原子写入的 canonical 链头（最后一个**已提交**块）。
- `checkpoint.json`：状态信封（state image envelope），包含
  - 边界块 `head - HistoryBlocks` 的完整代币状态镜像（余额、分层、窗口）；
  - 上一镜像到该边界之间的 canonical 块，且每块都带各自的完整状态镜像。
    这样即使某个块的父块已被最终化（深层重组的分叉点即如此），恢复时也能
    直接发布它的快照而无需重放父块。

每个“发布”动作都严格按 **先落盘、后改内存** 的顺序，因此任意时刻崩溃后重启
都恢复到一个完整一致的链状态，与不间断运行到某个已提交块的结果逐块相同：

- 线性推进：WAL 追加(fsync) → `head.json` 翻转 → 内存发布。
- 重组：① 新分支块追加到 WAL（head 仍指旧 tip）→ ② 原子重写 WAL 删除分叉点
  以上的旧分支块 → ③ 原子翻转 `head.json` 到新 tip → ④ 内存一次性切换快照
  → ⑤ 在新 tip 下推进状态信封并压缩 WAL。任一步骤崩溃：
  - ③ 之前崩溃：重启恢复为旧链（新 WAL 记录未被 head 锚定，视为未提交）；
  - ③ 之后崩溃：重启沿新 WAL + 信封恢复为新链。

恢复时先载入状态信封（边界镜像 + 内嵌块直接发布快照），再沿 `head.json` 的
父链重放其后的 WAL 块。回溯遇到缺失父链的记录段（崩溃残留的未提交/torn 尾部）
会丢弃并把 head 重新锚定到最后一个完整连接的块，因此**启动永远不会失败**，
不会出现新旧分支混合或已发布快照丢失。WAL 末尾的半行同样被丢弃。启动时还会
回收 WAL 中的未提交/死分支垃圾。

状态信封在 `head - HistoryBlocks` 处推进：信封切换先于 WAL 压缩，两个崩溃点
都留下自洽状态。深层重组的分叉点会作为额外内嵌块进入新信封，因此“分叉点早于
最近一次完整状态镜像”的重组在回滚目标仍处于保留范围时始终成功，且重启一致。

## 回滚范围与拒绝语义（二选一，行为明确）

引擎选择“在范围内完整支持、超出范围明确拒绝”：

- 回滚目标高度 `>= head - HistoryBlocks`：完整回滚到该祖先并沿新分支重放，
  结果与从该祖先起的全新重放一致，重启后保持，期间写过完整状态镜像也不影响。
- 回滚目标高度 `< head - HistoryBlocks`：返回 `ErrReorgTooDeep`（可用
  `errors.Is(err, chainmetrics.ErrReorgTooDeep)` 区分），链头、可查询快照、落盘
  数据全部保持重组前状态；之后正常摄入与查询不受影响。
- 仅当回滚目标确实超出范围时才拒绝；不会静默忽略新分支。

## 有界内存与有界落盘

- 内存快照保留 `[head-HistoryBlocks, head]` 共 `HistoryBlocks+1` 个高度；
  块树保留该范围内的 canonical 祖先及同高度分叉兄弟节点，孤儿缓冲与散事件
  缓冲同样以回滚边界为界，运行时间再长内存占用也不随链长度增长。
- 状态信封在 `head - HistoryBlocks` 处推进：信封内嵌至多 `HistoryBlocks` 个
  canonical 块（各带完整状态），WAL 保留信封以上至多 `HistoryBlocks+1` 条记录。
  因此落盘数据量的增长模型为 **1 份完整状态镜像 + O(HistoryBlocks) 个块**，
  与链总长度无关；`HistoryBlocks` 取远小于链长度的值或恰好等于重组深度时，
  上述行为全部成立。
- 滚动窗口只保留仍在任一窗口内的每块贡献，过期即淘汰。
- 余额分层计数随余额变更增量维护，不保留全量历史。

## 快速使用

```go
e, _ := chainmetrics.NewEngine(chainmetrics.Config{
    DataDir:       "./data", // 空串表示纯内存、不持久化
    HistoryBlocks: 128,
    Metrics:       chainmetrics.DefaultMetricConfig(),
})
defer e.Close()

_ = e.PushBlock(chainmetrics.Block{
    Number: 1, Hash: "h1",
    Events: []chainmetrics.Event{
        {TokenID: "TKN", Type: chainmetrics.Mint, To: "alice",
            Amount: big.NewInt(1000), Block: 1, Index: 0},
    },
})
snap, ok := e.QueryLatest()
```

## 测试

```sh
go test -race ./...
```

覆盖：

- 基础指标、滚动窗口淘汰、乱序块与散事件合流、重复投递/块内重复幂等；
- 浅层分叉重组及重组后继续延伸；
- **分叉点早于最近一次完整状态镜像、但回滚目标仍在保留范围内**的深层重组
  成功，且重启后按块高逐一比对历史快照与从创世全量重放一致；
- 回滚目标超出保留范围时返回 `ErrReorgTooDeep`，链头/快照/落盘均不变，重启
  后一致，拒绝后继续摄入与查询正常；
- 重组完成后立即重启、逐块高比对历史快照；
- 重组各阶段（WAL 追加、WAL 重写、head 翻转、信封写入、压缩之后）崩溃注入
  后的重启一致性（恢复为旧链或新链之一，且与不间断运行结果一致）；
- 分叉分支乱序（尾部先于中间）与交错到达，结果与按序到达一致；
- `HistoryBlocks` 取 1/2/3/5 等小值时，边界两侧（恰好等于 R 成功、深一块
  拒绝）的组合及重启一致性；
- 多次重组 + 乱序后，增量结果与从创世全量重放的**逐块、逐项数值**一致性；
- 并发重组与查询：任意查询都返回某个完整已发布块的快照，无新旧混合/空结果；
- 有界内存与有界落盘（WAL 与信封大小不随链总长度增长）。
