// normal 演示 Polymarket 预测市场的正常结算流程（无争议）。
//
// 场景：
//
//	提案方正确地提案 YES 赢 → 等待 liveness（无人质疑）→ settle → resolve → 赎回
//
// 流程：
//
//	步骤 1-5（公共）：部署合约、铸造 USDC、初始化市场、拆分头寸、撮合订单
//	步骤 6：deployer 提案 YES 赢（price=1e18）
//	步骤 7：等待 liveness 结束（120 秒，无质疑）
//	步骤 8：OO.settle → adapter.resolve → CTF.reportPayouts([1e18, 0])
//	步骤 9：User1 赎回 YES → +1000 USDC；User2 赎回 NO → +0 USDC
//
// 运行（从 exchange/ 目录）：
//
//	go run ./cmd/normal
//	go run ./cmd/normal -config config.json
package main

import (
	"flag"
	"fmt"
	"math/big"

	ex "polymarket-exchange"
)

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	// 步骤 1-5：部署合约、铸造 USDC、初始化市场、拆分头寸、撮合订单
	// 执行后状态：
	//   User1: 1000 YES + 0 NO + ~9500 USDC（卖出了 1000 NO，收到 500 USDC）
	//   User2: 1000 YES + 2000 NO + ~8500 USDC（买入了 1000 NO，花了 500 USDC）
	ctx := ex.RunCommonSetup(*configPath)

	// ── 步骤 6：提案正确结果（YES 赢）────────────────────────────────────────
	// proposePrice 参数说明：
	//   requester     = adapter 地址（OOv2 通过 requester+identifier+timestamp+ancillaryData 定位请求）
	//   identifier    = "YES_OR_NO_QUERY"（32 字节，右填充零）
	//   timestamp     = requestTime（initialize 区块的时间戳，即 T1）
	//   ancillaryData = 问题描述字节
	//   proposedPrice = 1e18（表示 YES 赢；0 表示 NO 赢；0.5e18 表示平局）
	//
	// 提案者需要事先 approve OO 花费 bond 金额的 USDC（bond 在 liveness 结束后归还）。
	ex.Div("步骤 6: 提案市场结果（OOv2.proposePrice → YES 赢）")

	deployerAuth := ex.NewAuth(ctx.Client, ctx.DeployerKey)
	ex.Send(ctx.Client, deployerAuth, ctx.USDCContract, "approve", ctx.OOAddr, ctx.ProposalBond)

	yesPrice := big.NewInt(1e18) // 1e18 = YES 赢（UmaCtfAdapter 的约定）
	ex.Send(ctx.Client, deployerAuth, ctx.OOContract, "proposePrice",
		ctx.AdapterAddr, ctx.Identifier, ctx.RequestTime, ctx.AncillaryData, yesPrice)
	fmt.Printf("✓ 已提案 YES（price=1e18），liveness=%d 秒\n", ctx.Cfg.Market.LivenessSeconds)

	// ── 步骤 7：等待 liveness 结束（无人质疑）────────────────────────────────
	// liveness 窗口期间，任何人可以调用 OO.disputePrice() 质疑提案。
	// 本演示中无人质疑，等待结束后提案自动生效。
	// 本地模式用 evm_increaseTime 瞬间推进；测试网则真实等待。
	ex.Div("步骤 7: 等待 liveness 结束")
	ex.WaitLiveness(ctx.Client, ctx.Cfg)

	// ── 步骤 8：结算 ──────────────────────────────────────────────────────────
	// 结算分两步：
	//  8a. OO.settle()：将价格从 Proposed 状态转为 Resolved，返还 bond 给提案者
	//  8b. adapter.resolve()：从 OO 读取价格，转换为 payouts 数组，调用 CTF.reportPayouts()
	//
	// OOv2 价格 → payouts 转换（在 UmaCtfAdapter._constructPayouts() 中）：
	//   price = 1e18 → payouts = [1e18, 0]    → YES 赢（index 0 全额，index 1 清零）
	//   price = 0    → payouts = [0, 1e18]    → NO  赢
	//   price = 0.5e18 → payouts = [5e17, 5e17] → 平局
	//   其他值        → payouts = [0, 0]       → 无效市场
	ex.Div("步骤 8: 结算（OO.settle → adapter.resolve → YES 赢）")

	// OO.settle：liveness 结束后任何人都可以调用，这里用 deployer
	ex.Send(ctx.Client, deployerAuth, ctx.OOContract, "settle",
		ctx.AdapterAddr, ctx.Identifier, ctx.RequestTime, ctx.AncillaryData)
	fmt.Println("✓ OO.settle 完成")

	// adapter.resolve：内部调用 OO.getPrice() → CTF.reportPayouts([1e18, 0])
	ex.Send(ctx.Client, deployerAuth, ctx.AdapterContract, "resolve", ctx.QuestionId)
	fmt.Println("✓ adapter.resolve 完成 → reportPayouts([1e18, 0]) → YES 赢！")

	// ── 步骤 9：赎回 ──────────────────────────────────────────────────────────
	// redeemPositions 在 conditionId 结算后，用条件代币换回 USDC。
	// indexSets 参数是位掩码（与 splitPosition 时的 partition 对应）：
	//   indexSets=[1] → 赎回 YES（indexSet=1=0b01）
	//   indexSets=[2] → 赎回 NO （indexSet=2=0b10）
	//
	// 结算结果 payouts=[1e18, 0]：
	//   YES（index 0）面值 = 1e18 / (1e18+0) = 100% → 每个 YES token 换 1 USDC
	//   NO（index 1）面值 = 0 / (1e18+0)   = 0%   → NO token 一文不值
	ex.Div("步骤 9: 赎回（CTF.redeemPositions）")

	// User1 持有 1000 YES → YES 赢，每个兑换 1 USDC，共 +1000 USDC
	var u1Before []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u1Before, ctx.User1Addr)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User1Key), ctx.CTFContract,
		"redeemPositions", ctx.USDCAddr, [32]byte{}, ctx.ConditionId, []*big.Int{big.NewInt(1)})
	var u1After []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u1After, ctx.User1Addr)
	fmt.Printf("✓ User1 赎回 YES: %.2f → %.2f USDC（+%.2f）\n",
		ex.FromUsdc(u1Before[0].(*big.Int)),
		ex.FromUsdc(u1After[0].(*big.Int)),
		ex.FromUsdc(u1After[0].(*big.Int))-ex.FromUsdc(u1Before[0].(*big.Int)))

	// User2 持有 2000 NO → YES 赢，NO 归零，赎回 0
	var u2Before []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2Before, ctx.User2Addr)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User2Key), ctx.CTFContract,
		"redeemPositions", ctx.USDCAddr, [32]byte{}, ctx.ConditionId, []*big.Int{big.NewInt(2)})
	var u2After []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2After, ctx.User2Addr)
	fmt.Printf("✓ User2 赎回 NO:  %.2f → %.2f USDC（YES 赢，NO 归零）\n",
		ex.FromUsdc(u2Before[0].(*big.Int)),
		ex.FromUsdc(u2After[0].(*big.Int)))

	ex.Div("正常结算流程演示完成！")
	fmt.Printf("\n部署的合约：\n")
	fmt.Printf("  MockOOv2     : %s\n", ctx.OOAddr.Hex())
	fmt.Printf("  UmaCtfAdapter: %s\n", ctx.AdapterAddr.Hex())
}
