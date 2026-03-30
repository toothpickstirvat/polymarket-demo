package exchange

// shared_v3.go 提供基于 UMA OOv3 的公共初始化流程（步骤 1-5）。
//
// 与 shared.go（OOv2 版本）的主要差异：
//   - 步骤 1 只部署 MockOOv3 + UmaCtfAdapterV3（无 MockAddressWhitelist）
//   - 步骤 3 的 initialize() 参数更简洁（无 rewardToken / reward）
//   - 无 requestTime 和 identifier（OOv3 不需要时间戳和 identifier 定位请求）
//   - MarketContext.OOContract 绑定至 OoV3ABI
//   - MarketContext.AdapterContract 绑定至 AdapterV3ABI
//
// OOv3 流程概述：
//   proposeResolution(questionId, result) → adapter 内部调用 OO.assertTruth()
//   settle(questionId)                   → adapter 内部调用 OO.settleAssertion() → assertionResolvedCallback()
//   OO.disputeAssertion(assertionId, disputer)
//   OO.mockDvmResolve(assertionId, resolution)  → assertionResolvedCallback()

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// OoV3ABI 是 MockOptimisticOracleV3 的最小 ABI（Go 侧调用的部分）。
//
// 注意：assertTruth 和 settleAssertion 由 UmaCtfAdapterV3 内部调用，
// Go 侧只需要 disputeAssertion（质疑）和 mockDvmResolve（DVM 仲裁）。
const OoV3ABI = `[
  {"type":"constructor","inputs":[{"name":"_dvm","type":"address"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"disputeAssertion","inputs":[{"name":"assertionId","type":"bytes32"},{"name":"disputer","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"mockDvmResolve","inputs":[{"name":"assertionId","type":"bytes32"},{"name":"resolution","type":"bool"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"getAssertionResult","inputs":[{"name":"assertionId","type":"bytes32"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"}
]`

// AdapterV3ABI 是 UmaCtfAdapterV3 的最小 ABI。
//
// initialize() 只创建 CTF 条件，不发起 OO 请求（与 OOv2 的 AdapterABI 不同）。
// proposeResolution() 代替 OOv2 的 proposePrice()，调用 OO.assertTruth() 发起断言。
// settle() 触发 OO.settleAssertion()，进而回调 assertionResolvedCallback()。
// getAssertionId() 是 OOv3 特有的：质疑前需要用它取回 assertionId。
const AdapterV3ABI = `[
  {"type":"constructor","inputs":[{"name":"_ctf","type":"address"},{"name":"_oo","type":"address"},{"name":"_usdc","type":"address"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"initialize","inputs":[{"name":"ancillaryData","type":"bytes"},{"name":"proposalBond","type":"uint256"},{"name":"liveness","type":"uint64"}],"outputs":[{"name":"questionId","type":"bytes32"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"proposeResolution","inputs":[{"name":"questionId","type":"bytes32"},{"name":"result","type":"bool"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"settle","inputs":[{"name":"questionId","type":"bytes32"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"getConditionId","inputs":[{"name":"questionId","type":"bytes32"}],"outputs":[{"name":"","type":"bytes32"}],"stateMutability":"view"},
  {"type":"function","name":"getAssertionId","inputs":[{"name":"questionId","type":"bytes32"}],"outputs":[{"name":"","type":"bytes32"}],"stateMutability":"view"},
  {"type":"event","name":"QuestionInitialized","inputs":[{"name":"questionId","type":"bytes32","indexed":true},{"name":"ancillaryData","type":"bytes","indexed":false},{"name":"creator","type":"address","indexed":true}]}
]`

// RunCommonSetupV3 执行步骤 1-5（OOv3 版本），返回包含所有链上状态的 MarketContext。
//
// 与 RunCommonSetup（OOv2）的差异：
//   步骤 1：部署 MockOOv3 + UmaCtfAdapterV3（_ctf, _oo, _usdc）
//           无需部署 MockAddressWhitelist（OOv3 不需要抵押品白名单）
//   步骤 3：adapter.initialize(ancillaryData, proposalBond, liveness)
//           无 rewardToken / reward 参数；不发起 OO 价格请求
//   返回值：MarketContext.OOContract 绑定至 OoV3ABI
//           MarketContext.AdapterContract 绑定至 AdapterV3ABI
//           MarketContext.RequestTime / Identifier 为零值（OOv3 不使用）
//
// 步骤 2/4/5 与 OOv2 版本完全相同（铸造 USDC、拆分头寸、订单撮合）。
func RunCommonSetupV3(configPath string) *MarketContext {
	cfg := LoadConfig(configPath)
	fmt.Printf("✓ 配置已加载（OOv3 模式）: %s\n", configPath)

	rpcURL := cfg.RPCURL
	if cfg.LocalMode {
		rpcURL = cfg.LocalRPCURL
	}
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		log.Fatalf("连接节点失败: %v", err)
	}
	chainID, _ := client.ChainID(context.Background())
	fmt.Printf("✓ 已连接: %s  Chain ID: %s\n", rpcURL, chainID)

	deployerKey := MustParseKey(cfg.Accounts.DeployerPrivateKey)
	user1Key := MustParseKey(cfg.Accounts.User1PrivateKey)
	user2Key := MustParseKey(cfg.Accounts.User2PrivateKey)
	operatorKey := MustParseKey(cfg.Accounts.OperatorPrivateKey)

	deployerAddr := crypto.PubkeyToAddress(deployerKey.PublicKey)
	user1Addr := crypto.PubkeyToAddress(user1Key.PublicKey)
	user2Addr := crypto.PubkeyToAddress(user2Key.PublicKey)
	operatorAddr := crypto.PubkeyToAddress(operatorKey.PublicKey)

	fmt.Printf("  Deployer/DVM:  %s\n", deployerAddr.Hex())
	fmt.Printf("  User1 (买YES): %s\n", user1Addr.Hex())
	fmt.Printf("  User2 (买NO):  %s\n", user2Addr.Hex())
	fmt.Printf("  Operator:      %s\n", operatorAddr.Hex())

	GasCfg.PriceWei = new(big.Int).Mul(big.NewInt(cfg.Gas.PriceGwei), big.NewInt(1e9))
	GasCfg.Limit = cfg.Gas.Limit

	ctfAddr := common.HexToAddress(cfg.Contracts.CTF)
	exchangeAddr := common.HexToAddress(cfg.Contracts.Exchange)
	usdcAddr := common.HexToAddress(cfg.Contracts.USDC)

	ctfABIParsed := MustParseABI(CtfABI)
	erc20ABIParsed := MustParseABI(Erc20ABI)
	ooV3ABIParsed := MustParseABI(OoV3ABI)
	adapterV3ABIParsed := MustParseABI(AdapterV3ABI)
	exchangeABIParsed := MustParseABI(ExchangeABI)

	ctfContract := bind.NewBoundContract(ctfAddr, ctfABIParsed, client, client, client)
	usdcContract := bind.NewBoundContract(usdcAddr, erc20ABIParsed, client, client, client)
	exchangeContract := bind.NewBoundContract(exchangeAddr, exchangeABIParsed, client, client, client)

	// ── 步骤 1：部署 MockOOv3 + UmaCtfAdapterV3 ─────────────────────────────
	// OOv3 版本不需要 MockAddressWhitelist（OOv3 不依赖抵押品白名单合约）。
	// UmaCtfAdapterV3 构造函数：(_ctf, _oo, _usdc)
	//   _usdc 作为 bond 货币直接传给 OOv3.assertTruth()，无需白名单校验。
	Div("步骤 1: 部署 MockOOv3 / UmaCtfAdapterV3（OOv3 版本）")
	deployerAuth := NewAuth(client, deployerKey)

	oo3Bytecode, loadErr := LoadBytecode("MockOOv3", "MockOptimisticOracleV3")
	if loadErr != nil {
		log.Fatal(loadErr)
	}
	// MockOOv3 构造函数接收 _dvm 地址，这里用 deployer 充当 DVM
	oo3Addr, oo3Contract := Deploy(client, deployerAuth, ooV3ABIParsed, oo3Bytecode, deployerAddr)
	fmt.Printf("✓ MockOOv3:          %s\n", oo3Addr.Hex())

	adapterV3Bytecode, loadErr := LoadBytecode("UmaCtfAdapterV3", "UmaCtfAdapterV3")
	if loadErr != nil {
		log.Fatal(loadErr)
	}
	// UmaCtfAdapterV3 构造函数：(_ctf, _oo, _usdc)
	adapterV3Addr, adapterV3Contract := Deploy(client, deployerAuth, adapterV3ABIParsed, adapterV3Bytecode,
		ctfAddr, oo3Addr, usdcAddr)
	fmt.Printf("✓ UmaCtfAdapterV3:   %s\n", adapterV3Addr.Hex())

	// 检查 operator 权限（与 OOv2 相同）
	var opCheck []interface{}
	CallView(exchangeContract, "isOperator", &opCheck, operatorAddr)
	if !opCheck[0].(bool) {
		Send(client, deployerAuth, exchangeContract, "addOperator", operatorAddr)
		fmt.Printf("✓ 已注册 operator: %s\n", operatorAddr.Hex())
	} else {
		fmt.Println("✓ operator 权限已存在")
	}

	// ── 步骤 2：铸造 USDC（与 OOv2 版本完全相同）────────────────────────────
	Div("步骤 2: 铸造测试 USDC（先清零存量，再 deposit 10000）")
	mintAmount := ToUsdc(10000)
	userKeys := map[common.Address]*ecdsa.PrivateKey{
		user1Addr: user1Key,
		user2Addr: user2Key,
	}
	for _, info := range []struct {
		addr common.Address
		name string
	}{{user1Addr, "User1"}, {user2Addr, "User2"}} {
		var existing []interface{}
		CallView(usdcContract, "balanceOf", &existing, info.addr)
		if bal := existing[0].(*big.Int); bal.Sign() > 0 {
			Send(client, NewAuth(client, userKeys[info.addr]), usdcContract, "transfer", deployerAddr, bal)
			fmt.Printf("  %s 退回存量 %.2f USDC → deployer\n", info.name, FromUsdc(bal))
		}
		depositData := make([]byte, 32)
		mintAmount.FillBytes(depositData)
		Send(client, deployerAuth, usdcContract, "deposit", info.addr, depositData)
		var bal []interface{}
		CallView(usdcContract, "balanceOf", &bal, info.addr)
		fmt.Printf("✓ %s USDC 余额: %.2f\n", info.name, FromUsdc(bal[0].(*big.Int)))
	}

	// ── 步骤 3：初始化市场（OOv3 版本）─────────────────────────────────────
	// OOv2：initialize(ancillaryData, rewardToken, reward, proposalBond, liveness)
	//        → CTF.prepareCondition() + OO.requestPrice()
	//
	// OOv3：initialize(ancillaryData, proposalBond, liveness)
	//        → 只调用 CTF.prepareCondition()；不发起 OO 请求
	//        → OO 请求延迟到 proposeResolution() 时发起（OO.assertTruth）
	Div("步骤 3: 初始化市场（adapterV3.initialize，OOv3 版本）")
	ancillaryData := []byte(cfg.Market.AncillaryData)
	proposalBond := ToUsdc(cfg.Market.ProposalBondUSDC)
	liveness := cfg.Market.LivenessSeconds

	fmt.Printf("  问题原文:     %s\n", cfg.Market.AncillaryData)
	fmt.Printf("  proposalBond: %.2f USDC\n", cfg.Market.ProposalBondUSDC)
	fmt.Printf("  liveness:     %d 秒\n", liveness)

	receipt := Send(client, deployerAuth, adapterV3Contract, "initialize",
		ancillaryData, proposalBond, uint64(liveness))

	// questionId 从 QuestionInitialized 事件的第一个 indexed 参数提取
	questionId := ExtractBytes32FromReceipt(receipt, adapterV3ABIParsed, "QuestionInitialized", 0)
	fmt.Printf("✓ questionId:  0x%x\n", questionId)

	// OOv3 无 requestTime（不发起 OO 请求），MarketContext.RequestTime 留为 nil

	// 从 adapter 查询 conditionId
	var condResult []interface{}
	CallView(adapterV3Contract, "getConditionId", &condResult, questionId)
	conditionId := condResult[0].([32]byte)
	fmt.Printf("✓ conditionId: 0x%x\n", conditionId)

	// 计算 YES/NO 的 ERC1155 tokenId（与 OOv2 相同，CTF 逻辑不变）
	var yesColResult, noColResult []interface{}
	CallView(ctfContract, "getCollectionId", &yesColResult, [32]byte{}, conditionId, big.NewInt(1))
	CallView(ctfContract, "getCollectionId", &noColResult, [32]byte{}, conditionId, big.NewInt(2))
	var yesPosResult, noPosResult []interface{}
	CallView(ctfContract, "getPositionId", &yesPosResult, usdcAddr, yesColResult[0].([32]byte))
	CallView(ctfContract, "getPositionId", &noPosResult, usdcAddr, noColResult[0].([32]byte))
	yesTokenId := yesPosResult[0].(*big.Int)
	noTokenId := noPosResult[0].(*big.Int)
	fmt.Printf("✓ YES tokenId: %s\n", yesTokenId)
	fmt.Printf("✓ NO  tokenId: %s\n", noTokenId)

	// 注册代币对（必须在 matchOrders 前调用，否则静默 revert）
	var isAdminResult []interface{}
	CallView(exchangeContract, "isAdmin", &isAdminResult, deployerAddr)
	if isAdminResult[0].(bool) {
		var condBytes [32]byte
		copy(condBytes[:], conditionId[:])
		Send(client, NewAuth(client, deployerKey), exchangeContract, "registerToken",
			yesTokenId, noTokenId, condBytes)
		fmt.Println("✓ registerToken 完成")
	}

	// ── 步骤 4：拆分头寸（与 OOv2 版本完全相同）────────────────────────────
	Div("步骤 4: 拆分 USDC → YES/NO 代币（CTF.splitPosition）")
	splitAmount := ToUsdc(1000)
	partition := []*big.Int{big.NewInt(1), big.NewInt(2)}

	for _, info := range []struct {
		addr common.Address
		name string
		key  *ecdsa.PrivateKey
	}{{user1Addr, "User1", user1Key}, {user2Addr, "User2", user2Key}} {
		auth := NewAuth(client, info.key)
		Send(client, auth, usdcContract, "approve", ctfAddr, splitAmount)
		Send(client, auth, ctfContract, "setApprovalForAll", exchangeAddr, true)
		Send(client, auth, ctfContract, "splitPosition",
			usdcAddr, [32]byte{}, conditionId, partition, splitAmount)
		var yBal, nBal []interface{}
		CallView(ctfContract, "balanceOf", &yBal, info.addr, yesTokenId)
		CallView(ctfContract, "balanceOf", &nBal, info.addr, noTokenId)
		fmt.Printf("✓ %s: YES=%s  NO=%s\n", info.name, yBal[0].(*big.Int), nBal[0].(*big.Int))
	}

	// ── 步骤 5：订单撮合（与 OOv2 版本完全相同）────────────────────────────
	Div("步骤 5: 订单撮合（CTFExchange.matchOrders）")
	fmt.Println("  User1 SELL 1000 NO @ 0.5 USDC，User2 BUY 1000 NO @ 0.5 USDC")

	Send(client, NewAuth(client, user2Key), usdcContract, "approve", exchangeAddr, ToUsdc(500))

	tradeAmount := splitAmount
	usdcAmount := ToUsdc(500)
	expiry := big.NewInt(time.Now().Unix() + 3600)

	var u1Nonce, u2Nonce []interface{}
	CallView(exchangeContract, "nonces", &u1Nonce, user1Addr)
	CallView(exchangeContract, "nonces", &u2Nonce, user2Addr)

	makerOrder := &CTFOrder{
		Salt: big.NewInt(rand.Int63()), Maker: user1Addr, Signer: user1Addr,
		Taker: common.Address{}, TokenId: noTokenId,
		MakerAmount: tradeAmount, TakerAmount: usdcAmount,
		Expiration: expiry, Nonce: u1Nonce[0].(*big.Int),
		FeeRateBps: big.NewInt(0), Side: 1, SignatureType: 0,
	}
	if err := SignOrder(makerOrder, user1Key, chainID, exchangeAddr); err != nil {
		log.Fatalf("签名 makerOrder: %v", err)
	}

	takerOrder := &CTFOrder{
		Salt: big.NewInt(rand.Int63()), Maker: user2Addr, Signer: user2Addr,
		Taker: common.Address{}, TokenId: noTokenId,
		MakerAmount: usdcAmount, TakerAmount: tradeAmount,
		Expiration: expiry, Nonce: u2Nonce[0].(*big.Int),
		FeeRateBps: big.NewInt(0), Side: 0, SignatureType: 0,
	}
	if err := SignOrder(takerOrder, user2Key, chainID, exchangeAddr); err != nil {
		log.Fatalf("签名 takerOrder: %v", err)
	}

	Send(client, NewAuth(client, operatorKey), exchangeContract, "matchOrders",
		ToOrderTuple(takerOrder),
		[]OrderTuple{ToOrderTuple(makerOrder)},
		usdcAmount,
		[]*big.Int{tradeAmount},
	)
	fmt.Println("✓ matchOrders 成功")

	for _, info := range []struct {
		addr common.Address
		name string
	}{{user1Addr, "User1"}, {user2Addr, "User2"}} {
		var yBal, nBal, uBal []interface{}
		CallView(ctfContract, "balanceOf", &yBal, info.addr, yesTokenId)
		CallView(ctfContract, "balanceOf", &nBal, info.addr, noTokenId)
		CallView(usdcContract, "balanceOf", &uBal, info.addr)
		fmt.Printf("  %s: YES=%s  NO=%s  USDC=%.2f\n",
			info.name, yBal[0].(*big.Int), nBal[0].(*big.Int), FromUsdc(uBal[0].(*big.Int)))
	}

	return &MarketContext{
		Client: client, ChainID: chainID, Cfg: cfg,
		DeployerKey: deployerKey, User1Key: user1Key,
		User2Key: user2Key, OperatorKey: operatorKey,
		DeployerAddr: deployerAddr, User1Addr: user1Addr,
		User2Addr: user2Addr, OperatorAddr: operatorAddr,
		CTFAddr: ctfAddr, ExchangeAddr: exchangeAddr, USDCAddr: usdcAddr,
		OOAddr: oo3Addr, AdapterAddr: adapterV3Addr,
		CTFContract: ctfContract, USDCContract: usdcContract,
		ExchangeContract: exchangeContract, OOContract: oo3Contract,
		AdapterContract: adapterV3Contract,
		QuestionId:      questionId, ConditionId: conditionId,
		// RequestTime=nil, Identifier=零值：OOv3 不使用时间戳和 identifier 定位请求
		AncillaryData: ancillaryData, ProposalBond: proposalBond,
		YesTokenId: yesTokenId, NoTokenId: noTokenId,
	}
}

