// v3/match_types 覆盖 CTFExchange 全部三种 MatchType（OOv3 版本）。
//
// 本测试与 cmd/match_types 逻辑完全相同，
// 唯一区别是步骤 1-5 使用 RunCommonSetupV3（部署 MockOOv3 + UmaCtfAdapterV3）。
//
// MatchType 是 CTFExchange 的功能，与 oracle 版本无关。
// 覆盖场景：
//   步骤 A：MINT        — 双边 BUY，铸造新代币对
//   步骤 B1：COMPLEMENTARY — BUY vs SELL，代币换手（正向）
//   步骤 B2：COMPLEMENTARY — SELL vs BUY，反向换手
//   步骤 C：MERGE       — 双边 SELL，销毁代币对换回 USDC
//
// 运行（从 exchange/ 目录）：
//
//	go run ./cmd/v3/match_types
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

func readNonce(ctx *ex.MarketContext, addr common.Address) *big.Int {
	var result []interface{}
	ex.CallView(ctx.ExchangeContract, "nonces", &result, addr)
	return result[0].(*big.Int)
}

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

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	// 步骤 1-5：OOv3 版本公共初始化
	ctx := ex.RunCommonSetupV3(*configPath)

	// ── 步骤 A：MINT（双边 BUY，铸造新代币对）────────────────────────────────
	ex.Div("步骤 A: MINT — User1 BUY 500 YES + User2 BUY 500 NO（铸造新代币对）")

	mintTokens := ex.ToUsdc(500)
	mintUsdc := ex.ToUsdc(250)

	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User1Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, mintUsdc)
	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User2Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, mintUsdc)

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
		Side:          0,
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerMint, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MINT takerOrder 签名失败: %v", err)
	}

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
		Side:          0,
		SignatureType: 0,
	}
	if err := ex.SignOrder(makerMint, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MINT makerOrder 签名失败: %v", err)
	}

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
	ex.Div("步骤 B1: COMPLEMENTARY — User1 BUY 500 YES ← User2 SELL 500 YES（代币换手）")

	compTokens := ex.ToUsdc(500)
	compUsdc := ex.ToUsdc(250)

	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User1Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, compUsdc)

	takerB1 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compUsdc,
		TakerAmount:   compTokens,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User1Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          0,
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerB1, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("B1 takerOrder 签名失败: %v", err)
	}

	makerB1 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compTokens,
		TakerAmount:   compUsdc,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User2Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1,
		SignatureType: 0,
	}
	if err := ex.SignOrder(makerB1, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("B1 makerOrder 签名失败: %v", err)
	}

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
	ex.Div("步骤 B2: COMPLEMENTARY（反向）— User2 BUY 500 YES ← User1 SELL 500 YES")

	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.User2Key), ctx.USDCContract, "approve", ctx.ExchangeAddr, compUsdc)

	takerB2 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compUsdc,
		TakerAmount:   compTokens,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User2Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          0,
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerB2, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("B2 takerOrder 签名失败: %v", err)
	}

	makerB2 := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   compTokens,
		TakerAmount:   compUsdc,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User1Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1,
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
	ex.Div("步骤 C: MERGE — User1 SELL 500 YES + User2 SELL 500 NO（销毁代币对换回 USDC）")

	mergeTokens := ex.ToUsdc(500)
	mergeUsdc := ex.ToUsdc(250)

	takerC := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   mergeTokens,
		TakerAmount:   mergeUsdc,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User1Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1,
		SignatureType: 0,
	}
	if err := ex.SignOrder(takerC, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MERGE takerOrder 签名失败: %v", err)
	}

	makerC := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.NoTokenId,
		MakerAmount:   mergeTokens,
		TakerAmount:   mergeUsdc,
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         readNonce(ctx, ctx.User2Addr),
		FeeRateBps:    big.NewInt(0),
		Side:          1,
		SignatureType: 0,
	}
	if err := ex.SignOrder(makerC, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("MERGE makerOrder 签名失败: %v", err)
	}

	ex.Send(ctx.Client, ex.NewAuth(ctx.Client, ctx.OperatorKey), ctx.ExchangeContract, "matchOrders",
		ex.ToOrderTuple(takerC),
		[]ex.OrderTuple{ex.ToOrderTuple(makerC)},
		mergeTokens,
		[]*big.Int{mergeTokens},
	)
	fmt.Println("✓ MERGE matchOrders 成功（Exchange 销毁 500 YES+NO 对，各返还 250 USDC）")

	fmt.Print("→ 等待 8 秒让 RPC 缓存刷新...")
	time.Sleep(8 * time.Second)
	fmt.Println(" ✓")
	printBalances("最终余额（RPC 缓存刷新后）", ctx)

	ex.Div("全部 MatchType 覆盖完成（OOv3 版本）")
	fmt.Println("  ✓ MINT        — 双边 BUY，铸造新代币对（步骤 A）")
	fmt.Println("  ✓ COMPLEMENTARY — BUY vs SELL，代币换手（步骤 B1 正向）")
	fmt.Println("  ✓ COMPLEMENTARY — SELL vs BUY，反向换手（步骤 B2 逆向）")
	fmt.Println("  ✓ MERGE       — 双边 SELL，销毁代币对换回抵押品（步骤 C）")
}
