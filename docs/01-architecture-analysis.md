# Polymarket 合约框架体系全面分析

> 本文档分析Polymarket在Polygon上的完整合约架构，梳理各合约的职责、地址和调用关系。

---

## 一、整体架构图

Polymarket由五层合约组成：

```
┌─────────────────────────────────────────────────────────────────┐
│  Layer 5: 用户交互层                                              │
│  代理钱包（ProxyWallet）+ 前端 + API                               │
└────────────────────────┬────────────────────────────────────────┘
                         │ 下单/授权
┌────────────────────────▼────────────────────────────────────────┐
│  Layer 4: 交易层                                                 │
│  CTFExchange            NegRiskCTFExchange                      │
│  (标准二元市场交易)        (互斥多结果市场交易)                       │
└────────────────────────┬────────────────────────────────────────┘
                         │ 结算/铸造
┌────────────────────────▼────────────────────────────────────────┐
│  Layer 3: 条件代币层                                              │
│  ConditionalTokens (Gnosis CTF)                                 │
│  ERC1155 YES/NO 代币 + 条件结算                                   │
└──────────────┬─────────────────────────────┬────────────────────┘
               │ prepareCondition            │ prepareCondition
┌──────────────▼──────────────┐  ┌───────────▼────────────────────┐
│  Layer 2A: 标准预言机适配层   │  │  Layer 2B: NegRisk 适配层       │
│  UmaCtfAdapter              │  │  NegRiskAdapter                │
│  (二元市场 + UMA OOv2)       │  │  NegRiskOperator               │
└──────────────┬──────────────┘  │  NegRiskUmaCtfAdapter          │
               │                 │  NegRiskWrappedCollateral      │
               │                 │  NegRiskVault                  │
               │                 └───────────┬────────────────────┘
               │                             │
┌──────────────▼─────────────────────────────▼────────────────────┐
│  Layer 1: UMA 乐观预言机层                                        │
│  OptimisticOracleV2  /  OptimisticOracleV3                      │
│  DVM (Data Verification Mechanism)                              │
└─────────────────────────────────────────────────────────────────┘
```

---

## 二、各合约详细说明

### 2.1 ConditionalTokens（Gnosis CTF）

**地址（Polygon Mainnet）**：`0x4d97dcd97ec945f40cf65f87097ace5ea0476045`

Polymarket 的底层资产系统，来自 Gnosis，采用 ERC1155 标准。

**核心概念**：

- **Condition（条件）**：由 `conditionId = keccak256(oracle, questionId, outcomeSlotCount)` 唯一标识
- **Outcome Token**：每个条件创建 N 个结果代币，二元市场为 YES（index 0）和 NO（index 1）
- **Collateral**：抵押品（USDC），铸造时锁入，结算时分配

**关键函数**：

```solidity
// 创建条件（必须由 oracle 调用）
function prepareCondition(address oracle, bytes32 questionId, uint outcomeSlotCount) external;

// 分割抵押品为条件代币（用户买入）
function splitPosition(
    IERC20 collateralToken,
    bytes32 parentCollectionId,
    bytes32 conditionId,
    uint[] calldata partition,
    uint amount
) external;

// 合并条件代币为抵押品（赎回）
function mergePositions(
    IERC20 collateralToken,
    bytes32 parentCollectionId,
    bytes32 conditionId,
    uint[] calldata partition,
    uint amount
) external;

// 条件结算后赎回胜利代币
function redeemPositions(
    IERC20 collateralToken,
    bytes32 parentCollectionId,
    bytes32 conditionId,
    uint[] calldata indexSets
) external;

// 上报结果（必须由 oracle 调用，数值归一化）
function reportPayouts(bytes32 questionId, uint[] calldata payouts) external;
```

**结算逻辑**：

- `payouts = [1, 0]` → YES 赢（持有者获得全额 USDC）
- `payouts = [0, 1]` → NO 赢
- `payouts = [1, 1]` → 平局（各得 50%）

---

### 2.2 UmaCtfAdapter（Oracle 适配层，核心合约）

**地址（Polygon Mainnet）**：

| 版本         | 地址                                           |
|------------|----------------------------------------------|
| v3.1.0（当前） | `0x157Ce2d672854c848c9b79C49a8Cc6cc89176a49` |
| v3.0.0     | `0x71392E133063CC0D16F40E1F9B60227404Bc03f7` |
| v2.0.0     | `0x6A9D222616C90FcA5754cd1333cFD9b7fb6a4F74` |

这是 Polymarket 最核心的适配合约，**桥接 UMA OptimisticOracleV2 和 Gnosis CTF**。

**使用的 Oracle 版本**：OOv2（不是 OOv3），identifier 为 `YES_OR_NO_QUERY`

**关键状态**：

```solidity
IConditionalTokens public immutable ctf;
IOptimisticOracleV2 public immutable optimisticOracle;
IAddressWhitelist public immutable collateralWhitelist;
uint256 public constant SAFETY_PERIOD = 1 hours;
bytes32 public constant YES_OR_NO_IDENTIFIER = "YES_OR_NO_QUERY";

struct QuestionData {
uint256 requestTime;       // 请求时间
uint256 reward;            // 成功解析的奖励
uint256 proposalBond;      // 提案押金
bool paused;               // 是否暂停
bool resolved;             // 是否已解析
bool flagged;              // 是否标记等待人工干预
address rewardToken;       // 奖励代币
}
mapping(bytes32 => QuestionData) public questions;
```

**核心函数**：

```solidity
// 1. 初始化市场（Polymarket 运营者调用，会先 prepareCondition 到 CTF）
function initialize(
    bytes32 questionId,       // 问题唯一标识
    bytes memory ancillaryData, // 问题描述（自然语言，UTF-8）
    address rewardToken,      // 奖励代币（通常 USDC）
    uint256 reward,           // 成功解析的奖励金额
    uint256 proposalBond      // 提案者需要锁定的押金
) external;

// 2. 解析市场（任何人在 OO 有结果后调用）
function resolve(bytes32 questionId) external;

// 3. 检查是否可以解析
function ready(bytes32 questionId) external view returns (bool);

// 4. 获取预期 payout
function getExpectedPayouts(bytes32 questionId) external view returns (uint256[] memory);

// 5. OO 价格被质疑时的回调（由 OO 调用）
function priceDisputed(
    bytes32 identifier,
    uint256 timestamp,
    bytes memory ancillaryData,
    uint256 refund
) external;

// 6. 管理员标记市场（人工干预）
function flag(bytes32 questionId) external;

// 7. 人工解析（需先 flag，SAFETY_PERIOD 后生效）
function resolveManually(bytes32 questionId, uint256[] calldata payouts) external;

// 8. 重置 OO 请求（质疑后重新请求）
function reset(bytes32 questionId) external;
```

**关键逻辑**：OOv2 返回的价格值：

- `1e18`（1 ether）→ YES 赢 → payouts = [1e18, 0]
- `0` → NO 赢 → payouts = [0, 1e18]
- `0.5e18` → 平局 → payouts = [0.5e18, 0.5e18]
- 其他值 → 无效，直接 payout 全为 0（视为无效市场）

---

### 2.3 CTFExchange（交易层）

**地址（Polygon Mainnet）**：`0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E`
**地址（Amoy 测试网）**：`0xdFE02Eb6733538f8Ea35D585af8DE5958AD99E40`

**功能**：混合去中心化交易所（链下撮合，链上结算），实现 CTF ERC1155 代币与 USDC 的原子交换。

**架构**：

```
用户签名链下订单 → Polymarket 撮合引擎 → CTFExchange.matchOrders() 链上结算
```

**关键函数**：

```solidity
// 撮合订单（operator 调用）
function matchOrders(
    Order calldata takerOrder,
    Order[] calldata makerOrders,
    uint takerFillAmount,
    uint[] calldata makerFillAmounts
) external;

// 查询已成交量
function getOrderStatus(bytes32 orderHash) external view returns (OrderStatus memory);
```

**订单结构**：

```solidity
struct Order {
    uint256 salt;           // 防重放
    address maker;          // 下单者
    address signer;         // 签名者（代理钱包）
    address taker;          // 对手方（0 = 任何人）
    uint256 tokenId;        // ERC1155 tokenId（对应条件代币）
    uint256 makerAmount;    // maker 提供的数量
    uint256 takerAmount;    // maker 期望获得的数量
    uint256 expiration;     // 过期时间
    uint256 nonce;          // 防重放
    uint8 side;             // 0=BUY, 1=SELL
    uint8 signatureType;    // 签名类型
    bytes signature;        // 签名
}
```

---

### 2.4 NegRisk 相关合约（互斥多结果市场）

**地址（Polygon Mainnet）**：

| 合约                       | 地址                                           |
|--------------------------|----------------------------------------------|
| NegRiskAdapter           | `0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296` |
| NegRiskCTFExchange       | `0xC5d563A36AE78145C45a50134d48A1215220f80a` |
| NegRiskOperator          | `0x71523d0f655B41E805Cec45b17163f528B59B820` |
| NegRiskVault             | `0x7f67327E88c258932D7d8f72950bE0d46975E11D` |
| NegRiskUmaCtfAdapter     | `0x2F5e3684cb1F318ec51b00Edba38d79Ac2c0aA9d` |
| NegRiskWrappedCollateral | `0x3A3BD7bb9528E159577F7C2e685CC81A765002E2` |

**NegRisk 解决的问题**：

在多候选人选举市场中，持有"拜登 NO + 哈里斯 NO"等价于持有"特朗普 YES"加上一定 USDC。NegRiskAdapter 允许用户将一组 NO
代币转换为等价的 YES 代币，减少资金占用。

```
NegRisk 转换逻辑：
NO(A) + NO(B) + ... + NO(N) ←→ YES(剩余候选者) + (N-1) USDC
```

**NegRiskOperator**：管理市场准备和问题初始化，与 NegRiskUmaCtfAdapter 协作进行 Oracle 解析。

---

## 三、完整交易流程（标准二元市场）

### 3.1 市场创建

```
Polymarket 运营者
    ↓
UmaCtfAdapter.initialize(questionId, ancillaryData, rewardToken, reward, proposalBond)
    ↓ 内部调用
ConditionalTokens.prepareCondition(adapter地址, questionId, 2)
    → 创建 conditionId，并准备 YES/NO 两个 slot
```

### 3.2 用户购买

```
用户（通过代理钱包）
    ↓ 签名 Order
CTFExchange.matchOrders(...)
    ↓ 内部调用
ConditionalTokens.splitPosition(USDC, bytes32(0), conditionId, [1,2], amount)
    → 铸造 YES/NO ERC1155 代币
    → 将 USDC 锁入 CTF
```

### 3.3 市场结算（Oracle 流程）

```
Step 1: 提案者（任何人）
    ↓
UMA OOv2.proposePriceFor(
    proposer,
    UmaCtfAdapter地址,
    "YES_OR_NO_QUERY",
    questionId时间戳,
    ancillaryData,
    price=1e18(YES) 或 0(NO)
)
    → 提案者锁定 proposalBond

Step 2: 质疑窗口（约 2 小时）
    ├── 无质疑 → 进入 Step 3
    └── 有质疑 → UmaCtfAdapter.priceDisputed() 被调用
                → 自动 reset，重新发起 OO 请求
                → 重复 Step 1

Step 3: 任何人调用
    ↓
UmaCtfAdapter.resolve(questionId)
    ↓ 内部查询 OO 价格
ConditionalTokens.reportPayouts(questionId, payouts)
    → 条件结算，YES/NO 代币面值确定

Step 4: 用户赎回
    ↓
ConditionalTokens.redeemPositions(USDC, bytes32(0), conditionId, [winnerIndex])
    → 胜利者代币换回 USDC
```

---

## 四、合约地址汇总表

### Polygon Mainnet（生产环境）

| 合约                   | 地址                                           | 说明         |
|----------------------|----------------------------------------------|------------|
| USDC                 | `0x2791bca1f2de4661ed88a30c99a7a9449aa84174` | 抵押品        |
| ConditionalTokens    | `0x4d97dcd97ec945f40cf65f87097ace5ea0476045` | Gnosis CTF |
| UmaCtfAdapter        | `0x157Ce2d672854c848c9b79C49a8Cc6cc89176a49` | Oracle 适配  |
| CTFExchange          | `0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E` | 交易所        |
| NegRiskAdapter       | `0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296` | 多结果市场      |
| NegRiskCTFExchange   | `0xC5d563A36AE78145C45a50134d48A1215220f80a` | 多结果交易所     |
| NegRiskOperator      | `0x71523d0f655B41E805Cec45b17163f528B59B820` | 多结果管理      |
| NegRiskUmaCtfAdapter | `0x2F5e3684cb1F318ec51b00Edba38d79Ac2c0aA9d` | 多结果 Oracle |

### Polygon Amoy（测试网）

| 合约            | 地址                                           |
|---------------|----------------------------------------------|
| CTFExchange   | `0xdFE02Eb6733538f8Ea35D585af8DE5958AD99E40` |
| UmaCtfAdapter | `0x2F6f8DA6A21023E62399801945eed1b1975A4e12` |

### UMA 合约（Ethereum Mainnet）

| 合约                 | 地址                                           |
|--------------------|----------------------------------------------|
| OptimisticOracleV3 | `0xfb55F43fB9F48F63f9269DB7Dde3BbBe1ebDC0dE` |
| OptimisticOracleV2 | `0xA0Ae6609447e57a42c51B50EAe921D701823FFAe` |
| Finder             | `0x40f941E48A552bF496B154Af6bf55725f18D77c3` |
| VotingToken (UMA)  | `0x04Fa0d235C4abf4BcF4787aF4CF447DE572eF828` |

---

## 五、Ming 做的代理钱包操作说明

Polymarket 为每个用户创建一个**代理钱包（Proxy Wallet）**，用于：

1. **创建代理钱包**：用户首次登录时，Polymarket 为其创建一个可升级代理钱包（类似 Safe/Gnosis 多签的简化版）
2. **授权**：用户授权代理钱包操作 USDC（approve）和 CTF 代币（setApprovalForAll）
3. **交易上链**：
    - 用户在前端签名订单（EIP-712 结构化签名，不上链）
    - 撮合引擎收集订单后，调用 `CTFExchange.matchOrders()`
    - 代理钱包的 `signer` 字段指向代理钱包地址，`maker` 字段是用户地址

这种设计的好处：**用户资产始终在自己的代理钱包中，Polymarket 无法动用**，但可以代替用户提交交易（用户不需要持有 MATIC 付
Gas）。

---

**下一篇**：[UMA Oracle 接入详解与适配层分析](02-uma-integration.md)
