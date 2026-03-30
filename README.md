# Polymarket Demo

在BSC测试网（Chapel，Chain ID: 97）上复现Polymarket预测市场完整流程，覆盖正常结算、争议处理、Nonce验证、三种撮合类型，并分别提供UMA
OOv2和OOv3两个版本。

## Dependencies

* [Foundry](https://www.getfoundry.sh/)
* [Golang](https://go.dev/doc/install)

## Build

```bash
cd contracts
forge build
```

## Run

```bash
cd exchange

# 配置 exchange/config.json（RPC、账户私钥等）

# ── OOv2 版本 ──────────────────────────────────────
# 正常结算（YES 赢）
go run ./cmd/normal
# 争议处理（错误提案 → 质疑 → DVM 裁定 → NO 赢）
go run ./cmd/dispute
# Nonce 行为验证
go run ./cmd/nonce_test
# 三种撮合类型（MINT / COMPLEMENTARY / MERGE）
go run ./cmd/match_types

# ── OOv3 版本 ──────────────────────────────────────
go run ./cmd/v3/normal
go run ./cmd/v3/dispute
go run ./cmd/v3/nonce_test
go run ./cmd/v3/match_types
```

## OOv2 vs OOv3 主要区别

### 合约层

| 项目       | OOv2 版本                  | OOv3 版本                  |
|----------|--------------------------|--------------------------|
| Oracle合约 | `MockOptimisticOracleV2` | `MockOptimisticOracleV3` |
| 适配合约     | `UmaCtfAdapter`          | `UmaCtfAdapterV3`        |
| 抵押品白名单   | 需要`MockAddressWhitelist` | 不需要（OOv3移除依赖）            |
| Go初始化函数  | `RunCommonSetup()`       | `RunCommonSetupV3()`     |

### 初始化市场

| 项目    | OOv2                                                          | OOv3                                                |
|-------|---------------------------------------------------------------|-----------------------------------------------------|
| 函数签名  | `initialize(ancillaryData, rewardToken, reward, requestTime)` | `initialize(ancillaryData, proposalBond, liveness)` |
| 内部动作  | `CTF.prepareCondition` + `OO.requestPrice(T1)`                | `CTF.prepareCondition`（无requestPrice）               |
| 请求时间戳 | 有（T1，后续步骤必须用T1）                                               | 无（OOv3不用时间戳定位请求）                                    |

### 提案结果

| 项目   | OOv2                                                  | OOv3                                              |
|------|-------------------------------------------------------|---------------------------------------------------|
| 谁来调用 | 提案者直接调 `OO.proposePrice(...)`                         | 提案者调`adapter.proposeResolution(questionId, bool)` |
| 内部机制 | OO存储int256价格（1e18=YES，0=NO）                           | adapter调`OO.assertTruth()`，OO返回`assertionId`      |
| 请求定位 | (requester, identifier, timestamp, ancillaryData) 四元组 | `assertionId`（bytes32，更简洁）                        |

### 正常结算（无争议）

| 项目   | OOv2                                               | OOv3                                                                   |
|------|----------------------------------------------------|------------------------------------------------------------------------|
| 步骤数  | 两步：`OO.settle(T1)` + `adapter.resolve(questionId)` | 一步：`adapter.settle(questionId)`                                        |
| 内部机制 | OO settle改状态；adapter resolve读价格再调CTF               | `settleAssertion`直接触发`assertionResolvedCallback` → `CTF.reportPayouts` |

### 争议处理

| 项目     | OOv2                                                                                                                         | OOv3                                                                                                            |
|--------|------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| 步骤数    | **7步**                                                                                                                       | **4步**                                                                                                          |
| 流程     | 错误提案 → `disputePrice` → `priceDisputed`回调重置T2 → `mockDvmSettle(T1,false)` → 重新提案(T2) → 等待liveness → `settle(T2)` + `resolve` | 错误提案 → `disputeAssertion` → `mockDvmResolve(false)` → `assertionResolvedCallback(false)`直接触发`CTF.reportPayouts` |
| 关键差异   | DVM裁定后仍需重新提案 + 二次liveness等待                                                                                                  | DVM裁定直接通过回调完成结算，无需重新提案                                                                                          |
| bond流向 | 提案者损失bond，质疑者获得2x bond                                                                                                       | 同左（机制相同）                                                                                                        |

### 不受Oracle版本影响的部分

以下逻辑在V2/V3中完全相同，代码无需修改：

- EIP-712订单签名（`SignOrder`、`OrderTypeHash`、`DomainSeparator`）
- `CTFExchange.matchOrders`（三种 MatchType：MINT / COMPLEMENTARY / MERGE）
- `CTFExchange` NonceManager（`nonces`、`incrementNonce`）
- `CTF.splitPosition` / `CTF.redeemPositions`
- `CTF.registerToken`

## Docs

* [架构分析](./docs/01-architecture-analysis.md)
* [UMA Oracle 接入详解](./docs/02-uma-integration.md)
* [BSC 部署指南](./docs/03-bsc-deployment-guide.md)
* [测试流程](./docs/04-flow-diagram.md)
* [测试中的坑](./docs/05-pitfalls.md)
* [adapter.initialize() 深度解析](./docs/06-initialize-deep-dive.md)
* [matchOrders 填充参数详解](./docs/07-match-fill-amounts.md)

## Logs

**OOv2 版本**

* [正常结算](logs/v2/normal.log)
* [争议处理](logs/v2/dispute.log)
* [Nonce 验证](logs/v2/nonce.log)
* [三种撮合类型](logs/v2/match_types.log)

**OOv3 版本**

* [正常结算](logs/v3/normal.log)
* [争议处理](logs/v3/dispute.log)
* [Nonce 验证](logs/v3/nonce.log)
* [三种撮合类型](logs/v3/match_types.log)
