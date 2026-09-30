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

## 分叉重组

- 所有收到的块都在一棵以 Hash 索引的块树中按“克隆父状态 + 应用本块”派生。
- 采用最长链规则：分叉分支只有在高度严格超过当前 canonical tip 时才被接受。
- 重组时回滚内存与磁盘到共同祖先，沿新分支逐块重放并重新发布快照；
  由于新分支节点本就由祖先状态克隆派生，结果与“从该祖先开始全新重放”一致
  （由 `RebuildFromScratch` + `VerifyAgainstRebuild` 校验）。

## 增量维护与全量校验

- 摄入路径只做增量更新；`RebuildFromScratch(blocks, cfg)` 提供从创世全量
  重放的参考实现；`Engine.VerifyAgainstRebuild` 逐项比较每个仍可查块高的
  快照，返回首个不一致高度。

## 崩溃恢复模型

持久化目录（`Config.DataDir`）包含：

- `blocks.log`：canonical 块事件的只追加 WAL；重组时按高度截断重写。
- `head.json`：原子（临时文件 + rename）写入的 canonical 链头。
- `checkpoint.json`：周期写入的完整状态镜像（全部代币余额、分层、窗口）。

恢复顺序：先载入最近的检查点镜像重建 tip 状态，再重放其后的 WAL 块。
WAL 末尾若有崩溃导致的半行（torn write）会被丢弃；已提交块由 `head.json`
的父链锚定，未提交内容不会被当成 canonical，因此不会重复应用或丢失事件。

当 tip 领先上一检查点超过 `HistoryBlocks` 时写入新检查点并清空 WAL，
使磁盘占用也是有界的；恢复点不早于最新检查点，因此不支持回滚到检查点
高度之前的深层分叉（这些块已经被最终化）。

## 有界内存

- 快照 map 与块树按 `HistoryBlocks` 滚动淘汰，运行时间再长也不随链长度增长。
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

覆盖：基础指标、乱序与重复、散事件合流、分叉重组及重组后延伸、崩溃恢复、
重组后恢复、周期检查点恢复、滚动窗口淘汰、有界内存、并发查询一致性
（race 检测）以及增量结果与全量重放的逐项一致性。
