// v3/normal 演示基于 UMA OOv3 的预测市场正常结算流程（无争议）。
//
// 场景：
//
//	提案方正确提案 YES 赢 → 等待 liveness（无人质疑）→ settle → YES 赢
//
// 与 OOv2 版本（cmd/normal）的关键差异：
//   - 步骤 6：调用 adapter.proposeResolution(questionId, true)
//             代替 OOv2 的 OO.proposePrice(...)
//             adapter 内部调用 OO.assertTruth()，返回 assertionId
//   - 步骤 8：调用 adapter.settle(questionId)
//             adapter 内部调用 OO.settleAssertion()
//             → assertionResolvedCallback(assertedTruthfully=true)
//             → CTF.reportPayouts([1e18, 0])
//   - 无 requestTime / identifier：OOv3 不需要时间戳和 identifier 定位请求
//   - 无 adapter.resolve()：OOv3 通过回调机制自动解析，不需要单独调用
//
// 流程：
//
//	步骤 1-5（公共）：部署合约、铸造 USDC、初始化市场、拆分头寸、撮合订单
//	步骤 6：deployer approve USDC 给 adapter → adapter.proposeResolution(questionId, true)
//	步骤 7：等待 liveness 结束（无质疑）
//	步骤 8：adapter.settle → OO.settleAssertion → assertionResolvedCallback → YES 赢
//	步骤 9：User1 赎回 YES → +1000 USDC；User2 赎回 NO → +0 USDC
//
// 运行（从 exchange/ 目录）：
//
//	go run ./cmd/v3/normal
//	go run ./cmd/v3/normal -config config.json
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

	// 步骤 1-5：OOv3 版本公共初始化（部署 MockOOv3 + UmaCtfAdapterV3，其余与 v2 相同）
	// 执行后状态：
	//   User1: 1000 YES + 0 NO + ~9500 USDC
	//   User2: 1000 YES + 2000 NO + ~8500 USDC
	ctx := ex.RunCommonSetupV3(*configPath)

	// ── 步骤 6：提案正确结果（YES 赢）────────────────────────────────────────
	// OOv3 与 OOv2 的最大流程差异在这里：
	//   OOv2：提案者直接调用 OO.proposePrice(requester, identifier, timestamp, ancillaryData, price)
	//   OOv3：提案者调用 adapter.proposeResolution(questionId, result)
	//         adapter 内部将 bond 转入 OO，再以 adapter 自身作为 asserter 调用 OO.assertTruth()
	//
	// 提案前：msg.sender（deployer）须 approve adapter 花费 proposalBond 数量的 USDC。
	// adapter 收到后，再 approve 并转给 OO 作为断言保证金。
	ex.Div("步骤 6: 提案市场结果（adapterV3.proposeResolution → YES 赢）")

	deployerAuth := ex.NewAuth(ctx.Client, ctx.DeployerKey)

	// deployer approve adapter 花费 bond
	ex.Send(ctx.Client, deployerAuth, ctx.USDCContract, "approve", ctx.AdapterAddr, ctx.ProposalBond)

	// 提案 YES（result=true）
	ex.Send(ctx.Client, deployerAuth, ctx.AdapterContract, "proposeResolution",
		ctx.QuestionId, true)
	fmt.Printf("✓ 已提案 YES（result=true），liveness=%d 秒\n", ctx.Cfg.Market.LivenessSeconds)
	fmt.Printf("  proposalBond: %.2f USDC（由 adapter 锁入 OOv3）\n",
		ex.FromUsdc(ctx.ProposalBond))

	// ── 步骤 7：等待 liveness 结束（无人质疑）────────────────────────────────
	// OOv3 的 liveness 机制与 OOv2 相同：
	//   liveness 窗口内任何人可调用 OO.disputeAssertion(assertionId, disputer) 质疑。
	//   无人质疑时，liveness 结束后断言自动生效（assertedTruthfully=true）。
	// 本演示中无人质疑，直接等待结束。
	ex.Div("步骤 7: 等待 liveness 结束")
	ex.WaitLiveness(ctx.Client, ctx.Cfg)

	// ── 步骤 8：结算 ──────────────────────────────────────────────────────────
	// OOv3 与 OOv2 的结算方式差异：
	//   OOv2：需要两步——OO.settle(...)  + adapter.resolve(questionId)
	//         OO.settle 将价格状态改为 Resolved，返还 bond；
	//         adapter.resolve 读取价格，调用 CTF.reportPayouts()。
	//
	//   OOv3：只需一步——adapter.settle(questionId)
	//         adapter.settle 内部调用 OO.settleAssertion(assertionId)；
	//         OO 在 settleAssertion 中直接调用回调 assertionResolvedCallback(assertionId, true)；
	//         assertionResolvedCallback 调用 CTF.reportPayouts([1e18, 0])。
	//         整个结算在一次调用中完成（通过回调链路驱动）。
	ex.Div("步骤 8: 结算（adapter.settle → OO.settleAssertion → assertionResolvedCallback → YES 赢）")

	ex.Send(ctx.Client, deployerAuth, ctx.AdapterContract, "settle", ctx.QuestionId)
	fmt.Println("✓ adapter.settle 完成")
	fmt.Println("  → OO.settleAssertion 触发 assertionResolvedCallback(assertedTruthfully=true)")
	fmt.Println("  → assertionResolvedCallback: finalResult=true → reportPayouts([1e18, 0]) → YES 赢！")
	fmt.Printf("  → bond 已退还给 proposer（deployer）: %.2f USDC\n",
		ex.FromUsdc(ctx.ProposalBond))

	// ── 步骤 9：赎回 ──────────────────────────────────────────────────────────
	// 结算结果 payouts=[1e18, 0]（与 OOv2 正常结算完全相同）：
	//   YES（index 0）面值 = 1e18 / 1e18 = 100% → 每个 YES token 换 1 USDC
	//   NO（index 1）面值 = 0             = 0%   → NO token 一文不值
	ex.Div("步骤 9: 赎回（CTF.redeemPositions）")

	// User1 持有 1000 YES → YES 赢，+1000 USDC
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

	// User2 持有 2000 NO → YES 赢，NO 归零
	var u2Before []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2Before, ctx.User2Addr)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User2Key), ctx.CTFContract,
		"redeemPositions", ctx.USDCAddr, [32]byte{}, ctx.ConditionId, []*big.Int{big.NewInt(2)})
	var u2After []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2After, ctx.User2Addr)
	fmt.Printf("✓ User2 赎回 NO:  %.2f → %.2f USDC（YES 赢，NO 归零）\n",
		ex.FromUsdc(u2Before[0].(*big.Int)),
		ex.FromUsdc(u2After[0].(*big.Int)))

	ex.Div("OOv3 正常结算流程演示完成！")
	fmt.Println("\n部署的合约：")
	fmt.Printf("  MockOOv3:          %s\n", ctx.OOAddr.Hex())
	fmt.Printf("  UmaCtfAdapterV3:   %s\n", ctx.AdapterAddr.Hex())
}
