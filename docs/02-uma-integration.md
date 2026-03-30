# UMA Oracle接入详解与适配层分析

> 本文档深入分析UMA OOv2/OOv3的接入方式，及Polymarket适配层的实现细节，并给出BSC测试网的接入策略。

---

## 一、UMA 是什么？与 Polymarket 的关系

**UMA（Universal Market Access）是独立的第三方去中心化预言机协议**，与Polymarket是两个完全独立的团队和项目。

- UMA有自己的治理代币（$UMA）、DVM（数据验证机制）和代币持有者投票社区。任何协议都可以接入UMA进行争议仲裁。
- Polymarket是预测市场平台，选择UMA作为价格仲裁层，但自己维护CTFExchange和UmaCtfAdapter等合约。

两者的关系类似**甲方与基础设施供应商**：Polymarket 用UMA的OOv2解决"谁提案、谁质疑、最终谁说了算"的问题，但DVM投票由UMA代币持有者完成，Polymarket无法干预仲裁结果。

这也是本项目用`MockOOv2` + `mockDvmSettle()`的原因——在测试环境中模拟这个本来由UMA社区治理的仲裁过程。

---

## 二、UMA OOv2 vs OOv3 对比（Polymarket用哪个？）

| 特性                | OOv2                       | OOv3                                                          |
|-------------------|----------------------------|---------------------------------------------------------------|
| **Polymarket 使用** | ✅ 是（UmaCtfAdapter 使用 OOv2） | ❌ 否（但更新的项目推荐）                                                 |
| **交互模式**          | 请求-应答（先 Request，再 Propose） | 主动断言（直接 assertTruth）                                          |
| **返回值类型**         | int256（价格，如 1e18 表示 YES）   | bool（true/false）                                              |
| **标识符**           | `YES_OR_NO_QUERY`          | `ASSERT_TRUTH`                                                |
| **回调**            | `priceDisputed()`          | `assertionResolvedCallback()` + `assertionDisputedCallback()` |
| **争议处理**          | 直接上 DVM                    | 可配置 EscalationManager                                         |
| **适用场景**          | 预测市场（Polymarket 验证）        | 通用断言、新项目                                                      |

**结论**：Polymarket 生产环境用的是 OOv2。但对于新项目，推荐使用 OOv3，接口更简洁，回调更完善。

---

## 三、UMA OOv2 工作流程（Polymarket 实际流程）

```
┌─────────────────────────────────────────────────────────────┐
│ Step 1: 请求价格 (由 UmaCtfAdapter.initialize() 触发)         │
│                                                             │
│ OOv2.requestPrice(                                          │
│   identifier = "YES_OR_NO_QUERY",                           │
│   timestamp  = questionStartTime,                           │
│   ancillaryData = question描述,                              │
│   currency   = USDC,                                        │
│   reward     = 0 (奖励在 proposer 中配置)                     │
│ )                                                           │
└─────────────────────┬───────────────────────────────────────┘
                      │
┌─────────────────────▼───────────────────────────────────────┐
│ Step 2: 提案 (任何 Proposer 调用，通常是 Polymarket 机器人)     │
│                                                             │
│ OOv2.proposePrice(                                          │
│   requester  = UmaCtfAdapter地址,                           │
│   identifier = "YES_OR_NO_QUERY",                           │
│   timestamp  = questionStartTime,                           │
│   ancillaryData = question描述,                             │
│   proposedPrice = 1e18 (YES) 或 0 (NO)                      │
│ )                                                           │
│ → 提案者锁定 proposalBond                                     │
│ → 进入质疑窗口（约 2 小时）                                     │
└─────────────────────┬───────────────────────────────────────┘
                      │
          ┌───────────┴───────────┐
          │ 无质疑                │ 有质疑
┌─────────▼──────────┐ ┌─────────▼──────────────────────────────┐
│ Step 3A: 解析       │ │ Step 3B: 质疑                          │
│                    │ │                                        │
│ UmaCtfAdapter      │ │ OOv2.disputePrice(...)                 │
│   .resolve(qId)    │ │ → UmaCtfAdapter.priceDisputed() 回调    │
│ → 调用              │ │   → 重置请求（reset）                    │
│   CTF.reportPayouts│ │   → 返还提案者 bond                      │
│ → 市场结算完成       │ │ → 争议进入 DVM（48-96 小时）              │
└────────────────────┘ │ → DVM 投票结果返回                       │
                       │ → 重新触发 resolve 流程                  │
                       └────────────────────────────────────────┘
```

---

## 四、UMA OOv3 工作流程（推荐新项目使用）

OOv3 更简洁，**断言者主动推送结果**，无需先请求。

```
┌─────────────────────────────────────────────────────────────┐
│ Step 1: 断言                                                 │
│                                                             │
│ OOv3.assertTruth(                                           │
│   claim     = "The market resolved YES because...",         │
│   asserter  = 断言者地址,                                     │
│   callbackRecipient = 你的合约地址,                           │
│   escalationManager = address(0),                           │
│   liveness  = 7200 (2小时),                                  │
│   currency  = USDC,                                         │
│   bond      = 1500e6 (1500 USDC),                           │
│   identifier = bytes32("ASSERT_TRUTH"),                     │
│   domainId  = bytes32(0)                                    │
│ ) returns (bytes32 assertionId)                             │
│ → 断言者 USDC 被锁定                                          │
└─────────────────────┬───────────────────────────────────────┘
                      │
          ┌───────────┴───────────┐
          │ liveness 结束，无质疑   │ 有人质疑
┌─────────▼───────────┐ ┌─────────▼──────────────────────────────┐
│ Step 2A: 结算        │ │ Step 2B: 质疑                          │
│                     │ │                                        │
│ OOv3.settleAssertion│ │ OOv3.disputeAssertion(assertionId,     │
│   (assertionId)     │ │   disputer)                            │
│ → 断言被接受          │ │ → disputer 锁定等额 bond                │
│ → bond 返还断言者     │ │ → callbackRecipient.                   │
│ → 回调:              │ │     assertionDisputedCallback(id)      │
│   assertionResolved │ │ → 争议上升到 DVM                         │
│   Callback(id,true) │ │ → DVM 投票 48-96 小时                    │
│ → 你的合约处理结果     │ │ → 结果返回，回调:                         │
└─────────────────────┘ │   assertionResolvedCallback(id, result) │
                        └─────────────────────────────────────────┘
```

### OOv3接入代码模板

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

interface IOptimisticOracleV3 {
    function assertTruth(
        bytes memory claim,
        address asserter,
        address callbackRecipient,
        address escalationManager,
        uint64 liveness,
        address currency,
        uint256 bond,
        bytes32 identifier,
        bytes32 domainId
    ) external returns (bytes32 assertionId);

    function disputeAssertion(bytes32 assertionId, address disputer) external;

    function settleAssertion(bytes32 assertionId) external;

    function getAssertionResult(bytes32 assertionId) external view returns (bool);
}

interface IOptimisticOracleV3CallbackRecipient {
    function assertionResolvedCallback(bytes32 assertionId, bool assertedTruthfully) external;

    function assertionDisputedCallback(bytes32 assertionId) external;
}

contract MyPredictionMarket is IOptimisticOracleV3CallbackRecipient {
    IOptimisticOracleV3 public immutable oracle;
    address public immutable usdc;

    mapping(bytes32 => bytes32) public assertionToMarket;

    constructor(address _oracle, address _usdc) {
        oracle = IOptimisticOracleV3(_oracle);
        usdc = _usdc;
    }

    // 提交断言（请求 Oracle 裁定结果）
    function proposeResolution(bytes32 marketId, bool result) external {
        // 从调用者拉取 bond
        IERC20(usdc).transferFrom(msg.sender, address(this), BOND_AMOUNT);
        IERC20(usdc).approve(address(oracle), BOND_AMOUNT);

        bytes memory claim = abi.encodePacked(
            "Market ", marketId, result ? " resolved YES." : " resolved NO."
        );

        bytes32 assertionId = oracle.assertTruth(
            claim,
            address(this),  // asserter = 本合约（代表提案者）
            address(this),  // callbackRecipient = 本合约
            address(0),     // 无 escalation manager
            7200,           // 2 小时
            usdc,
            BOND_AMOUNT,
            bytes32("ASSERT_TRUTH"),
            bytes32(0)
        );

        assertionToMarket[assertionId] = marketId;
    }

    // OOv3 回调：断言结算
    function assertionResolvedCallback(
        bytes32 assertionId,
        bool assertedTruthfully
    ) external override {
        require(msg.sender == address(oracle));
        bytes32 marketId = assertionToMarket[assertionId];
        // assertedTruthfully = true  → 断言成立（提案结果生效）
        // assertedTruthfully = false → 断言被推翻（结果取反）
        _finalizeMarket(marketId, assertedTruthfully);
    }

    // OOv3 回调：断言被质疑
    function assertionDisputedCallback(bytes32 assertionId) external override {
        require(msg.sender == address(oracle));
        bytes32 marketId = assertionToMarket[assertionId];
        // 市场进入争议状态，等待 DVM 仲裁
        _setMarketDisputed(marketId);
    }
}
```

---

## 五、UmaCtfAdapter核心适配逻辑解析

### 4.1 OOv2价格 → CTF Payouts转换

```solidity
function _constructPayouts(uint256 price) internal pure returns (uint256[] memory payouts) {
    payouts = new uint256[](2);
    if (price == 0) {
        // NO 赢
        payouts[0] = 0;
        payouts[1] = 1e18;
    } else if (price == 0.5e18) {
        // 平局
        payouts[0] = 0.5e18;
        payouts[1] = 0.5e18;
    } else if (price == 1e18) {
        // YES 赢
        payouts[0] = 1e18;
        payouts[1] = 0;
    } else {
        // 无效价格，全零（市场作废）
        payouts[0] = 0;
        payouts[1] = 0;
    }
}
```

### 4.2 ancillaryData（问题描述）格式

OOv2的ancillaryData是UTF-8字节，Polymarket采用以下格式：

```
"q: title: 'Will Biden win the 2024 US Presidential Election?',
 description: 'This market will resolve YES if Biden wins...',
 res_data: p1: 0, p2: 1, p3: 0.5, p4: -57896...,
 Where p1 corresponds to No, p2 to a Yes, p3 to unknown/50-50 and p4 to an early resolution"
```

### 4.3 priceDisputed 回调（质疑重置逻辑）

```
质疑发生时：
1. OO调用 UmaCtfAdapter.priceDisputed()
2. 适配器重新发起 OO requestPrice（新请求）
3. 提案者的 bond 被退回
4. 新提案者需要重新提案（更高 bond）
```

这是 Polymarket 的"**两次质疑才能上 DVM**"机制：

- 第一次质疑 → 自动reset，重新开始
- 第二次质疑 → 才真正上DVM仲裁

---

## 六、Bond经济机制

### 5.1 各方资金流向

```
正常解析（无质疑）：
  提案者 → OO（锁定 bond）
  liveness 结束后 → 提案者收回 bond + 奖励（reward）

质疑发生（提案正确）：
  质疑者 bond → 提案者（惩罚质疑者）
  奖励（reward）→ 提案者

质疑发生（提案错误）：
  提案者 bond → 质疑者（惩罚提案者）
  奖励（reward）→ 质疑者
  部分 bond → UMA Store（协议费）
```

### 5.2 Polymarket的Bond设置

- 标准市场proposalBond：约 **1,500 USDC**
- 高风险/大资金市场：更高 bond
- liveness：约 **2小时**

这确保攻击成本（1500 USDC bond + 被发现风险）大于可能的套利收益。

---

## 七、BSC测试网的UMA现状与解决方案

### 6.1 UMA部署现状

UMA 官方支持的网络：

| 网络                       | 状态        |
|--------------------------|-----------|
| Ethereum Mainnet         | ✅ 生产，有监控  |
| Polygon Mainnet          | ✅ 生产，有监控  |
| Optimism、Arbitrum、Base   | ✅ 生产，无监控  |
| Sepolia、Amoy 等测试网        | ✅ 测试      |
| **BSC Mainnet**          | ❌ **未部署** |
| **BSC Testnet (Chapel)** | ❌ **未部署** |

### 6.2 BSC 测试网方案

**方案：自建 MockOptimisticOracleV2 + 复用已部署合约**

由于 UMA 未在 BSC 部署，我们采用以下策略：

- **复用**：BSC 测试网上已有 ConditionalTokens（Gnosis CTF）和 CTFExchange 的部署
- **自部署**：MockOptimisticOracleV2 + MockAddressWhitelist + UmaCtfAdapter（指向 MockOOv2）

MockOOv2 实现与真实 OOv2 相同的接口：

- `requestPrice()` / `proposePrice()` / `disputePrice()` / `settle()`
- 调用 `UmaCtfAdapter.priceDisputed()` 回调
- 额外提供 `mockDvmSettle(resolution bool)` 供测试模拟 DVM 裁决

差别：

- **无真实 DVM**：用一个管理员账户模拟 DVM 裁决（`mockDvmSettle()`）
- **liveness 可自定义**：测试时设为 120 秒，生产建议 2 小时

这是完全合理的测试方案——生产上线时，只需要：

1. 等 UMA 部署到 BSC（或选择 UMA 已支持的链）
2. 替换 MockOOv2 地址为真实 OOv2 地址，UmaCtfAdapter 指向真实 OO

---

**下一篇**：[BSC 测试网部署方案 + 合约代码 + Go Demo](03-bsc-deployment-guide.md)
