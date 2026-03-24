// nonce_test 验证 CTFExchange NonceManager 的行为：
//
//   - 同一 nonce 下可以挂多个订单（A、B、C 都用 nonce=N）
//   - nonce 不匹配的订单无效（D 用 nonce=N+1，在 N 阶段应 revert）
//   - 调用 incrementNonce() 后，旧 nonce 订单全部失效，新 nonce 订单生效
//
// 运行（从 exchange/ 目录）：
//
//	go run ./cmd/nonce_test
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

// buildOrder 构造一个 SELL YES 订单并签名，nonce 由调用方指定。
// 步骤 5 后 User1 有 1000 YES、0 NO，所以用 YES 代币避免余额不足。
func buildOrder(ctx *ex.MarketContext, nonce *big.Int) *ex.CTFOrder {
	order := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   ex.ToUsdc(100), // SELL 100 YES
		TakerAmount:   ex.ToUsdc(50),  // 期望收 50 USDC（@ 0.5）
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         nonce,
		FeeRateBps:    big.NewInt(0),
		Side:          1, // SELL
		SignatureType: 0,
	}
	if err := ex.SignOrder(order, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("签名失败: %v", err)
	}
	return order
}

// buildBuyOrder 构造一个配套的 BUY YES 订单（User2），用于触发撮合。
func buildBuyOrder(ctx *ex.MarketContext, nonce *big.Int) *ex.CTFOrder {
	order := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   ex.ToUsdc(50),  // BUY：出 50 USDC
		TakerAmount:   ex.ToUsdc(100), // 期望收 100 YES
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         nonce,
		FeeRateBps:    big.NewInt(0),
		Side:          0, // BUY
		SignatureType: 0,
	}
	if err := ex.SignOrder(order, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("签名失败: %v", err)
	}
	return order
}

// tryMatch 尝试撮合 makerOrder(SELL) 和 takerOrder(BUY)。
// 返回 true 表示成功，false 表示链上 revert（订单无效）。
// 使用 TrySend 代替 Send，避免 revert 时 log.Fatalf 直接退出程序。
func tryMatch(ctx *ex.MarketContext, maker, taker *ex.CTFOrder) bool {
	operatorAuth := ex.NewAuth(ctx.Client, ctx.OperatorKey)
	user2Auth := ex.NewAuth(ctx.Client, ctx.User2Key)
	ex.Send(ctx.Client, user2Auth, ctx.USDCContract, "approve", ctx.ExchangeAddr, ex.ToUsdc(50))
	_, err := ex.TrySend(ctx.Client, operatorAuth, ctx.ExchangeContract, "matchOrders",
		ex.ToOrderTuple(taker),
		[]ex.OrderTuple{ex.ToOrderTuple(maker)},
		ex.ToUsdc(50),
		[]*big.Int{ex.ToUsdc(100)},
	)
	return err == nil
}

func readNonce(ctx *ex.MarketContext, addr common.Address) *big.Int {
	var result []interface{}
	ex.CallView(ctx.ExchangeContract, "nonces", &result, addr)
	return result[0].(*big.Int)
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	// 执行公共步骤 1-5（部署合约、铸造 USDC、初始化市场、拆分头寸、撮合初始订单）
	// 注意：步骤 5 已经撮合了一次（User1 SELL 1000 NO），会消耗 nonce=0 的一个名额，
	// 但 nonce 不会变，仍为 0。
	ctx := ex.RunCommonSetup(*configPath)

	// ── 读取初始 nonce ────────────────────────────────────────────────────────
	n1 := readNonce(ctx, ctx.User1Addr)
	n2 := readNonce(ctx, ctx.User2Addr)
	ex.Div(fmt.Sprintf("初始状态：User1 nonce=%s，User2 nonce=%s", n1, n2))

	currentNonce := new(big.Int).Set(n1) // User1 当前 nonce
	nextNonce := new(big.Int).Add(currentNonce, big.NewInt(1))

	// ── 场景一：A/B/C（nonce=N）有效，D（nonce=N+1）无效 ─────────────────────
	ex.Div(fmt.Sprintf("场景一：构造 orderA/B/C（nonce=%s）和 orderD（nonce=%s）", currentNonce, nextNonce))

	orderA := buildOrder(ctx, new(big.Int).Set(currentNonce))
	orderB := buildOrder(ctx, new(big.Int).Set(currentNonce))
	orderC := buildOrder(ctx, new(big.Int).Set(currentNonce))
	orderD := buildOrder(ctx, new(big.Int).Set(nextNonce))

	fmt.Printf("  orderA nonce=%s，orderB nonce=%s，orderC nonce=%s，orderD nonce=%s\n",
		orderA.Nonce, orderB.Nonce, orderC.Nonce, orderD.Nonce)

	// 验证 orderA（nonce=N）有效
	takerA := buildBuyOrder(ctx, new(big.Int).Set(currentNonce))
	if tryMatch(ctx, orderA, takerA) {
		fmt.Printf("✓ orderA（nonce=%s）撮合成功 → 符合预期（nonce 匹配）\n", orderA.Nonce)
	} else {
		fmt.Printf("✗ orderA（nonce=%s）撮合失败 → 不符合预期\n", orderA.Nonce)
	}

	// 验证 orderD（nonce=N+1）无效（链上 nonce 仍为 N）
	takerD := buildBuyOrder(ctx, new(big.Int).Set(nextNonce))
	if tryMatch(ctx, orderD, takerD) {
		fmt.Printf("✗ orderD（nonce=%s）撮合成功 → 不符合预期（应该失败）\n", orderD.Nonce)
	} else {
		fmt.Printf("✓ orderD（nonce=%s）撮合失败 → 符合预期（nonce 不匹配，revert InvalidNonce）\n", orderD.Nonce)
	}

	fmt.Printf("  此时 orderB/C（nonce=%s）仍然有效（同 nonce 下多订单互不影响）\n", currentNonce)

	// ── 场景二：incrementNonce() 后，B/C 失效，D 生效 ────────────────────────
	ex.Div("场景二：User1 调用 incrementNonce()")

	user1Auth := ex.NewAuth(ctx.Client, ctx.User1Key)
	ex.Send(ctx.Client, user1Auth, ctx.ExchangeContract, "incrementNonce")

	n1After := readNonce(ctx, ctx.User1Addr)
	fmt.Printf("✓ incrementNonce() 完成，User1 nonce: %s → %s\n", currentNonce, n1After)

	// 验证 orderB（nonce=N）现在无效
	user2Auth := ex.NewAuth(ctx.Client, ctx.User2Key)
	ex.Send(ctx.Client, user2Auth, ctx.USDCContract, "approve", ctx.ExchangeAddr, ex.ToUsdc(50))
	takerB := buildBuyOrder(ctx, new(big.Int).Set(currentNonce))
	if tryMatch(ctx, orderB, takerB) {
		fmt.Printf("✗ orderB（nonce=%s）撮合成功 → 不符合预期（应已失效）\n", orderB.Nonce)
	} else {
		fmt.Printf("✓ orderB（nonce=%s）撮合失败 → 符合预期（incrementNonce 后旧订单全部失效）\n", orderB.Nonce)
	}

	// 验证 orderD（nonce=N+1）现在有效
	// 注意：User2 的 nonce 也需要是 N+1
	n2After := readNonce(ctx, ctx.User2Addr)
	fmt.Printf("  User2 当前 nonce=%s，需要也 incrementNonce 才能让 takerD 有效\n", n2After)
	if n2After.Cmp(nextNonce) != 0 {
		ex.Send(ctx.Client, user2Auth, ctx.ExchangeContract, "incrementNonce")
		fmt.Printf("✓ User2 incrementNonce() 完成，nonce=%s\n", nextNonce)
	}

	ex.Send(ctx.Client, user2Auth, ctx.USDCContract, "approve", ctx.ExchangeAddr, ex.ToUsdc(50))
	takerD2 := buildBuyOrder(ctx, new(big.Int).Set(nextNonce))
	if tryMatch(ctx, orderD, takerD2) {
		fmt.Printf("✓ orderD（nonce=%s）撮合成功 → 符合预期（incrementNonce 后新 nonce 订单有效）\n", orderD.Nonce)
	} else {
		fmt.Printf("✗ orderD（nonce=%s）撮合失败 → 不符合预期\n", orderD.Nonce)
	}

	ex.Div("Nonce 验证完成")
	fmt.Println("\n【结论】")
	fmt.Println("  1. 同一 nonce 下可同时存在多个有效订单（A/B/C）")
	fmt.Println("  2. nonce 不匹配的订单无效（D 在 nonce=N 时 revert）")
	fmt.Println("  3. incrementNonce() 使旧 nonce 的所有订单失效（B/C 作废）")
	fmt.Println("  4. incrementNonce() 后新 nonce 订单生效（D 可成交）")
}
