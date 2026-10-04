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
- 内存只保留最近 `HistoryBlocks` 个快照；更早的块高不再可查并被剔除。

## 乱序与重复

- 块内按 `(TokenID, Index)` 去重；同一块重复投递（相同 Hash）整体忽略。
- 父块未知的块进入 `pendingBlocks` 缓冲，祖先到达后按序级联激活。
- 散事件可经 `PushEvent` 先到，按 `(Block, Token, Index)` 缓冲，待对应
  `PushBlock` 时合流去重；同一事件绝不被计入两次。
- 迟到/重复的低块高事件不会改变更高块高已发布的快照。
- 已跌出保留窗口（见下）的块与散事件不再缓冲：重复投递已裁剪的旧块会
  收到 `ErrReorgTooDeep`（见“分叉重组”），状态不受影响；低于窗口的散
  事件直接丢弃。缓冲中的块/事件随窗口前移被清理，内存不随链长增长。

## 分叉重组

- 所有收到的块都在一棵以 Hash 索引的块树中按“克隆父状态 + 应用本块”派生。
- 采用最长链规则：分叉分支只有在高度严格超过当前 canonical tip 时才被接受。
- **支持的回滚边界**：分叉点（共同祖先）高度必须 `>= 当前 tip 高度 -
  HistoryBlocks`，即回滚深度不超过 `HistoryBlocks`；深度恰好等于
  `HistoryBlocks` 时仍可成功。该边界就是服务声明的全部边界，不存在更小
  的隐藏边界：完整状态镜像（checkpoint）永远写在保留窗口下沿
  （`tip - HistoryBlocks`）或更低处，因此窗口内的回滚永远不会被镜像阻挡。
- 重组时先落盘后改内存：新分支块追加进 WAL，随后原子切换 `head.json`
  （唯一提交点），最后才回滚内存快照并沿新分支逐块重新发布。由于新分支
  节点本就由祖先状态克隆派生，结果与“从该祖先开始全新重放”一致（由
  `RebuildFromScratch` + `VerifyAgainstRebuild` 校验）。
- **拒绝语义**：回滚目标超出保留窗口时，`PushBlock` 返回可用
  `errors.Is(err, ErrReorgTooDeep)` 判别的错误，说明无法回滚的原因。
  拒绝是原子的：不修改任何已发布快照，不写入指向新分支的落盘数据，链头
  保持重组前状态，后续摄入与查询不受影响。已被拒绝的分叉根的后代块同样
  返回该错误，而不是被静默缓冲。注意：保留窗口以下、父块未知的块无法与
  深分叉区分（重复投递的旧块同样会收到该错误，调用方可安全忽略）。
- 分叉分支的块可以任意顺序到达（包括中间块晚于尾部块）：父块未知的块被
  缓冲，祖先到达后级联连接，最终重组结果与按序到达完全一致。

## 增量维护与全量校验

- 摄入路径只做增量更新；`RebuildFromScratch(blocks, cfg)` 提供从创世全量
  重放的参考实现；`Engine.VerifyAgainstRebuild` 逐项比较每个仍可查块高的
  快照，返回首个不一致高度。

## 崩溃恢复模型

持久化目录（`Config.DataDir`）包含：

- `blocks.log`：canonical 块事件的只追加 WAL。崩溃恢复时沿 `head.json`
  的父链挑选 canonical 块；不可达记录（未提交的追加、被废弃的旧分支）
  被忽略并由后台压缩清除。
- `head.json`：原子（临时文件 + rename）写入的 canonical 链头，是所有
  状态变更的唯一提交点。
- `checkpoint.json`：完整状态镜像（全部代币余额、分层、窗口），每
  `HistoryBlocks` 个块在当时的保留窗口下沿高度写入一次。

提交协议：任何变更都遵循“先写数据、后移动 head 指针”。普通块先追加
WAL 再更新 `head.json`；重组先把新分支全部追加进 WAL，再一次原子切换
`head.json`，最后清理旧分支记录；镜像先写 `checkpoint.json`，再压缩
WAL。因此任意时刻崩溃，恢复结果要么是变更前的完整旧链，要么是变更后的
完整新链，绝不出现新旧分支混合。

恢复顺序：先载入最近的镜像，再沿 `head.json` 的父链重放 WAL 中镜像高度
  之后的块，并校验链的连续性与锚定关系（损坏的持久化状态会明确报错而
不是恢复出混合状态）。WAL 末尾的半行（torn write）被丢弃。因为镜像写在
保留窗口下沿，恢复后可查询的窗口 `[checkpoint, head]` 必然覆盖崩溃前的
`[head - HistoryBlocks, head]`：崩溃前可查的每个已发布快照在重启后仍然
可查且逐块一致，包括重组刚完成或进行到一半时崩溃的情况。

磁盘增长模型：WAL 只保留镜像高度之后、且从链头可达的块，稳态上界为
`2 * HistoryBlocks` 个块（两次镜像之间的追加加上一个窗口）；镜像每
`HistoryBlocks` 个块覆盖一次，大小与代币/持有者规模成正比；崩溃遗留的
临时文件与不可达记录在下次启动或压缩时清除。

## 有界内存

- 快照 map 与块树按 `HistoryBlocks` 滚动淘汰，运行时间再长也不随链长度增长。
- 滚动窗口只保留仍在任一窗口内的每块贡献，过期即淘汰。
- 余额分层计数随余额变更增量维护，不保留全量历史。
- 乱序缓冲（`pendingBlocks`、散事件）随窗口前移清理窗口以下的条目；
  其占用与对端乱序投递的跨度成正比，不随链长增长。

## 保留深度（HistoryBlocks）的影响

- 可查询的历史快照范围：最近 `HistoryBlocks` 个已发布块。
- 可回滚的重组深度：分叉点不得浅于 `tip - HistoryBlocks`（含边界）。
- 镜像频率与磁盘上界：每 `HistoryBlocks` 块写一次镜像；WAL 上界
  `2 * HistoryBlocks` 块。
- 取小值（如 1、2、3）时以上语义全部不变，仅窗口更小、镜像更频繁；
  该场景由 `TestSmallHistoryBoundary` 等测试覆盖。

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

覆盖：基础指标、乱序与重复、散事件合流、分叉重组及重组后延伸、崩溃恢复、
重组后恢复、周期检查点恢复、滚动窗口淘汰、有界内存、并发查询与并发重组
一致性（race 检测）、增量结果与全量重放的逐项一致性，以及：

- 已写完整状态镜像后、回滚目标仍在保留窗口内的重组成功与重启逐块一致；
- 回滚目标超出保留窗口时的 `ErrReorgTooDeep` 拒绝与状态不变；
- 重组完成后立即重启，按块高逐一比对全部历史快照；
- 在每个落盘阶段（WAL 追加、head 切换、镜像写入、WAL 压缩、重组各阶段）
  注入崩溃并逐一验证恢复一致性（`TestCrashAtEveryDurableStage` 等）；
- 分叉分支乱序/重复到达与按序到达的结果一致性；
- 保留深度取 1/2/3 且重组深度恰好等于保留深度的边界组合；
- 长分支重组跨越多个检查点时的崩溃恢复。
