# BSC 测试网部署方案

> 本文档说明如何在 BSC 测试网（Chapel，Chain ID: 97）上部署和测试 Polymarket 完整流程。
> 代码位于 `polymarket/exchange/` 目录，使用 Go 编写。

---

## 一、架构概述

Polymarket 的完整合约体系在 BSC 测试网上的部署策略：

```
┌─────────────────────────────────────────────────────────────────────┐
│  复用（BSC 测试网已有部署）                                             │
│                                                                     │
│  ConditionalTokens (Gnosis CTF)  ← ERC1155 YES/NO 代币              │
│  CTFExchange                     ← 混合订单簿交易所                    │
│  MockUSDC (ChildERC20)           ← 测试用 USDC（6位精度）              │
└─────────────────────────────────────────────────────────────────────┘
                            ↑ 调用
┌─────────────────────────────────────────────────────────────────────┐
│  自部署（每次测试重新部署，地址不固定）                                   │
│                                                                     │
│  MockAddressWhitelist            ← 抵押品白名单（始终返回 true）         │
│  MockOptimisticOracleV2          ← 模拟 UMA OOv2，deployer 充当 DVM  │
│  UmaCtfAdapter                   ← Oracle 适配层（Polymarket 开源）    │
└─────────────────────────────────────────────────────────────────────┘
```

**与生产环境的差异**：

| 组件               | 生产（Polygon）         | 测试（BSC Testnet）            |
|------------------|---------------------|----------------------------|
| OO Oracle        | 真实 UMA OOv2         | MockOOv2（管理员模拟 DVM）        |
| DVM 仲裁           | UMA 代币持有者投票（48-96h） | `mockDvmSettle(bool)` 即时裁定 |
| AddressWhitelist | UMA 治理维护            | MockWhitelist（恒返回 true）    |
| CTF / Exchange   | 同一套合约               | 同一套合约（复用）                  |
| liveness         | 约 2 小时              | 120 秒（可配置）                 |

---

## 二、合约说明

### 2.1 自部署合约（`contracts/src/`）

| 文件                         | 合约名                      | 说明                                          |
|----------------------------|--------------------------|---------------------------------------------|
| `MockAddressWhitelist.sol` | `MockAddressWhitelist`   | 白名单合约，`isOnWhitelist()` 恒返回 true，满足 OOv2 要求 |
| `MockOOv2.sol`             | `MockOptimisticOracleV2` | 模拟 UMA OOv2，实现完整的 propose/dispute/settle 流程 |
| `UmaCtfAdapter.sol`        | `UmaCtfAdapter`          | Polymarket 开源的适配合约，桥接 OO 与 CTF              |

### 2.2 MockOOv2 关键机制

```
requestPrice(requester, identifier, timestamp, ancillaryData)
    → 创建 price request，状态: Requested

proposePrice(requester, identifier, timestamp, ancillaryData, price)
    → 状态: Proposed，进入 liveness 窗口

disputePrice(requester, identifier, timestamp, ancillaryData)
    → 回调 requester.priceDisputed()（即 UmaCtfAdapter）
    → 状态: Disputed
    → UmaCtfAdapter 内部更新 requestTime=block.timestamp（T2）
    → UmaCtfAdapter 向 OO 发起新的 requestPrice(T2)
    → OO 状态（T1）重置为 Requested（由 mockDvmSettle 清理）

mockDvmSettle(requester, identifier, timestamp, ancillaryData, resolution)
    → 仅 deployer（DVM角色）可调用
    → resolution=true:  提案正确，disputer 损失 bond
    → resolution=false: 提案错误，disputer 获得 2x bond

settle(requester, identifier, timestamp, ancillaryData)
    → liveness 结束后调用，finalizes price
    → 返回 bond 给提案者
```

---

## 三、环境准备

### 3.1 安装依赖

```bash
# 安装 Foundry（合约编译）
curl -L https://foundry.paradigm.xyz | bash && foundryup

# 编译合约（生成 out/ 目录供 Go 读取）
cd polymarket/contracts
forge build

# 安装 Go 依赖
cd ../exchange
go mod tidy
```

### 3.2 配置文件

`exchange/config.json`：

```json
{
  "rpc_url": "https://bnb-testnet.g.alchemy.com/v2/<YOUR_API_KEY>",
  "accounts": {
    "deployer_private_key": "...",
    "user1_private_key": "...",
    "user2_private_key": "...",
    "operator_private_key": "..."
  },
  "contracts": {
    "ctf": "0x...",
    "exchange": "0x...",
    "usdc": "0x..."
  },
  "market": {
    "ancillary_data": "q: Will BNB exceed $1000 by 2025-12-31?",
    "proposal_bond_usdc": 100,
    "liveness_seconds": 120
  },
  "gas": {
    "price_gwei": 3,
    "limit": 3000000
  }
}
```

**账户说明**：

| 账户       | 角色                                      |
|----------|-----------------------------------------|
| deployer | 部署合约 + 充当 DVM（调用 mockDvmSettle）+ 提案者    |
| user1    | 买 YES，撮合后持有 YES 代币                      |
| user2    | 买 NO，撮合后持有 NO 代币，争议流程中充当质疑者             |
| operator | CTFExchange operator，唯一有权调用 matchOrders |

**建议 RPC**：使用 Alchemy BSC Testnet 节点，避免公共 RPC 的随机 500 错误。

### 3.3 获取测试 BNB

访问 https://testnet.bnbchain.org/faucet-smart 获取测试 BNB（所有账户都需要 BNB 支付 Gas）。

---

## 四、运行演示

从 `exchange/` 目录运行：

```bash
# 场景 A：正常结算（YES 赢）
go run ./cmd/normal

# 场景 B：争议处理（NO 赢）
go run ./cmd/dispute

# 使用指定配置文件
go run ./cmd/normal -config /path/to/config.json
```

---

## 五、完整流程说明

### 5.1 公共步骤（步骤 1-5，两个场景共享）

**步骤 1：部署合约**

```
deployer 部署 MockAddressWhitelist
deployer 部署 MockOptimisticOracleV2（传入 deployer 地址作为 DVM）
deployer 部署 UmaCtfAdapter（传入 CTF, MockOOv2, Whitelist 地址）
检查 operator 权限，如未注册则调用 CTFExchange.addOperator()
```

**步骤 2：铸造测试 USDC**

```
USDC 合约（ChildERC20）：deployer 调用 deposit(user, amount) 铸造
User1 +10000 USDC
User2 +10000 USDC
```

**步骤 3：初始化市场**

```
deployer 调用 UmaCtfAdapter.initialize(ancillaryData, usdc, reward=0, bond, liveness)
    ↓ adapter 内部调用
ConditionalTokens.prepareCondition(adapter地址, questionId, 2)
    → 创建 conditionId
OO.requestPrice(identifier, timestamp, ancillaryData)
    → 记录 requestTime = initialize 区块时间戳（T1）

计算 YES/NO tokenId：
    yesCollectionId = CTF.getCollectionId(bytes32(0), conditionId, 1)  // indexSet=1=0b01
    noCollectionId  = CTF.getCollectionId(bytes32(0), conditionId, 2)  // indexSet=2=0b10
    yesTokenId      = CTF.getPositionId(usdc, yesCollectionId)
    noTokenId       = CTF.getPositionId(usdc, noCollectionId)

注册代币对（必须步骤，否则 matchOrders 会静默 revert）：
    CTFExchange.registerToken(yesTokenId, noTokenId, conditionId)
```

**步骤 4：拆分头寸**

```
User1 和 User2 各执行：
    USDC.approve(CTF, 1000e6)
    CTF.setApprovalForAll(Exchange, true)  // 授权交易所转移 ERC1155
    CTF.splitPosition(usdc, bytes32(0), conditionId, [1,2], 1000e6)
        → 锁入 1000 USDC，铸造 1000 YES + 1000 NO（ERC1155）

执行后：
    User1: 1000 YES + 1000 NO
    User2: 1000 YES + 1000 NO
```

**步骤 5：订单撮合**

```
场景：User1 以 0.5 USDC/个 卖出 1000 NO，User2 以 0.5 USDC/个 买入 1000 NO

User2 需先 approve：USDC.approve(Exchange, 500e6)

构造订单（链下 EIP-712 签名）：
    读取链上 nonce（重要：NonceManager 使用精确匹配，初始值为 0）
    makerOrder: User1 SELL 1000 NO（makerAmount=1000e6 NO，takerAmount=500e6 USDC）
    takerOrder: User2 BUY  1000 NO（makerAmount=500e6 USDC，takerAmount=1000e6 NO）

operator 调用 CTFExchange.matchOrders(takerOrder, [makerOrder], 500e6, [1000e6])

执行后：
    User1: 1000 YES + 0 NO + 500 USDC（卖出 NO，收回 USDC）
    User2: 1000 YES + 2000 NO + ~9000 USDC（买入 NO）
```

### 5.2 场景 A：正常结算（步骤 6-9）

```
步骤 6: 提案 YES 赢
    deployer.approve(OO, bond)
    OO.proposePrice(adapter, "YES_OR_NO_QUERY", T1, ancillaryData, 1e18)
    → 状态: Proposed，进入 liveness（120 秒）

步骤 7: 等待 liveness 结束（无人质疑）

步骤 8: 结算
    OO.settle(adapter, "YES_OR_NO_QUERY", T1, ancillaryData)
        → OO 状态: Resolved，bond 返还给 proposer
    adapter.resolve(questionId)
        → 查询 OO 价格（1e18 = YES）
        → CTF.reportPayouts(questionId, [1e18, 0])
        → conditionId 对应的 YES 可兑换，NO 清零

步骤 9: 赎回
    User1 赎回 YES: CTF.redeemPositions(usdc, bytes32(0), conditionId, [1])
        → 1000 YES → +1000 USDC
    User2 赎回 NO: CTF.redeemPositions(usdc, bytes32(0), conditionId, [2])
        → 2000 NO → +0 USDC（YES 赢，NO 归零）
```

### 5.3 场景 B：争议处理（步骤 6-12）

```
步骤 6: 提案错误结果（YES，但实际应为 NO）
    deployer.approve(OO, bond)
    OO.proposePrice(adapter, "YES_OR_NO_QUERY", T1, ancillaryData, 1e18)  ← 错误提案

步骤 7: User2 发起质疑
    user2.approve(OO, bond)
    OO.disputePrice(adapter, "YES_OR_NO_QUERY", T1, ancillaryData)
        → OO 回调 adapter.priceDisputed()
        → adapter 内部: q.requestTime = block.timestamp（T2）
        → adapter 向 OO 发起新 requestPrice(T2)
        → 读取新 requestTime: adapter.getQuestion(questionId).requestTime

步骤 8: DVM 裁定（质疑者胜）
    OO.mockDvmSettle(adapter, "YES_OR_NO_QUERY", T1, ancillaryData, false)
        → resolution=false: disputer（User2）获得 2x bond = +200 USDC

步骤 9: 重新提案正确结果（NO 赢，使用 T2）
    deployer.approve(OO, bond)
    OO.proposePrice(adapter, "YES_OR_NO_QUERY", T2, ancillaryData, 0)  ← price=0 = NO 赢

步骤 10: 等待 liveness 结束（无新质疑）

步骤 11: 结算
    OO.settle(adapter, "YES_OR_NO_QUERY", T2, ancillaryData)
    adapter.resolve(questionId)
        → 价格=0 → NO 赢
        → CTF.reportPayouts(questionId, [0, 1e18])

步骤 12: 赎回
    User1 赎回 YES: 1000 YES → +0 USDC（NO 赢，YES 归零）
    User2 赎回 NO: 2000 NO  → +2000 USDC
```

---

## 六、项目结构

```
exchange/
├── config.json          # 配置文件（RPC、私钥、合约地址、市场参数）
├── go.mod               # Go 模块（module: polymarket-exchange）
├── shared.go            # 公共库（package exchange）：ABI、签名、部署、公共步骤 1-5
└── cmd/
    ├── normal/
    │   └── main.go      # 正常结算演示（步骤 6-9）
    └── dispute/
        └── main.go      # 争议处理演示（步骤 6-12）

contracts/
├── src/
│   ├── MockAddressWhitelist.sol
│   ├── MockOOv2.sol
│   └── UmaCtfAdapter.sol
└── out/                 # forge build 输出（ABI + bytecode），Go 代码从此读取
```

---

## 七、生产迁移清单

当准备上线真实网络时：

- [ ] 选择 UMA 支持的网络（Polygon、Ethereum、Base 等）
- [ ] 使用真实 UMA OOv2 地址替换 MockOOv2
- [ ] 使用真实 AddressWhitelist 替换 MockWhitelist
- [ ] UmaCtfAdapter 指向真实 OO 和 Whitelist
- [ ] 调整 `proposalBond`（Polymarket 生产约 1500 USDC）
- [ ] 调整 `liveness`（建议 ≥ 7200 秒 = 2 小时）
- [ ] 部署 Watcher Bot（监控 OO 的 `proposePrice` 事件，及时质疑错误提案）
- [ ] 审计 UmaCtfAdapter 和任何自定义逻辑

---

**相关文档**：

- [架构分析](01-architecture-analysis.md)
- [UMA 接入详解](02-uma-integration.md)
- [流程图](04-flow-diagram.md)
- [踩坑记录](05-pitfalls.md)
