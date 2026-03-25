# matchOrders 填充参数详解

> 说明`matchOrders`的四个填充参数在三种MatchType下的含义与取值规则。

---

## 参数概览

```solidity
function matchOrders(
    Order memory takerOrder,
    Order[] memory makerOrders,
    uint256 takerFillAmount,           // taker本次交易投入的数量
    uint256 takerReceiveAmount,        // taker本次交易收到的数量（滑点保护下限）
    uint256[] memory makerFillAmounts, // 每个maker本次交易投入的数量
    uint256 takerFeeAmount,            // 从taker侧额外收取的协议手续费
    uint256[] memory makerFeeAmounts   // 从每个maker侧额外收取的协议手续费
) external;
```

**订单结构中的价格字段**（用于校验填充参数合法性）：

```solidity
struct Order {
    uint256 makerAmount;  // BUY方：愿意支付的USDC；SELL方：愿意卖出的代币数量
    uint256 takerAmount;  // BUY方：期望收到的代币数量；SELL方：期望收到的USDC
    uint8 side;           // 0 = BUY，1 = SELL
    // ...
}
```

> **核心规则**：`takerFillAmount` 和 `makerFillAmounts` 描述的是**每一方投入**的数量，`takerReceiveAmount` 描述的是 taker
**收到**的数量。三者必须满足订单中 `makerAmount / takerAmount` 定义的价格比，否则合约 revert。

---

## 三种 MatchType 详解

### MatchType 1：MINT（双边BUY，铸造新代币对）

**触发条件**：takerOrder.side == BUY，makerOrder.side == BUY，两者 tokenId **互补**（YES ↔ NO）。

**资金流**：

```
taker(BUY YES) 出USDC ──┐
                        ├─ Exchange 调用 CTF.splitPosition() 铸造 YES+NO
maker(BUY NO)  出USDC ──┘
→ YES转给taker，NO转给maker
```

**参数含义**：

| 参数                    | 单位        | 含义              | 示例（500 YES/NO @ 0.5 USDC/个） |
|-----------------------|-----------|-----------------|-----------------------------|
| `takerFillAmount`     | USDC      | taker支付的USDC    | 250_000_000（250USDC）        |
| `takerReceiveAmount`  | YES token | taker收到的YES代币数量 | 500_000_000（500个）           |
| `makerFillAmounts[0]` | USDC      | maker支付的USDC    | 250_000_000（250USDC）        |
| `takerFeeAmount`      | USDC      | 从taker侧收取的协议费   | 0（无手续费时）                    |

**合法性校验**（合约内部）：

```
takerFillAmount / takerReceiveAmount ≤ takerOrder.makerAmount / takerOrder.takerAmount
（taker 实际付出的价格 ≤ 订单中愿意支付的最高价格）
```

---

### MatchType 2：COMPLEMENTARY（BUY vs SELL，代币直接换手）

**触发条件**：takerOrder.side == BUY，makerOrder.side == SELL，两者 tokenId **相同**。

**资金流**：

```
taker(BUY YES)  出 USDC  → maker(SELL YES)
maker(SELL YES) 出 YES   → taker(BUY YES)
```

**参数含义**：

| 参数                    | 单位        | 含义              | 示例（500 YES @ 0.5 USDC/个） |
|-----------------------|-----------|-----------------|--------------------------|
| `takerFillAmount`     | USDC      | taker支付的USDC    | 250_000_000（250 USDC）    |
| `takerReceiveAmount`  | YES token | taker收到的YES代币数量 | 500_000_000（500 个）       |
| `makerFillAmounts[0]` | YES token | maker卖出的YES代币数量 | 500_000_000（500 个）       |
| `takerFeeAmount`      | USDC      | 从taker侧收取的协议费   | 0（无手续费时）                 |

**注意**：`takerReceiveAmount == makerFillAmounts[0]`（taker 收到的 = maker 卖出的），两者恒等，因为是直接换手。

---

### MatchType 3：MERGE（双边 SELL，销毁代币对换回 USDC）

**触发条件**：takerOrder.side == SELL，makerOrder.side == SELL，两者 tokenId **互补**（YES ↔ NO）。

**资金流**：

```
taker(SELL YES) 出 YES ──┐
                         ├─ Exchange 调用 CTF.mergePositions() 销毁 YES+NO 对
maker(SELL NO)  出 NO  ──┘
→ USDC 分别返还给 taker 和 maker
```

**参数含义**：

| 参数                    | 单位        | 含义                   | 示例（500 YES/NO @ 0.5 USDC/个） |
|-----------------------|-----------|----------------------|-----------------------------|
| `takerFillAmount`     | YES token | taker卖出的YES代币数量      | 500_000_000（500个）           |
| `takerReceiveAmount`  | USDC      | taker收到的USDC         | 250_000_000（250USDC）        |
| `makerFillAmounts[0]` | NO token  | maker卖出的NO代币数量       | 500_000_000（500个）           |
| `takerFeeAmount`      | USDC      | 从taker收到的USDC中扣除的协议费 | 0（无手续费时）                    |

---

## 三种 MatchType 对比汇总

|                         | MINT               | COMPLEMENTARY      | MERGE              |
|-------------------------|--------------------|--------------------|--------------------|
| taker side              | BUY                | BUY                | SELL               |
| maker side              | BUY                | SELL               | SELL               |
| tokenId关系               | 互补（YES↔NO）         | 相同                 | 互补（YES↔NO）         |
| `takerFillAmount`单位     | **USDC**（taker付出）  | **USDC**（taker付出）  | **Token**（taker付出） |
| `takerReceiveAmount`单位  | **Token**（taker收到） | **Token**（taker收到） | **USDC**（taker收到）  |
| `makerFillAmounts[0]`单位 | **USDC**（maker付出）  | **Token**（maker付出） | **Token**（maker付出） |
| Exchange内部操作            | splitPosition（铸造）  | 直接转账               | mergePositions（销毁） |

**规律**：

- BUY方投入USDC，收到Token
- SELL方投入Token，收到USDC
- `takerFillAmount`的单位跟随takerOrder.side：BUY → USDC，SELL → Token
- `takerReceiveAmount`的单位与`takerFillAmount`相反

---

## takerFeeAmount / makerFeeAmounts 说明

这两个字段是在正常交易金额之外额外收取的**协议手续费**，与订单中 `feeRateBps` 字段不同：

| 字段                   | 位置            | 说明                                             |
|----------------------|---------------|------------------------------------------------|
| `feeRateBps`         | Order struct内 | 订单级别的费率（基点），由maker/taker在签名时承诺接受的最大费率          |
| `takerFeeAmount`     | matchOrders参数 | operator本次撮合实际从taker收取的手续费金额（必须≤feeRateBps换算值） |
| `makerFeeAmounts[i]` | matchOrders参数 | operator本次撮合实际从每个maker收取的手续费金额                 |

手续费单位与对应方投入资产相同：BUY方费用以 **USDC**计，SELL方费用以**Token**计。

无手续费场景（`feeRateBps = 0`）时，两者均传 `0`。

---

## 与本项目4参数版本的对比

本项目使用的BSC Testnet CTFExchange 是团队成员 Ming 部署的测试合约，使用旧版4参数接口：

```solidity
// 旧版（本项目使用）
matchOrders(takerOrder, makerOrders, takerFillAmount, makerFillAmounts)

// 新版（Polymarket 生产环境）
matchOrders(takerOrder, makerOrders, takerFillAmount, takerReceiveAmount, makerFillAmounts, takerFeeAmount, makerFeeAmounts)
```

新增的`takerReceiveAmount`作用是**滑点保护**：合约会校验taker实际收到的数量不低于此值，防止operator
以不利价格撮合。旧版没有这个校验，完全依赖订单的`makerAmount/takerAmount`比例。
