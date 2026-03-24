# 开发踩坑记录

> 记录在 BSC Testnet 上复现 Polymarket 完整流程（UmaCtfAdapter + CTFExchange + ConditionalTokens）过程中遇到的真实错误和解决方法。

---

## 坑 1：matchOrders revert，错误数据为空（`0x`）

### 现象

调用 `CTFExchange.matchOrders()` 时交易 revert，链上 revert data 是空字节 `0x`，没有任何错误信息。

### 根因

`CTFExchange` 在撮合订单之前，会校验交易的两种代币（YES token、NO token）是否已在交易所注册。如果未注册，合约会直接 `revert`
，但由于使用的是底层校验而非自定义错误，revert data 为空。

### 排查过程

1. 怀疑是签名问题，反复检查签名逻辑
2. 怀疑是 EIP-712 domain separator 不对
3. 最终注意到 `CTFExchange` 合约有一个 `registerToken` 函数
4. 用 `isAdmin(deployer)` 确认 deployer 有权限调用
5. 在 `splitPosition` 之后调用 `registerToken(yesTokenId, noTokenId, conditionId)` 解决

### 解决方法

```go
// 获取 YES/NO token ID
var tokenIds []interface{}
CallView(ctfContract, "getPositionId", &tokenIds, usdcAddr, yesIndexSet)
yesTokenId := tokenIds[0].(*big.Int)
CallView(ctfContract, "getPositionId", &tokenIds, usdcAddr, noIndexSet)
noTokenId := tokenIds[0].(*big.Int)

// 必须先注册 token，matchOrders 才不会 revert
Send(client, deployerAuth, exchangeContract, "registerToken",
yesTokenId, noTokenId, conditionId)
```

### 教训

**CTFExchange 的 `registerToken` 是必须步骤**，文档中几乎没有提及，但不调用会导致 matchOrders 静默失败。

---

## 坑 2：matchOrders revert，错误码 `0x756688fe`（`InvalidNonce`）

### 现象

加了 `registerToken` 之后，matchOrders 再次 revert，这次链上有 4 字节错误选择器 `0x756688fe`。

### 排查过程

1. 在 4byte.directory 查询选择器，确认是 `InvalidNonce()`
2. 查看 Polymarket `NonceManager` 合约源码，发现校验逻辑：

```solidity
function isValidNonce(address maker, uint256 nonce) external view returns (bool) {
    return nonces[maker] == nonce;  // 精确匹配，不是 >=
}
```

3. 代码中订单的 `Nonce` 字段硬编码为 `big.NewInt(1)`，但链上 nonces 初始值为 `0`

### 解决方法

```go
// 签名前先从链上读取当前 nonce
var u1Nonce, u2Nonce []interface{}
CallView(exchangeContract, "nonces", &u1Nonce, user1Addr)
CallView(exchangeContract, "nonces", &u2Nonce, user2Addr)

makerOrder := CTFOrder{
Nonce: u1Nonce[0].(*big.Int), // 使用链上实际 nonce
// ...
}
```

### 教训

**Polymarket NonceManager 使用精确匹配（`==`），而不是传统的单调递增（`>=`）**。不能假设初始 nonce 是 1，必须先读链上状态再构造订单。

---

## 坑 3：BSC Testnet 公共 RPC 不稳定（500 Internal Server Error）

### 现象

使用 BSC Testnet 官方公共 RPC（`data-seed-prebsc-1-s1.binance.org:8545`）时，`eth_call` 请求随机返回 HTTP 500，导致
`CallView` 失败。

### 解决方法

**双管齐下：**

1. 改用 Alchemy 稳定节点：
   ```json
   {
     "rpc_url": "https://bnb-testnet.g.alchemy.com/v2/<YOUR_API_KEY>"
   }
   ```

2. 给 `CallView` 加 5 次重试，每次失败等 2 秒：
   ```go
   func CallView(contract *bind.BoundContract, method string, result *[]interface{}, args ...interface{}) {
       var err error
       for i := 0; i < 5; i++ {
           err = contract.Call(&bind.CallOpts{Context: context.Background()}, result, method, args...)
           if err == nil {
               return
           }
           time.Sleep(2 * time.Second)
       }
       log.Fatalf("callView %s: %v", method, err)
   }
   ```

### 教训

**测试网 RPC 不可靠，不能假设单次调用必定成功。** 生产代码中对 eth_call 加重试是基本防御措施。

---

## 坑 4：争议后 requestTime 变了，后续操作必须用新时间戳

### 现象

调用 `OO.disputePrice()` 之后，再次调用 `OO.proposePrice()` 或 `OO.settle()` 时传入原始 `requestTime`（T1），交易失败。

### 根因

`UmaCtfAdapter.priceDisputed()` 回调（由 MockOOv2 在 `disputePrice` 中触发）会更新内部状态：

```solidity
// UmaCtfAdapter.sol
function priceDisputed(
    bytes32 identifier,
    uint256 timestamp,
    bytes memory ancillaryData,
    uint256 refund
) external {
    bytes32 questionId = _getQuestionId(identifier, timestamp, ancillaryData);
    Question storage q = questions[questionId];
    q.requestTime = block.timestamp;  // ← T2（新时间戳）
    _requestPrice(questionId, q.requestTime);  // ← 用 T2 向 OO 重新 requestPrice
}
```

即：**争议发生时，adapter 用 `block.timestamp` 作为新的 T2，并向 OO 提交新的 price request。** 此后所有操作（第二次
proposePrice、settle、resolve）都必须用 T2。

### 解决方法

```go
// 争议后，从 adapter 读取新的 requestTime
func getNewRequestTime(ctx *MarketContext) *big.Int {
var qData []interface{}
CallView(ctx.AdapterContract, "getQuestion", &qData, ctx.QuestionId)
v := reflect.ValueOf(qData[0])
if v.Kind() == reflect.Ptr {
v = v.Elem()
}
f := v.FieldByName("RequestTime")
if rt, ok := f.Interface().(*big.Int); ok {
return rt
}
panic("无法从 getQuestion 结果中读取 requestTime")
}

newRequestTime := getNewRequestTime(ctx)

// 第二次提案用 newRequestTime（T2），不是原来的 ctx.RequestTime（T1）
Send(client, auth, ooContract, "proposePrice",
adapterAddr, identifier, newRequestTime, ancillaryData, price)

// settle 和 resolve 同样用 T2
Send(client, auth, ooContract, "settle",
adapterAddr, identifier, newRequestTime, ancillaryData)
```

### 教训

**OO 的 price request 是以 `(requester, identifier, timestamp, ancillaryData)` 为 key 区分的。** 争议会产生一个全新的
request（T2），原来的 T1 request 进入 Disputed 终态，不能再用于 propose/settle。必须跟踪 adapter 内部的 `requestTime` 变化。

---

## 坑 5：go-ethereum ABI 解码复杂返回值返回匿名 struct，无法直接类型断言

### 现象

调用 `adapter.getQuestion(questionId)` 后，尝试将返回值强制转换为具名 struct 类型时失败：

```go
var result []interface{}
contract.Call(&bind.CallOpts{}, &result, "getQuestion", questionId)
q := result[0].(*Question) // panic: interface conversion failed
```

### 根因

go-ethereum 的 ABI decoder 在没有预先生成 Go binding 的情况下，会将 tuple 类型（Solidity `struct`）解码为**匿名 struct**（
`struct { Field1 type1; Field2 type2; ... }`），而不是任何具名类型。因此无法通过类型断言转换到自定义 struct。

### 解决方法

使用反射按字段名提取：

```go
v := reflect.ValueOf(result[0])
if v.Kind() == reflect.Ptr {
v = v.Elem()
}
f := v.FieldByName("RequestTime") // 按 Solidity 字段名（大写开头）查找
if rt, ok := f.Interface().(*big.Int); ok {
return rt
}
```

> 注意：go-ethereum 将 Solidity 字段名转为 Go 风格（首字母大写），例如 `requestTime` → `RequestTime`。

### 教训

**不要在没有 abigen 生成 binding 的情况下对 tuple 返回值做类型断言。** 要么用 abigen 预先生成 Go binding，要么用反射按字段名取值。

---

## 坑 6：`redeemPositions` 的 `indexSets` 参数与直觉相反

### 现象

赎回 YES 头寸时，传 `indexSets = [1]`；赎回 NO 头寸时，传 `indexSets = [2]`——与"1=YES, 2=NO"的理解没有问题，但容易混淆的是这里传的不是
token ID，而是分区的位掩码。

### 根因

ConditionalTokens 的分区使用**位掩码**表示：

- `partition = [1, 2]` → YES 对应掩码 `0b01 = 1`，NO 对应掩码 `0b10 = 2`
- `redeemPositions(collateral, parentCollection, conditionId, indexSets)` 中 `indexSets` 是你要赎回的分区掩码列表

```go
// 赎回 YES（掩码 = 1）
Send(client, user1Auth, ctfContract, "redeemPositions",
usdcAddr, [32]byte{}, conditionId, []*big.Int{big.NewInt(1)})

// 赎回 NO（掩码 = 2）
Send(client, user2Auth, ctfContract, "redeemPositions",
usdcAddr, [32]byte{}, conditionId, []*big.Int{big.NewInt(2)})
```

### 教训

**ConditionalTokens 中 "YES=1, NO=2" 是 `splitPosition` 时指定的分区 `partition`，赎回时沿用相同的 indexSet 值。** 看起来像
token 序号，其实是位掩码。

---

## 总结

| # | 错误现象                              | 根本原因                              | 解决方法                                |
|---|-----------------------------------|-----------------------------------|-------------------------------------|
| 1 | `matchOrders` revert，data=`0x`    | Token 未在 CTFExchange 注册           | `splitPosition` 后调用 `registerToken` |
| 2 | `matchOrders` revert，`0x756688fe` | NonceManager 精确匹配 nonce，初始值为 0    | 签名前先读链上 nonces                      |
| 3 | `CallView` 随机 HTTP 500            | BSC Testnet 公共 RPC 不稳定            | 改用 Alchemy + 加重试逻辑                  |
| 4 | 争议后 `proposePrice` 失败             | `priceDisputed()` 更新了 requestTime | 争议后从 adapter 读取新 T2                 |
| 5 | `getQuestion` 返回值无法类型断言           | go-ethereum 解码 tuple 为匿名 struct   | 用反射 `FieldByName` 提取字段              |
| 6 | `redeemPositions` 参数混淆            | indexSets 是位掩码，不是 token 序号        | YES=1, NO=2 对应 partition 掩码         |
