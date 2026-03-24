// dispute 演示 Polymarket 预测市场的争议处理流程。
//
// 场景：
//
//	提案方错误地提案 YES 赢 → User2（持有 NO）发起争议 → DVM 裁定提案方错误 →
//	争议者获得 bond 奖励 → 用新 requestTime（T2）重新提案 NO 赢 → 等待 liveness → 结算
//
// 流程：
//
//	步骤 1-5（公共）：部署合约、铸造 USDC、初始化市场、拆分头寸、撮合订单
//	步骤 6：deployer 提案 YES（错误答案，price=1e18）
//	步骤 7：User2 质疑提案（disputePrice）→ adapter 回调更新 requestTime 为 T2
//	步骤 8：DVM 裁定（mockDvmSettle, resolution=false）→ 质疑者胜，User2 +200 USDC bond 奖励
//	步骤 9：deployer 重新提案 NO（正确答案，price=0，使用新 requestTime T2）
//	步骤 10：等待 liveness（无新质疑）
//	步骤 11：OO.settle(T2) → adapter.resolve → NO 赢
//	步骤 12：赎回（User1 YES → +0；User2 NO → +2000 USDC）
//
// 运行（从 exchange/ 目录）：
//
//	go run ./cmd/dispute
//	go run ./cmd/dispute -config config.json
package main

import (
	"flag"
	"fmt"
	"math/big"
	"reflect"

	ex "polymarket-exchange"
)

// getNewRequestTime 从 adapter.getQuestion() 读取争议后更新的 requestTime（T2）。
//
// 背景：disputePrice 被调用时，MockOOv2 会回调 adapter.priceDisputed()，
// adapter 内部将 q.requestTime 更新为当前区块时间（T2），并向 OO 发起新的 requestPrice(T2)。
// 后续的 proposePrice/settle 必须使用 T2，而不是原来的 T1。
//
// 技术细节：go-ethereum 的 ABI 解码器将 Solidity struct（tuple）解码为匿名 struct，
// 无法通过类型断言转换为自定义 Go struct（会 panic）。
// 解决方案：使用反射（reflect）按字段名提取值。
// go-ethereum 将 Solidity 字段名转为 Go 风格（首字母大写），如 requestTime → RequestTime。
func getNewRequestTime(ctx *ex.MarketContext) *big.Int {
	var qData []interface{}
	ex.CallView(ctx.AdapterContract, "getQuestion", &qData, ctx.QuestionId)

	// qData[0] 是 ABI 解码后的匿名 struct（具体类型在运行时确定）
	v := reflect.ValueOf(qData[0])
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	// 按字段名查找 RequestTime（Solidity requestTime → Go RequestTime）
	f := v.FieldByName("RequestTime")
	if !f.IsValid() {
		// 兜底：尝试忽略大小写的匹配
		f = v.FieldByNameFunc(func(s string) bool {
			return s == "RequestTime" || s == "requestTime"
		})
	}
	if rt, ok := f.Interface().(*big.Int); ok {
		return rt
	}
	panic("无法从 getQuestion 结果中读取 requestTime")
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	// 步骤 1-5：部署合约、铸造 USDC、初始化市场、拆分头寸、撮合订单（与 normal 完全相同）
	// 执行后状态：
	//   User1: 1000 YES + 0 NO + ~9500 USDC
	//   User2: 1000 YES + 2000 NO + ~9000 USDC
	ctx := ex.RunCommonSetup(*configPath)

	// ── 步骤 6：错误提案（deployer 提案 YES，但实际结果应为 NO）────────────
	// 这模拟了现实中提案方试图欺诈或判断错误的场景。
	// 在 OOv2 协议中，任何人都可以提案，但错误提案会导致 bond 损失。
	ex.Div("步骤 6: 提案错误结果（OOv2.proposePrice → YES，但这是错的）")

	deployerAuth := ex.NewAuth(ctx.Client, ctx.DeployerKey)
	ex.Send(ctx.Client, deployerAuth, ctx.USDCContract, "approve", ctx.OOAddr, ctx.ProposalBond)

	wrongPrice := big.NewInt(1e18) // YES=1e18，但实际结果是 NO
	ex.Send(ctx.Client, deployerAuth, ctx.OOContract, "proposePrice",
		ctx.AdapterAddr, ctx.Identifier, ctx.RequestTime, ctx.AncillaryData, wrongPrice)
	fmt.Printf("✓ Proposer（deployer）提案 YES（price=1e18）\n")
	fmt.Printf("  → 质押 bond: %.2f USDC\n", ex.FromUsdc(ctx.ProposalBond))

	// 记录质疑前 User2 的余额，用于计算 DVM 奖励
	var u2BalBefore []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2BalBefore, ctx.User2Addr)

	// ── 步骤 7：User2 质疑提案 ────────────────────────────────────────────────
	// disputePrice 需要质疑者质押相同数量的 bond。
	// MockOOv2.disputePrice() 内部会：
	//  1. 从 User2 拉取 bond
	//  2. 回调 adapter.priceDisputed(identifier, timestamp=T1, ancillaryData, refund)
	//     adapter 内部：q.requestTime = block.timestamp（T2），并向 OO 发起新的 requestPrice(T2)
	//  3. OO 记录 T1 请求进入 Disputed 状态（等待 mockDvmSettle 处理）
	ex.Div("步骤 7: User2 质疑提案（disputePrice）")
	fmt.Println("  User2 持有 NO tokens，认为提案 YES 是错误的，发起质疑")

	user2Auth := ex.NewAuth(ctx.Client, ctx.User2Key)
	ex.Send(ctx.Client, user2Auth, ctx.USDCContract, "approve", ctx.OOAddr, ctx.ProposalBond)
	ex.Send(ctx.Client, user2Auth, ctx.OOContract, "disputePrice",
		ctx.AdapterAddr, ctx.Identifier, ctx.RequestTime, ctx.AncillaryData)
	fmt.Printf("✓ User2 质疑成功，质押 bond: %.2f USDC\n", ex.FromUsdc(ctx.ProposalBond))
	fmt.Println("  → MockOOv2 调用 adapter.priceDisputed()，市场重置（requestTime 更新）")

	// 读取争议后 adapter 内部更新的新 requestTime（T2）
	// 这是后续所有 OO 交互必须使用的时间戳
	newRequestTime := getNewRequestTime(ctx)
	fmt.Printf("✓ 新 requestTime: %s（原 requestTime: %s）\n", newRequestTime, ctx.RequestTime)

	// ── 步骤 8：DVM 裁定 ────────────────────────────────────────────────────
	// resolution=false 表示"原提案是错误的"，即质疑者（User2）胜。
	// 资金流向：
	//   - disputer（User2）获得 2x bond = proposer_bond + disputer_bond
	//   - 在真实 OOv2 中还有一部分给 UMA Store（协议费）；Mock 版本简化处理
	// 注意：mockDvmSettle 处理的是 T1 的请求（已进入 Disputed 状态的那个）。
	// adapter 在步骤 7 中已经用 T2 发起了新请求，T1 和 T2 是完全独立的两个请求。
	ex.Div("步骤 8: DVM 裁定（mockDvmSettle, resolution=false → 质疑者胜）")
	fmt.Println("  DVM 调查后确认：原提案 YES 是错误的，质疑者（User2）胜")

	ex.Send(ctx.Client, deployerAuth, ctx.OOContract, "mockDvmSettle",
		ctx.AdapterAddr, ctx.Identifier, ctx.RequestTime, ctx.AncillaryData, false)
	fmt.Println("✓ DVM 裁定完成（resolution=false）")

	var u2BalAfterDvm []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u2BalAfterDvm, ctx.User2Addr)
	dvmGain := new(big.Int).Sub(u2BalAfterDvm[0].(*big.Int), u2BalBefore[0].(*big.Int))
	fmt.Printf("  User2 DVM 奖励收益: %+.2f USDC（2x bond）\n", ex.FromUsdc(dvmGain))
	fmt.Println("  → OO 对 T1 的处理完毕，T2 请求已在步骤 7 创建，等待新提案")

	// ── 步骤 9：重新提案正确结果（NO 赢，使用 T2）───────────────────────────
	// 关键：必须使用 newRequestTime（T2），而不是原来的 ctx.RequestTime（T1）。
	// T1 的 OO 请求在 mockDvmSettle 后已处理完毕；
	// T2 的 OO 请求是 adapter.priceDisputed() 在步骤 7 中新建的，目前状态为 Requested，
	// 等待新的 proposePrice。
	ex.Div("步骤 9: 重新提案正确结果（price=0 → NO 赢）")
	fmt.Printf("  使用新 requestTime: %s\n", newRequestTime)

	ex.Send(ctx.Client, deployerAuth, ctx.USDCContract, "approve", ctx.OOAddr, ctx.ProposalBond)
	noPrice := big.NewInt(0) // 0 = NO 赢（UmaCtfAdapter 约定）
	ex.Send(ctx.Client, deployerAuth, ctx.OOContract, "proposePrice",
		ctx.AdapterAddr, ctx.Identifier, newRequestTime, ctx.AncillaryData, noPrice)
	fmt.Printf("✓ 重新提案 NO（price=0），liveness=%d 秒\n", ctx.Cfg.Market.LivenessSeconds)

	// ── 步骤 10：等待 liveness（无新质疑）───────────────────────────────────
	// 第二次提案期间无人质疑，liveness 结束后价格生效。
	ex.Div("步骤 10: 等待 liveness 结束（无新质疑）")
	ex.WaitLiveness(ctx.Client, ctx.Cfg)

	// ── 步骤 11：结算 ────────────────────────────────────────────────────────
	// 必须使用 newRequestTime（T2）调用 settle 和 resolve。
	// OO.settle(T2) → 价格 Resolved，bond 返还给第二次提案者（deployer）
	// adapter.resolve(questionId) → 查询 OO 价格（price=0 = NO 赢）
	//                             → CTF.reportPayouts([0, 1e18])
	ex.Div("步骤 11: 结算（OO.settle → adapter.resolve → NO 赢）")

	ex.Send(ctx.Client, deployerAuth, ctx.OOContract, "settle",
		ctx.AdapterAddr, ctx.Identifier, newRequestTime, ctx.AncillaryData)
	fmt.Println("✓ OO.settle 完成")

	ex.Send(ctx.Client, deployerAuth, ctx.AdapterContract, "resolve", ctx.QuestionId)
	fmt.Println("✓ adapter.resolve 完成 → reportPayouts([0, 1e18]) → NO 赢！")

	// ── 步骤 12：赎回 ────────────────────────────────────────────────────────
	// 结算结果 payouts=[0, 1e18]：
	//   YES（index 0）面值 = 0    → YES token 一文不值
	//   NO（index 1）面值 = 1e18 → 每个 NO token 换 1 USDC
	ex.Div("步骤 12: 赎回（CTF.redeemPositions）")

	// User1 持有 1000 YES → NO 赢，YES 归零，赎回 0
	var u1Before []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u1Before, ctx.User1Addr)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User1Key), ctx.CTFContract,
		"redeemPositions", ctx.USDCAddr, [32]byte{}, ctx.ConditionId, []*big.Int{big.NewInt(1)})
	var u1After []interface{}
	ex.CallView(ctx.USDCContract, "balanceOf", &u1After, ctx.User1Addr)
	fmt.Printf("✓ User1 赎回 YES: %.2f → %.2f USDC（NO 赢，YES 归零）\n",
		ex.FromUsdc(u1Before[0].(*big.Int)),
		ex.FromUsdc(u1After[0].(*big.Int)))

	// User2 持有 2000 NO（原有 1000 + 步骤 5 买入 1000）→ NO 赢，每个兑换 1 USDC，共 +2000 USDC
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

	ex.Div("争议处理流程演示完成！")
	fmt.Println("\n【资金流向总结】")
	fmt.Printf("  Proposer（deployer）：错误提案，损失 bond %.2f USDC\n",
		ex.FromUsdc(ctx.ProposalBond))
	fmt.Printf("  User2（质疑者）    ：DVM bond 奖励 %+.2f USDC + NO 赎回 %.2f USDC\n",
		ex.FromUsdc(dvmGain), ex.FromUsdc(noRedemption))
	fmt.Printf("\n部署的合约：\n")
	fmt.Printf("  MockOOv2     : %s\n", ctx.OOAddr.Hex())
	fmt.Printf("  UmaCtfAdapter: %s\n", ctx.AdapterAddr.Hex())
}
