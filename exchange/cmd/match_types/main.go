// match_types 演示 CTFExchange 的三种撮合类型：MINT、COMPLEMENTARY、MERGE。
//
// 前置：RunCommonSetup（步骤 1-5）执行完毕后：
//
//	User1: 1000 YES,    0 NO, 9500 USDC
//	User2: 1000 YES, 2000 NO, 8500 USDC
//
// 本测试新增以下四笔撮合（均通过 CTFExchange.matchOrders）：
//
//	步骤 A（MINT）       : User1 BUY 500 YES + User2 BUY 500 NO
//	                       → Exchange 用 500 USDC 铸造 500 YES+NO 对后分配
//	步骤 B1（COMPLEMENTARY）: User2 SELL 500 YES → User1 BUY 500 YES
//	                       → 代币直接换手，User2 收回 USDC
//	步骤 B2（COMPLEMENTARY，反向）: User1 SELL 500 YES → User2 BUY 500 YES
//	                       → 反向换手，回到 B1 前状态
//	步骤 C（MERGE）      : User1 SELL 500 YES + User2 SELL 500 NO
//	                       → Exchange 合并 500 YES+NO 对，各换回 250 USDC
//
// 运行（从 exchange/ 目录）：
//
//	go run cmd/match_types/main.go
//	go run cmd/match_types/main.go -config config.json
package main

import (
	"flag"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"

	ex "polymarket-exchange"
)

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	// ── 步骤 1-5：公共初始化 ──────────────────────────────────────────────────
	// 执行后状态：
	//   User1: 1000 YES,    0 NO, ~9500 USDC（步骤 5 SELL NO 收到 500 USDC）
	//   User2: 1000 YES, 2000 NO, ~8500 USDC（步骤 5 BUY  NO 花了 500 USDC）
	ctx := ex.RunCommonSetup(*configPath)

	printBalances("公共初始化完成后", ctx)

	// ── 步骤 A：MINT（双边 BUY，铸造新代币对）────────────────────────────────
	//
	// 触发条件：takerOrder.side == BUY && makerOrder.side == BUY
	//           且 takerOrder.tokenId 与 makerOrder.tokenId 互补（YES ↔ NO）
	//
	// 资金流：
	//   Exchange 从 User1 拉取 250 USDC，从 User2 拉取 250 USDC
	//   → Exchange 调用 CTF.splitPosition（铸造 500 YES + 500 NO）
	//   → 500 YES 转给 User1，500 NO 转给 User2
	//
	// 预期结果：
	//   User1: +500 YES,   0 NO, -250 USDC  → 1500 YES,    0 NO, ~9250 USDC
	//   User2:    0 YES, +500 NO, -250 USDC  → 1000 YES, 2500 NO, ~8250 USDC
	ex.Div("步骤 A: MINT — User1 BUY 500 YES + User2 BUY 500 NO（铸造新代币对）")

	mintTokens := ex.ToUsdc(500) // 500 个代币（精度 6 位）
	mintUsdc := ex.ToUsdc(250)   // 每人出 250 USDC（500 token * 0.5 USDC/token）

	// BUY 方需要预先 approve Exchange 花费 USDC（撮合时 Exchange 从用户拉取）
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User1Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, mintUsdc)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User2Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, mintUsdc)

	// takerOrder：User1 BUY YES
	//   makerAmount = USDC 愿意支付（250）
	//   takerAmount = YES token 期望获得（500）
	//   side = 0（BUY）
	takerMint := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   mintUsdc,
		TakerAmount:   mintTokens,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User1Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          0, // BUY
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerMint, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MINT takerOrder 签名失败: %v", err)
	}

	// makerOrder：User2 BUY NO（tokenId 与 taker 互补，触发 MINT 而非 COMPLEMENTARY）
	//   makerAmount = USDC 愿意支付（250）
	//   takerAmount = NO token 期望获得（500）
	//   side = 0（BUY）
	makerMint := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.NoTokenId,
		MakerAmount:   mintUsdc,
		TakerAmount:   mintTokens,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User2Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          0, // BUY
		SignatureType: 0,
	}
	if err := ex.SignOrder(makerMint, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MINT makerOrder 签名失败: %v", err)
	}

	// takerFillAmount：taker（BUY）填入的是 USDC 数量
	// makerFillAmounts：maker（BUY）填入的是 USDC 数量
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.OperatorKey), ctx.ExchangeContract, "matchOrders",
		ex.ToOrderTuple(takerMint),
		[]ex.OrderTuple{ex.ToOrderTuple(makerMint)},
		mintUsdc,
		[]*big.Int{mintUsdc},
	)
	fmt.Println("✓ MINT matchOrders 成功（Exchange 铸造了 500 YES+NO 对）")
	time.Sleep(4 * time.Second)
	printBalances("步骤 A（MINT）完成后", ctx)

	// ── 步骤 B1：COMPLEMENTARY（User2 SELL YES → User1 BUY YES）────────────
	//
	// 触发条件：takerOrder.side == BUY && makerOrder.side == SELL，相同 tokenId
	//
	// 资金流：
	//   Exchange 从 User1 拉取 250 USDC → 转给 User2
	//   Exchange 从 User2 拉取 500 YES  → 转给 User1
	//
	// 预期结果：
	//   User1: +500 YES, -250 USDC  → 2000 YES, 0 NO, ~9000 USDC
	//   User2: -500 YES, +250 USDC  →  500 YES, 2500 NO, ~8500 USDC
	ex.Div("步骤 B1: COMPLEMENTARY — User1 BUY 500 YES ← User2 SELL 500 YES（代币换手）")

	compTokens := ex.ToUsdc(500)
	compUsdc := ex.ToUsdc(250)

	// BUY 方（User1）approve USDC 给 Exchange
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User1Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, compUsdc)

	// takerOrder：User1 BUY YES
	takerB1 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compUsdc,    // USDC 支付
		TakerAmount:   compTokens,  // YES 期望收到
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User1Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          0, // BUY
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerB1, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("B1 takerOrder 签名失败: %v", err)
	}

	// makerOrder：User2 SELL YES（同一 tokenId，触发 COMPLEMENTARY）
	//   makerAmount = YES token 卖出数量（500）
	//   takerAmount = USDC 期望收到（250）
	//   side = 1（SELL）
	makerB1 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compTokens,  // YES token 卖出
		TakerAmount:   compUsdc,    // USDC 期望收到
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User2Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1, // SELL
		SignatureType: 0,
	}
	if err := ex.SignOrder(makerB1, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("B1 makerOrder 签名失败: %v", err)
	}

	// takerFillAmount：taker（BUY）填入 USDC 数量
	// makerFillAmounts：maker（SELL）填入 token 数量
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.OperatorKey), ctx.ExchangeContract, "matchOrders",
		ex.ToOrderTuple(takerB1),
		[]ex.OrderTuple{ex.ToOrderTuple(makerB1)},
		compUsdc,
		[]*big.Int{compTokens},
	)
	fmt.Println("✓ COMPLEMENTARY matchOrders 成功（User2 YES → User1，User1 USDC → User2）")
	time.Sleep(4 * time.Second)
	printBalances("步骤 B1（COMPLEMENTARY）完成后", ctx)

	// ── 步骤 B2：COMPLEMENTARY 反向（User1 SELL YES → User2 BUY YES）─────────
	//
	// 与 B1 方向完全相反：User1 变为卖方，User2 变为买方。
	// 演示同一种 MatchType 在不同方向下的资金流。
	//
	// 资金流：
	//   Exchange 从 User2 拉取 250 USDC → 转给 User1
	//   Exchange 从 User1 拉取 500 YES  → 转给 User2
	//
	// 预期结果（恢复至 B1 前状态）：
	//   User1: -500 YES, +250 USDC  → 1500 YES,    0 NO, ~9250 USDC
	//   User2: +500 YES, -250 USDC  → 1000 YES, 2500 NO, ~8250 USDC
	ex.Div("步骤 B2: COMPLEMENTARY（反向）— User2 BUY 500 YES ← User1 SELL 500 YES")

	// BUY 方（User2）approve USDC 给 Exchange
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User2Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, compUsdc)

	// takerOrder：User2 BUY YES（角色与 B1 对调）
	takerB2 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compUsdc,    // USDC 支付
		TakerAmount:   compTokens,  // YES 期望收到
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User2Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          0, // BUY
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerB2, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("B2 takerOrder 签名失败: %v", err)
	}

	// makerOrder：User1 SELL YES（角色与 B1 对调）
	makerB2 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compTokens,  // YES token 卖出
		TakerAmount:   compUsdc,    // USDC 期望收到
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User1Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1, // SELL
		SignatureType: 0,
	}
	if err := ex.SignOrder(makerB2, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("B2 makerOrder 签名失败: %v", err)
	}

	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.OperatorKey), ctx.ExchangeContract, "matchOrders",
		ex.ToOrderTuple(takerB2),
		[]ex.OrderTuple{ex.ToOrderTuple(makerB2)},
		compUsdc,
		[]*big.Int{compTokens},
	)
	fmt.Println("✓ COMPLEMENTARY（反向）matchOrders 成功（User1 YES → User2，User2 USDC → User1）")
	time.Sleep(4 * time.Second)
	printBalances("步骤 B2（COMPLEMENTARY 反向）完成后", ctx)

	// ── 步骤 C：MERGE（双边 SELL，销毁代币对换回抵押品）─────────────────────
	//
	// 触发条件：takerOrder.side == SELL && makerOrder.side == SELL
	//           且 takerOrder.tokenId 与 makerOrder.tokenId 互补（YES ↔ NO）
	//
	// 资金流：
	//   Exchange 从 User1 拉取 500 YES，从 User2 拉取 500 NO
	//   → Exchange 调用 CTF.mergePositions（销毁 500 YES+NO 对，释放 500 USDC）
	//   → 250 USDC 转给 User1，250 USDC 转给 User2
	//
	// 预期结果：
	//   User1: -500 YES, +250 USDC  → 1000 YES,    0 NO, ~9500 USDC
	//   User2:    0 YES, -500 NO,   → 1000 YES, 2000 NO, ~8500 USDC
	//                   +250 USDC
	ex.Div("步骤 C: MERGE — User1 SELL 500 YES + User2 SELL 500 NO（销毁代币对换回 USDC）")

	mergeTokens := ex.ToUsdc(500)
	mergeUsdc := ex.ToUsdc(250)

	// MERGE 不需要额外的 approve：Exchange 通过 ERC1155.setApprovalForAll 拉取代币
	// （步骤 4 已授权），无需 USDC approve（Exchange 是 USDC 的给出方，不是拉取方）

	// takerOrder：User1 SELL YES
	//   makerAmount = YES token 卖出数量（500）
	//   takerAmount = USDC 期望收到（250）
	//   side = 1（SELL）
	takerC := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   mergeTokens, // YES token 卖出
		TakerAmount:   mergeUsdc,   // USDC 期望收到
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User1Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1, // SELL
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerC, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MERGE takerOrder 签名失败: %v", err)
	}

	// makerOrder：User2 SELL NO（tokenId 与 taker 互补，触发 MERGE）
	//   makerAmount = NO token 卖出数量（500）
	//   takerAmount = USDC 期望收到（250）
	//   side = 1（SELL）
	makerC := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.NoTokenId,
		MakerAmount:   mergeTokens, // NO token 卖出
		TakerAmount:   mergeUsdc,   // USDC 期望收到
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User2Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1, // SELL
		SignatureType: 0,
	}
	if err := ex.SignOrder(makerC, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MERGE makerOrder 签名失败: %v", err)
	}

	// takerFillAmount：taker（SELL YES）填入 YES token 数量
	// makerFillAmounts：maker（SELL NO）填入 NO token 数量
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.OperatorKey), ctx.ExchangeContract, "matchOrders",
		ex.ToOrderTuple(takerC),
		[]ex.OrderTuple{ex.ToOrderTuple(makerC)},
		mergeTokens,
		[]*big.Int{mergeTokens},
	)
	fmt.Println("✓ MERGE matchOrders 成功（Exchange 销毁 500 YES+NO 对，各返还 250 USDC）")

	// 等待 BSC Testnet RPC 缓存刷新（出块 ~3s，等 2 个块确保 ERC1155 余额最终一致）
	fmt.Print("→ 等待 8 秒让 RPC 缓存刷新...")
	time.Sleep(8 * time.Second)
	fmt.Println(" ✓")
	printBalances("最终余额（RPC 缓存刷新后）", ctx)

	ex.Div("全部 MatchType 覆盖完成")
	fmt.Println("  ✓ MINT        — 双边 BUY，铸造新代币对（步骤 A）")
	fmt.Println("  ✓ COMPLEMENTARY — BUY vs SELL，代币换手（步骤 B1 正向）")
	fmt.Println("  ✓ COMPLEMENTARY — SELL vs BUY，反向换手（步骤 B2 逆向）")
	fmt.Println("  ✓ MERGE       — 双边 SELL，销毁代币对换回抵押品（步骤 C）")
}

// readNonce 每次用新的空 slice 读取链上 nonce，避免 go-ethereum v1.13 复用
// 非空 result slice 时写入旧类型（如 bool）导致的 "cannot unmarshal" 错误。
func readNonce(ctx *ex.MarketContext, addr common.Address) *big.Int {
	var result []interface{}
	ex.CallView(ctx.ExchangeContract, "nonces", &result, addr)
	return result[0].(*big.Int)
}

// printBalances 打印两个用户的 YES/NO/USDC 余额快照，用于验证每步结果。
func printBalances(label string, ctx *ex.MarketContext) {
	fmt.Printf("\n  [余额快照] %s\n", label)
	for _, info := range []struct {
		addr interface{}
		name string
	}{{ctx.User1Addr, "User1"}, {ctx.User2Addr, "User2"}} {
		var yBal, nBal, uBal []interface{}
		ex.CallView(ctx.CTFContract, "balanceOf", &yBal, info.addr, ctx.YesTokenId)
		ex.CallView(ctx.CTFContract, "balanceOf", &nBal, info.addr, ctx.NoTokenId)
		ex.CallView(ctx.USDCContract, "balanceOf", &uBal, info.addr)
		fmt.Printf("    %s: YES=%-12s  NO=%-12s  USDC=%.2f\n",
			info.name,
			yBal[0].(*big.Int),
			nBal[0].(*big.Int),
			ex.FromUsdc(uBal[0].(*big.Int)),
		)
	}
}
