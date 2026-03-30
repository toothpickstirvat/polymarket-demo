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
// 签名前先从链上读取当前 nonce.log
var u1Nonce, u2Nonce []interface{}
CallView(exchangeContract, "nonces", &u1Nonce, user1Addr)
CallView(exchangeContract, "nonces", &u2Nonce, user2Addr)

makerOrder := CTFOrder{
Nonce: u1Nonce[0].(*big.Int), // 使用链上实际 nonce.log
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

---

## 坑 7：go-ethereum v1.13 复用非空 result slice 导致 "cannot unmarshal" panic

### 现象

在同一个函数中多次调用 `CallView`，第二次调用时 panic：

```
cannot unmarshal *big.Int in to bool
```

### 根因

go-ethereum v1.13 的 `BoundContract.Call` 对 `result *[]interface{}` 的处理逻辑分两路：

- `len(*result) == 0`：ABI 解码时新建正确类型填入 slice
- `len(*result) > 0`：走 `UnpackIntoInterface` 路径，**尝试将新值解码进现有元素的类型**

如果第一次调用（如 `isAdmin`，返回 `bool`）后 slice 里留了一个 `bool`，第二次调用（如 `nonces`，返回 `uint256`）就会因类型不匹配而 panic。

### 解决方法

**每次调用 `CallView` 都声明一个全新的空 slice，绝不复用：**

```go
// ✗ 错误：复用同一个 slice
var result []interface{}
CallView(contract, "isAdmin", &result, addr)   // result 现在有一个 bool
CallView(contract, "nonces", &result, addr)    // panic: cannot unmarshal *big.Int in to bool

// ✓ 正确：每次用新 slice，或封装成函数
func readNonce(ctx *MarketContext, addr common.Address) *big.Int {
    var result []interface{}  // 每次新建
    CallView(ctx.ExchangeContract, "nonces", &result, addr)
    return result[0].(*big.Int)
}
```

### 教训

**go-ethereum `Call` 的非空 slice 路径是隐患。** 不同方法的返回类型不同，复用 slice 会触发类型断言失败。养成"一个 CallView 对应一个新 slice"的习惯，或将每种查询封装成独立辅助函数。

---

## 坑 8：测试 USDC 余额跨轮次累积

### 现象

第二次（或之后）运行测试脚本时，User1/User2 的 USDC 余额远大于 10000，例如：

```
User1 USDC: 19500.00  # 上一轮剩余 9500 + 本轮新增 10000
```

后续步骤的断言或余额校验因此失败。

### 根因

BSC Testnet 上的 MockUSDC（ChildERC20）使用 `deposit(user, amount_as_bytes)` 铸币，每次调用都在原有余额上**累加**，没有任何重置逻辑。测试脚本直接 `deposit` 不会清除上一轮余额。

### 解决方法

铸币前先将用户现有余额转回 deployer，再重新 `deposit` 固定金额：

```go
for _, info := range []struct{ addr common.Address; key *ecdsa.PrivateKey }{
    {user1Addr, user1Key}, {user2Addr, user2Key},
} {
    var existing []interface{}
    CallView(usdcContract, "balanceOf", &existing, info.addr)
    if bal := existing[0].(*big.Int); bal.Sign() > 0 {
        // 用户自己的私钥签名，将余额退回 deployer
        Send(client, NewAuth(client, info.key), usdcContract, "transfer", deployerAddr, bal)
    }
    // 重新 deposit 固定金额（10000 USDC）
    depositData := make([]byte, 32)
    mintAmount.FillBytes(depositData)
    Send(client, deployerAuth, usdcContract, "deposit", info.addr, depositData)
}
```

### 教训

**测试脚本的"初始化"步骤必须显式重置状态，而不是假设链上是干净的。** 对于累加型铸币接口，先清零再铸造是唯一可靠方式。

---

## 坑 9：CTFExchange 的 collateral 地址是 immutable，无法替换

### 现象

尝试部署一个自定义 MockERC20 替换 MockUSDC 以规避余额累积问题，但 `matchOrders` 依然 revert，或资金流向仍然是原始 USDC 合约。

### 根因

`CTFExchange` 合约在部署时将抵押品（USDC）地址声明为 `immutable`：

```solidity
IERC20 public immutable collateral;
constructor(address _collateral, ...) {
    collateral = IERC20(_collateral);
}
```

BSC Testnet 上已部署的 CTFExchange 实例的 `collateral` 已固化为原始 ChildERC20 USDC 地址。所有内部 `transferFrom`、`transfer` 都指向该固化地址，**与我们新部署的 MockERC20 无关**。

### 解决方法

不要尝试替换 collateral，应使用坑 8 中的余额重置方案，保持使用原始 MockUSDC 合约。

### 教训

**复用已部署的第三方合约时，必须先阅读其构造函数，确认哪些参数是 `immutable`。** 无法通过换地址绕过的限制，只能从业务逻辑层面适配。

---

## 坑 10：BSC Testnet 交易后立即读链上状态返回滞后数据

### 现象

`matchOrders` 交易已确认（`Send` 返回），紧接着 `CallView` 读取余额，得到的是交易执行**之前**的旧数据。典型例子：

```
MINT matchOrders 成功
User1 USDC: 9500.00  ← 应为 9250.00（扣了 250 USDC 后的正确值）
```

USDC（ERC20）和 ERC1155 余额均可能出现此问题，ERC1155 尤为明显。

### 根因

BSC Testnet 节点对 `eth_call` 有读缓存，不一定立即反映最新区块。BSC 出块约 3 秒，`Send` 等到交易上链即返回，但缓存节点可能仍在服务旧区块的数据。

### 解决方法

在 `matchOrders` 成功后，打印余额快照前等待至少一个出块周期（4 秒）：

```go
ex.Send(ctx.Client, operatorAuth, ctx.ExchangeContract, "matchOrders", ...)
fmt.Println("✓ matchOrders 成功")
time.Sleep(4 * time.Second)  // 等待 RPC 缓存刷新
printBalances("步骤 X 完成后", ctx)
```

对于 ERC1155 余额，建议等 8 秒（约 2 个区块）以保证最终一致。

### 教训

**测试网 RPC 的读一致性不是即时的。** 不要假设写入（`Send`）返回后读取就能拿到最新数据。中间快照加等待，最终快照建议 8 秒，是在测试网上得到可靠读数的最简单方法。

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
| 7 | `CallView` 第二次调用 panic            | go-ethereum v1.13 复用非空 slice 写入旧类型 | 每次 CallView 使用新声明的空 slice          |
| 8 | 测试 USDC 余额跨轮次累积                  | ChildERC20 `deposit` 累加，无重置逻辑     | 铸币前先 transfer 回 deployer 再重新 deposit |
| 9 | 替换 MockERC20 后 matchOrders 仍 revert | CTFExchange collateral 是 immutable | 保持用原始 USDC，用余额重置方案               |
| 10 | 交易后立即读取余额得到旧数据                  | BSC Testnet RPC 读缓存滞后             | 每次 matchOrders 后 sleep 4-8 秒再读余额   |
