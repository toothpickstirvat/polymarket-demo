// v3/dispute 演示基于 UMA OOv3 的预测市场争议处理流程。
//
// 场景：
//
//	提案方错误提案 YES → User2 发起质疑 → DVM 裁定提案错误 → NO 赢（无需二次提案）
//
// 与 OOv2 版本（cmd/dispute）的关键差异：
//   OOv2 争议流程（7 步）：
//     错误提案 → disputePrice → priceDisputed 回调（requestTime 更新到 T2）→
//     mockDvmSettle(T1, false) → 重新提案(T2, 正确) → 等待 liveness → settle(T2)
//
//   OOv3 争议流程（4 步，更简洁）：
//     错误提案（assertTruth）→ disputeAssertion → mockDvmResolve(false)
//     → assertionResolvedCallback(false) → 结果自动取反 → NO 赢！
//     无需重新提案，无需二次 liveness 等待，DVM 直接触发回调结算。
//
// OOv3 争议后 bond 流向：
//   - 提案者（deployer）：损失 proposalBond（由 adapter 锁入 OO）
//   - 质疑者（User2）：获得 2x bond（自己的 bond + 提案者的 bond）
//   - 无协议费（Mock 简化）
//
// 流程：
//
//	步骤 1-5（公共）：部署合约、铸造 USDC、初始化市场、拆分头寸、撮合订单
//	步骤 6：deployer 提案 YES（错误），adapter 调用 OO.assertTruth()
//	步骤 7：User2 质疑（OO.disputeAssertion）→ assertionDisputedCallback（状态记录）
//	步骤 8：DVM 裁定（OO.mockDvmResolve, resolution=false）
//	        → assertionResolvedCallback(false) → 结果取反 → NO 赢！
//	步骤 9：User1 赎回 YES → +0；User2 赎回 NO → +2000 USDC
//
// 运行（从 exchange/ 目录）：
//
//	go run ./cmd/v3/dispute
//	go run ./cmd/v3/dispute -config config.json
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

	// 步骤 1-5：OOv3 版本公共初始化
	// 执行后状态：
	//   User1: 1000 YES + 0 NO + ~9500 USDC
	//   User2: 1000 YES + 2000 NO + ~8500 USDC
	ctx := ex.RunCommonSetupV3(*configPath)

	// ── 步骤 6：错误提案（deployer 提案 YES，但实际结果应为 NO）────────────
	// deployer approve adapter → adapter.proposeResolution(questionId, true)
	// adapter 内部：usdc.transferFrom(deployer→adapter) → usdc.approve(oo) → OO.assertTruth(...)
	// OO 生成并存储 assertionId，供后续 disputeAssertion 使用。
	ex.Div("步骤 6: 提案错误结果（adapterV3.proposeResolution → YES，但这是错的）")

	deployerAuth := ex.NewAuth(ctx.Client, ctx.DeployerKey)
	ex.Send(ctx.Client, deployerAuth, ctx.USDCContract, "approve", ctx.AdapterAddr, ctx.ProposalBond)
	ex.Send(ctx.Client, deployerAuth, ctx.AdapterContract, "proposeResolution",
		ctx.QuestionId, true)
	fmt.Printf("✓ Proposer（deployer）提案 YES（result=true）\n")
	fmt.Printf("  → bond %.2f USDC 已由 adapter 锁入 OOv3\n",
		ex.FromUsdc(ctx.ProposalBond))

	// 记录 User2 质疑前的 USDC 余额（用于计算 DVM 奖励）
	var u2BalBefore []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2BalBefore, ctx.User2Addr)

	// 从 adapter 读取 assertionId（质疑时需要）
	// OOv3 与 OOv2 的关键区别：
	//   OOv2 通过 (requester, identifier, timestamp, ancillaryData) 四元组定位请求
	//   OOv3 通过 assertionId（一个随机 bytes32）定位请求，更简洁
	var assertionIdResult []interface{}
	ex.CallView(ctx.AdapterContract, "getAssertionId", &assertionIdResult, ctx.QuestionId)
	assertionId := assertionIdResult[0].([32]byte)
	fmt.Printf("✓ assertionId: 0x%x\n", assertionId)

	// ── 步骤 7：User2 质疑提案 ────────────────────────────────────────────────
	// OOv2：disputePrice(requester, identifier, timestamp, ancillaryData)
	//        → priceDisputed 回调 → adapter 重置 requestTime 为 T2，发起新 requestPrice
	//
	// OOv3：disputeAssertion(assertionId, disputer)
	//        → assertionDisputedCallback(assertionId)（仅记录事件，等待 DVM）
	//        → 无需重置 requestTime，无需新建请求
	//
	// 质疑前：User2 须 approve OO 花费 proposalBond 数量的 USDC（作为反向保证金）。
	ex.Div("步骤 7: User2 质疑提案（OO.disputeAssertion）")
	fmt.Println("  User2 持有 NO tokens，认为提案 YES 是错误的，发起质疑")

	user2Auth := ex.NewAuth(ctx.Client, ctx.User2Key)
	// User2 approve OO（不是 adapter！）花费 bond
	ex.Send(ctx.Client, user2Auth, ctx.USDCContract, "approve", ctx.OOAddr, ctx.ProposalBond)
	ex.Send(ctx.Client, user2Auth, ctx.OOContract, "disputeAssertion",
		assertionId, ctx.User2Addr)
	fmt.Printf("✓ User2 质疑成功，质押 bond: %.2f USDC → OOv3\n",
		ex.FromUsdc(ctx.ProposalBond))
	fmt.Println("  → OOv3 调用 adapter.assertionDisputedCallback()（事件记录）")
	fmt.Println("  → OOv3 内部：断言状态 = Disputed，等待 DVM 裁定")
	fmt.Println("  ★ 注意：OOv3 无需重置 requestTime，也无需二次提案（与 OOv2 最大的流程差异）")

	// ── 步骤 8：DVM 裁定 ────────────────────────────────────────────────────
	// OOv2：mockDvmSettle(requester, identifier, timestamp_T1, ancillaryData, resolution=false)
	//        → disputer 获得 2x bond；T1 请求终结；T2 请求仍在 Requested 状态，
	//          需要再次 proposePrice(T2) + 等待 liveness + settle(T2) 才能最终解析
	//
	// OOv3：mockDvmResolve(assertionId, resolution=false)
	//        → disputer 直接获得 2x bond
	//        → OO 调用 adapter.assertionResolvedCallback(assertionId, assertedTruthfully=false)
	//        → assertionResolvedCallback 中：
	//            finalResult = assertedTruthfully(false) ? proposedResult(true) : !proposedResult(true)
	//                        = false ? true : false = false（NO 赢）
	//        → CTF.reportPayouts([0, 1e18]) → 市场解析完成！
	//        全程只需一次 mockDvmResolve 调用，无需后续 settle 步骤。
	ex.Div("步骤 8: DVM 裁定（OO.mockDvmResolve, resolution=false → NO 赢）")
	fmt.Println("  DVM 确认：原提案 YES 是错误的，质疑者（User2）胜")

	ex.Send(ctx.Client, deployerAuth, ctx.OOContract, "mockDvmResolve",
		assertionId, false)
	fmt.Println("✓ mockDvmResolve(resolution=false) 完成")
	fmt.Println("  → OO 向 disputer（User2）发送 2x bond")
	fmt.Println("  → OO 调用 assertionResolvedCallback(assertedTruthfully=false)")
	fmt.Println("  → assertionResolvedCallback: finalResult = !true = false → NO 赢！")
	fmt.Println("  → CTF.reportPayouts([0, 1e18]) 已调用，市场解析完成")

	var u2BalAfterDvm []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2BalAfterDvm, ctx.User2Addr)
	dvmGain := new(big.Int).Sub(u2BalAfterDvm[0].(*big.Int), u2BalBefore[0].(*big.Int))
	fmt.Printf("  User2 DVM 奖励收益: %+.2f USDC（2x bond）\n", ex.FromUsdc(dvmGain))

	// ── 步骤 9：赎回 ──────────────────────────────────────────────────────────
	// 结算结果 payouts=[0, 1e18]：
	//   YES（index 0）面值 = 0    → YES token 一文不值
	//   NO（index 1）面值 = 1e18 → 每个 NO token 换 1 USDC
	//
	// 与 OOv2 dispute 流程完全相同（赎回部分不受 oracle 版本影响）。
	ex.Div("步骤 9: 赎回（CTF.redeemPositions）")

	// User1 持有 1000 YES → NO 赢，YES 归零
	var u1Before []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u1Before, ctx.User1Addr)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User1Key), ctx.CTFContract,
		"redeemPositions", ctx.USDCAddr, [32]byte{}, ctx.ConditionId, []*big.Int{big.NewInt(1)})
	var u1After []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u1After, ctx.User1Addr)
	fmt.Printf("✓ User1 赎回 YES: %.2f → %.2f USDC（NO 赢，YES 归零）\n",
		ex.FromUsdc(u1Before[0].(*big.Int)),
		ex.FromUsdc(u1After[0].(*big.Int)))

	// User2 持有 2000 NO → NO 赢，每个兑换 1 USDC，共 +2000 USDC
	var u2Before []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2Before, ctx.User2Addr)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User2Key), ctx.CTFContract,
		"redeemPositions", ctx.USDCAddr, [32]byte{}, ctx.ConditionId, []*big.Int{big.NewInt(2)})
	var u2After []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2After, ctx.User2Addr)
	noRedemption := new(big.Int).Sub(u2After[0].(*big.Int), u2Before[0].(*big.Int))
	fmt.Printf("✓ User2 赎回 NO:  %.2f → %.2f USDC（NO 赢，+%.2f）\n",
		ex.FromUsdc(u2Before[0].(*big.Int)),
		ex.FromUsdc(u2After[0].(*big.Int)),
		ex.FromUsdc(noRedemption))

	ex.Div("OOv3 争议处理流程演示完成！")
	fmt.Println("\n【OOv3 与 OOv2 争议流程对比】")
	fmt.Println("  OOv2 需要 7 步：错误提案 → 质疑 → DVM 裁定 → 重新提案 → 等待 liveness → settle → resolve")
	fmt.Println("  OOv3 只需 4 步：错误提案 → 质疑 → DVM 裁定（直接触发回调结算）→ 赎回")
	fmt.Println()
	fmt.Println("【资金流向总结】")
	fmt.Printf("  Proposer（deployer）：错误提案，损失 bond %.2f USDC\n",
		ex.FromUsdc(ctx.ProposalBond))
	fmt.Printf("  User2（质疑者）    ：DVM bond 奖励 %+.2f USDC + NO 赎回 %.2f USDC\n",
		ex.FromUsdc(dvmGain), ex.FromUsdc(noRedemption))
	fmt.Println()
	fmt.Println("部署的合约：")
	fmt.Printf("  MockOOv3:          %s\n", ctx.OOAddr.Hex())
	fmt.Printf("  MockFinder:        %s\n", ctx.FinderAddr.Hex())
	fmt.Printf("  UmaCtfAdapterV3:   %s\n", ctx.AdapterAddr.Hex())
}
