# adapter.initialize() 深度解析

> 一笔交易背后发生的一切：市场开启的完整链上流程。

---

## 概述

`UmaCtfAdapter.initialize()` 是开启预测市场的**唯一入口**。调用方只需提供问题描述和参数，合约内部会自动串联
ConditionalTokens和OptimisticOracleV2的所有前置操作，全部在**一笔交易**中原子完成。

```
调用方
  └─ adapter.initialize(ancillaryData, rewardToken, reward, proposalBond, liveness)
        ├─ 1. 生成 questionId，写入链上状态
        ├─ 2. ctf.prepareCondition(...)        → ConditionalTokens
        └─ 3. _requestPrice(questionId)
                  ├─ oo.requestPrice(...)       → OptimisticOracleV2
                  ├─ oo.setBond(...)
                  ├─ oo.setCustomLiveness(...)
                  └─ oo.setRefundOnDispute(...)
```

---

## 函数签名

```solidity
function initialize(
    bytes memory ancillaryData,   // 问题描述（UTF-8），例如 "Will BNB exceed $1000 by 2025-12-31?"
    address rewardToken,          // 奖励代币（必须在 collateralWhitelist 中，通常是 USDC）
    uint256 reward,               // 成功解析后给 proposer 的奖励金额（可为 0）
    uint256 proposalBond,         // 提案者需要锁定的 bond（USDC，防止恶意提案）
    uint64 liveness              // 质疑窗口（秒），0 表示使用默认 7200 秒
) external returns (bytes32 questionId)
```

Go调用示例（本项目 `shared.go`）：

```go
receipt := Send(client, deployerAuth, adapterContract, "initialize",
    []byte("q: Will BNB price exceed $1000 by 2025-12-31? ..."),
    usdcAddr,          // rewardToken
    big.NewInt(0),     // reward = 0（不设奖励）
    ToUsdc(100), // proposalBond = 100 USDC
    uint64(120), // liveness = 120 秒（测试用，生产建议 7200）
)
```

---

## 内部执行流程

### 第 1 步：参数校验与状态写入

```solidity
questionId = keccak256(ancillaryData);          // 问题唯一 ID
require(!_isInitialized(questionId), "...");    // 不能重复初始化

questions[questionId] = QuestionData({
    requestTime : block.timestamp,  // ← 关键：记录本次初始化的区块时间戳
    proposalBond : proposalBond,
    liveness : liveness == 0 ? DEFAULT_LIVENESS : liveness,
    resolved : false,
    // ...
});
```

**关键点**：`requestTime = block.timestamp`（initialize区块的时间戳）。后续所有OO操作（`proposePrice`、`disputePrice`、
`settle`）都以这个时间戳作为key定位price request，**必须使用相同值**。

---

### 第 2 步：CTF.prepareCondition()

```solidity
ctf.prepareCondition(
    address(this),   // oracle = adapter 本身（唯一有权 reportPayouts 的地址）
    questionId,      // 问题 ID
    2                // outcomeSlotCount = 2（YES 和 NO 两个结果）
);
```

`prepareCondition` 在ConditionalTokens合约中创建一个**条件**，后续的YES/NO代币都基于这个条件铸造。

完成后，可以通过以下计算链得到ERC1155 tokenId：

```
conditionId  = keccak256(abi.encode(adapter地址, questionId, 2))
collectionId = CTF.getCollectionId(bytes32(0), conditionId, indexSet)
                   indexSet: YES=1（0b01），NO=2（0b10）
tokenId      = CTF.getPositionId(usdcAddr, collectionId)
```

这两个tokenId是`CTFExchange.matchOrders()`订单中的`tokenId`字段，也是ERC1155`balanceOf()`的查询key。

---

### 第 3 步：_requestPrice()（向OOv2发起价格请求）

内部私有函数，依次调用OOv2的四个方法：

#### 3a. requestPrice — 发起价格请求

```solidity
optimisticOracle.requestPrice(
    "YES_OR_NO_QUERY",   // identifier：Polymarket 标准标识符
    q.requestTime,       // timestamp：即 initialize 区块时间戳
    q.ancillaryData,     // 问题描述，作为 key 的一部分
    q.rewardToken,       // 计价代币
    q.reward             // 奖励金额
);
```

OOv2以 `(requester, identifier, timestamp, ancillaryData)` 四元组唯一标识一条price request。**这四个值在后续每次调用中都必须保持一致**。

#### 3b. setBond — 设置提案保证金

```solidity
optimisticOracle.setBond(..., q.proposalBond);
```

提案者提交答案时需要存入`proposalBond`数量的USDC作为押金。无人质疑且liveness结束后，押金退还给提案者；若被质疑且最终判定提案错误，押金没收。

#### 3c. setCustomLiveness — 设置质疑窗口

```solidity
optimisticOracle.setCustomLiveness(..., q.liveness);
```

覆盖OOv2的默认liveness，使用initialize时指定的值（本项目测试使用120秒，生产建议7200秒）。

#### 3d. setRefundOnDispute — 设置质疑时退还 reward

```solidity
optimisticOracle.setRefundOnDispute(...);
```

发生质疑时，OOv2会将reward退还给requester（adapter本身），防止reward被锁死在OOv2合约中。

---

## 交易完成后可读取的数据

`initialize`交易成功后，通过receipt和链上查询可以拿到后续所有步骤需要的数据：

| 数据            | 来源                                         | 用途               |
|---------------|--------------------------------------------|------------------|
| `questionId`  | `QuestionInitialized`事件（indexed[0]）        | adapter所有操作的主key |
| `requestTime` | initialize区块的 `block.timestamp`            | OOv2所有操作必须带此时间戳  |
| `conditionId` | `adapter.getConditionId(questionId)`       | CTF操作、计算tokenId  |
| `yesTokenId`  | `CTF.getPositionId(usdc, yesCollectionId)` | 订单签名、余额查询        |
| `noTokenId`   | `CTF.getPositionId(usdc, noCollectionId)`  | 订单签名、余额查询        |

---

## 初始化后的后续步骤

```
adapter.initialize()    ← 本文重点，市场开启
        ↓
CTFExchange.registerToken(yesTokenId, noTokenId, conditionId)
        ↓               （必须在 matchOrders 之前调用）
CTF.splitPosition(...)  ← 用户将 USDC 锁入 CTF，获得 YES/NO 代币
        ↓
CTFExchange.matchOrders(...)  ← 链下签名 + 链上撮合，代币在用户间流转
        ↓
OO.proposePrice(...)    ← 提案者提交结果（YES=1e18 / NO=0 / TIE=0.5e18）
        ↓
（等待 liveness，无质疑）
        ↓
OO.settle(...)          ← 锁定结果，释放 bond
        ↓
adapter.resolve(...)    ← 从 OO 取价格，调用 CTF.reportPayouts()
        ↓
CTF.redeemPositions(...) ← 用户用胜出代币换回 USDC
```

---

## 争议时的特殊行为

质疑发生时，OOv2会回调 `adapter.priceDisputed()`：

```solidity
function priceDisputed(...) external {
    q.reset = true;
    q.requestTime = block.timestamp;  // ← requestTime更新为T2（质疑区块时间）
    _requestPrice(questionId);        // ← 重新发起完整的OOv2请求
}
```

**这意味着争议后`requestTime`发生了变化（T1 → T2）**，之后的所有操作（第二次 `proposePrice`、`settle`、`resolve`）必须使用新的
T2，而不是原始的T1。需要通过 `adapter.getQuestion(questionId).requestTime` 读取最新值。

---

## 总结

| 操作                   | 调用合约               | 调用方         |
|----------------------|--------------------|-------------|
| `prepareCondition`   | ConditionalTokens  | adapter（内部） |
| `requestPrice`       | OptimisticOracleV2 | adapter（内部） |
| `setBond`            | OptimisticOracleV2 | adapter（内部） |
| `setCustomLiveness`  | OptimisticOracleV2 | adapter（内部） |
| `setRefundOnDispute` | OptimisticOracleV2 | adapter（内部） |

以上5个链上操作全部由`adapter.initialize()`在一笔交易中完成，调用方无需关心内部细节，只需提供问题描述和参数即可开启市场。
