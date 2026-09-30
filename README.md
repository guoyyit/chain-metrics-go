# chain-metrics-go

本地链上代币指标计算与查询服务。输入是区块链转账事件流（普通转账、发行 `mint`、销毁 `burn`），
服务在本地增量维护每个代币的流通供应、活跃地址、转账量与持仓集中度等指标，支持按块高查询
历史快照，并在事件**乱序、重复、分叉重组、崩溃重启**下保持结果正确。

## 包结构

| 包 | 职责 |
| --- | --- |
| `domain` | 事件 / 区块 / 快照类型、指标配置 `Config`、哨兵错误 |
| `metrics` | 每代币增量状态、余额分层、平方和、top-K 持仓 Treap、滚动窗口、checkpoint 序列化 |
| `store` | 区块内容寻址落盘、快照 JSONL 归档、head 指针、checkpoint、reorg 意图（原子写 + fsync） |
| `engine` | 摄入去重、乱序缓冲、分叉回滚/重放、快照发布环、历史查询 |
| `rebuild` | 全量重放参考实现与增量结果校验 |
| `service` | 对外门面：打开即恢复、摄入、查询、校验 |
| `cmd/metricsd` | 最小可运行入口 |

## 指标语义

每个 `TokenSnapshot` 在某个已发布块上对单个代币给出：

- `CirculatingSupply`：流通供应 = Σmint − Σburn。
- `TotalMinted` / `TotalBurned`：累计发行量 / 销毁量。
- `TransferCount` / `TotalTransferVolume`：普通转账笔数与总量（mint/burn 不计入转账量）。
- `WindowTransferVolume` / `WindowTransferCount` / `ActiveAddresses`：
  最近 `ActiveWindowBlocks` 个块（含当前块）的普通转账量、笔数，以及在该窗口内
  发生过转账的去重地址数。窗口按块滚动推进，块 `h` 的窗口为 `[h-W+1, h]`。
- `BalanceTiers`：余额分层计数。层边界 `BalanceTierBounds` 为升序的“含下界”，
  余额 `b` 归入满足 `b >= bound` 的最后一层；零余额地址不属于任何层。
- `TopHolders`：按余额降序（并列按地址升序）的前 `TopHolderCount` 名持仓者。
- `TopHolderShare`：top-K 持仓者余额之和 / 流通供应。
- `HerfindahlIndex`：所有持仓者份额平方和 Σ(balanceᵢ / supply)²，取值 0..1，
  越大越集中。内部以精确大整数维护 Σbalance²，用 53 位精度求比值。

金额一律用 `math/big.Int` 精确保存（序列化为十进制字符串），不存在浮点累计误差。

## 数据模型与去重

- 事件由 `(Token, TxHash, LogIndex)` 唯一标识（`Event.Key()`）。同一区块内禁止重复键。
- 区块以内容寻址文件 `blocks/<height>/<hash>.json` 持久化；同哈希重复写入是空操作，
  因此**同一事件不可能被计入两次**。
- 区块必须带 `Height/Hash/ParentHash`；mint 的 `From` 与 burn 的 `To` 为零地址，
  普通转账两侧均非零，金额非负。

## 摄入、乱序与分叉

- **乱序**：高于 `head+1` 的块进入内存缓冲；当其父块成为规范块时自动按高度连出。
  一个分叉分支只有在整条链（到当前链上的共同祖先）都已到齐时才会被采纳，
  未连出的尖块安静等待，不推进 head。
- **重复**：同哈希块再次投递直接忽略（先查分片路径，O(1)）。
- **分叉重组**：出现与当前块同高度但不同哈希、且分支完整的竞争块时：
  1. 沿父链定位共同祖先（区块不删除，祖先可以是任意历史高度）；
  2. 对当前状态逐块执行 `Rollback`（`Apply` 的精确逆运算）直到祖先；
  3. 沿新分支逐块重放并生成新快照；
  4. 原子重写快照归档（祖先及之前保留，祖先之后换成新分支）；
  5. 原子发布新 head 指针。

  重组后的状态与“从祖先开始全新重放”逐字段一致——这一点由
  `metrics.TestRollback*` 与 `rebuild.Verify` 保证。

## 可见性与一致性

- 每个块发布一个 `BlockSnapshot`，内含该块上**所有代币**的完整切片；
  读者要么看到上一个完整块，要么看到新的完整块，不会看到跨块拼凑的指标。
- 摄入在写锁内串行提交，查询持读锁并返回**深拷贝**，二者可安全并发
  （`-race` 下有覆盖测试）。
- 提交顺序（每块的发布边界）：
  1. 区块文件 fsync；
  2. 快照行 append + fsync；
  3. `head.json` 原子替换（tmp + fsync + rename + 目录 fsync）。

  head 指针是唯一“提交记录”：只有它推进后该块才算已发布。
- 查询语义：`height=0` 表示最新块；`height > head` 返回 `ErrHeightNotYetPublished`；
  低于保留地板返回 `ErrHeightPruned`；未知代币返回 `ErrNotFound`。
  近期高度由有界内存环服务，更早但仍保留的高度流式扫描 `snapshots.jsonl`，
  查询**不触发任何全量重算**。

## 崩溃恢复模型

- 打开服务时 `Recover()`：读取 `head.json`；优先加载位于规范链上的最近
  `checkpoint.json`，再重放其后的规范块；没有 checkpoint 时从创世全量重放。
- 由于 head 只在快照行落盘后推进，重启不会对已发布块重复计事件，也不会丢失已提交事件。
- 重组是崩溃安全的：切换前先持久化 `reorg.tmp` 意图；若在 head 提交前崩溃，
  重开时先恢复到旧 head，再幂等完成该切换（归档重写与 head 提交都是原子 rename）。
- checkpoint 每 `CheckpointInterval` 个块生成一次，内含余额、分层、平方和与
  滚动窗口条目，因此恢复重放长度有界；checkpoint 损坏或不在当前链上时自动回退到全量重放。

## 有界内存

- 余额表只保留非零地址，地址归零立即从余额表、分层统计和持仓索引中删除。
- 持仓集中度用 Treap 精确保序（`(余额 desc, 地址 asc)`），每次余额变化
  期望 O(log N)，内存 O(活跃持仓者数)，与链长度无关。
- 滚动窗口每代币仅保留最近 W 个块的聚合，推进即淘汰。
- 历史快照环容量为 `HistoryRetainBlocks`；磁盘归档按批量（保留窗口的 1/4）
  惰性物理裁剪，稳态摄入是 append-only。区块本身始终保留，供任意深度重组与全量重放。

## 可配置项（`domain.Config`）

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `ActiveWindowBlocks` | 64 | 活跃/转账量滚动窗口大小（含当前块） |
| `TopHolderCount` | 10 | top-K 持仓排名与集中度口径 |
| `BalanceTierBounds` | 1、1e3、1e5、1e7、1e9 | 升序的余额分层含下界 |
| `HistoryRetainBlocks` | 10000 | 快照历史保留块数（内存环 + 磁盘裁剪） |
| `CheckpointInterval` | 256 | 崩溃恢复 checkpoint 间隔 |
| `DataDir` | `./metrics-data` | 本地状态目录 |

## 全量重建与校验

`rebuild.FullReplay` 从落盘区块按父链选规范链、在全新状态上重放到指定尖块；
`service.VerifyIncremental()` 用它对比当前增量快照。两者结果必须逐字段一致。

## 运行

```sh
go run ./cmd/metricsd --data-dir ./metrics-data --window-blocks 64 \
  --top-holders 10 --history-blocks 10000
```

## 测试

```sh
go test ./...            # 功能测试
go test ./... -race      # 含摄入/查询并发竞态检测
```

覆盖场景：指标基础语义、乱序与重复、分叉重组（含尖块先到）、回滚即精确逆运算、
崩溃恢复（有/无 checkpoint、重组中途崩溃）、并发查询一致性、窗口淘汰、
分层随余额迁移、集中度、有界历史裁剪与零余额淘汰、全量重建校验。
