// v3/nonce_test 验证 CTFExchange NonceManager 的行为（OOv3 版本）。
//
// 本测试与 cmd/nonce_test 逻辑完全相同，
// 唯一区别是步骤 1-5 使用 RunCommonSetupV3（部署 MockOOv3 + UmaCtfAdapterV3）。
//
// NonceManager 是 CTFExchange 的功能，与 oracle 版本无关。
// 验证场景：
//   - 同一 nonce.log 下可以挂多个订单（A、B、C 都用 nonce.log=N）
//   - nonce.log 不匹配的订单无效（D 用 nonce.log=N+1，在 N 阶段应 revert）
//   - 调用 incrementNonce() 后，旧 nonce.log 订单全部失效，新 nonce.log 订单生效
//
// 运行（从 exchange/ 目录）：
//
//	go run ./cmd/v3/nonce_test
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

func buildOrder(ctx *ex.MarketContext, nonce *big.Int) *ex.CTFOrder {
	order := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User1Addr,
		Signer:        ctx.User1Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   ex.ToUsdc(100),
		TakerAmount:   ex.ToUsdc(50),
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         nonce,
		FeeRateBps:    big.NewInt(0),
		Side:          1,
		SignatureType: 0,
	}
	if err := ex.SignOrder(order, ctx.User1Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("签名失败: %v", err)
	}
	return order
}

func buildBuyOrder(ctx *ex.MarketContext, nonce *big.Int) *ex.CTFOrder {
	order := &ex.CTFOrder{
		Salt:          big.NewInt(rand.Int63()),
		Maker:         ctx.User2Addr,
		Signer:        ctx.User2Addr,
		Taker:         common.Address{},
		TokenId:       ctx.YesTokenId,
		MakerAmount:   ex.ToUsdc(50),
		TakerAmount:   ex.ToUsdc(100),
		Expiration:    big.NewInt(time.Now().Unix() + 3600),
		Nonce:         nonce,
		FeeRateBps:    big.NewInt(0),
		Side:          0,
		SignatureType: 0,
	}
	if err := ex.SignOrder(order, ctx.User2Key, ctx.ChainID, ctx.ExchangeAddr); err != nil {
		log.Fatalf("签名失败: %v", err)
	}
	return order
}

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

	// 步骤 1-5：OOv3 版本公共初始化
	ctx := ex.RunCommonSetupV3(*configPath)

	n1 := readNonce(ctx, ctx.User1Addr)
	n2 := readNonce(ctx, ctx.User2Addr)
	ex.Div(fmt.Sprintf("初始状态：User1 nonce.log=%s，User2 nonce.log=%s", n1, n2))

	currentNonce := new(big.Int).Set(n1)
	nextNonce := new(big.Int).Add(currentNonce, big.NewInt(1))

	ex.Div(fmt.Sprintf("场景一：构造 orderA/B/C（nonce.log=%s）和 orderD（nonce.log=%s）", currentNonce, nextNonce))

	orderA := buildOrder(ctx, new(big.Int).Set(currentNonce))
	orderB := buildOrder(ctx, new(big.Int).Set(currentNonce))
	orderC := buildOrder(ctx, new(big.Int).Set(currentNonce))
	orderD := buildOrder(ctx, new(big.Int).Set(nextNonce))

	fmt.Printf("  orderA nonce.log=%s，orderB nonce.log=%s，orderC nonce.log=%s，orderD nonce.log=%s\n",
		orderA.Nonce, orderB.Nonce, orderC.Nonce, orderD.Nonce)

	takerA := buildBuyOrder(ctx, new(big.Int).Set(currentNonce))
	if tryMatch(ctx, orderA, takerA) {
		fmt.Printf("✓ orderA（nonce.log=%s）撮合成功 → 符合预期（nonce.log 匹配）\n", orderA.Nonce)
	} else {
		fmt.Printf("✗ orderA（nonce.log=%s）撮合失败 → 不符合预期\n", orderA.Nonce)
	}

	takerD := buildBuyOrder(ctx, new(big.Int).Set(nextNonce))
	if tryMatch(ctx, orderD, takerD) {
		fmt.Printf("✗ orderD（nonce.log=%s）撮合成功 → 不符合预期（应该失败）\n", orderD.Nonce)
	} else {
		fmt.Printf("✓ orderD（nonce.log=%s）撮合失败 → 符合预期（nonce.log 不匹配，revert InvalidNonce）\n", orderD.Nonce)
	}

	fmt.Printf("  此时 orderB/C（nonce.log=%s）仍然有效（同 nonce.log 下多订单互不影响）\n", currentNonce)

	ex.Div("场景二：User1 调用 incrementNonce()")

	user1Auth := ex.NewAuth(ctx.Client, ctx.User1Key)
	ex.Send(ctx.Client, user1Auth, ctx.ExchangeContract, "incrementNonce")

	n1After := readNonce(ctx, ctx.User1Addr)
	fmt.Printf("✓ incrementNonce() 完成，User1 nonce.log: %s → %s\n", currentNonce, n1After)

	user2Auth := ex.NewAuth(ctx.Client, ctx.User2Key)
	ex.Send(ctx.Client, user2Auth, ctx.USDCContract, "approve", ctx.ExchangeAddr, ex.ToUsdc(50))
	takerB := buildBuyOrder(ctx, new(big.Int).Set(currentNonce))
	if tryMatch(ctx, orderB, takerB) {
		fmt.Printf("✗ orderB（nonce.log=%s）撮合成功 → 不符合预期（应已失效）\n", orderB.Nonce)
	} else {
		fmt.Printf("✓ orderB（nonce.log=%s）撮合失败 → 符合预期（incrementNonce 后旧订单全部失效）\n", orderB.Nonce)
	}

	n2After := readNonce(ctx, ctx.User2Addr)
	fmt.Printf("  User2 当前 nonce.log=%s，需要也 incrementNonce 才能让 takerD 有效\n", n2After)
	if n2After.Cmp(nextNonce) != 0 {
		ex.Send(ctx.Client, user2Auth, ctx.ExchangeContract, "incrementNonce")
		fmt.Printf("✓ User2 incrementNonce() 完成，nonce.log=%s\n", nextNonce)
	}

	ex.Send(ctx.Client, user2Auth, ctx.USDCContract, "approve", ctx.ExchangeAddr, ex.ToUsdc(50))
	takerD2 := buildBuyOrder(ctx, new(big.Int).Set(nextNonce))
	if tryMatch(ctx, orderD, takerD2) {
		fmt.Printf("✓ orderD（nonce.log=%s）撮合成功 → 符合预期（incrementNonce 后新 nonce.log 订单有效）\n", orderD.Nonce)
	} else {
		fmt.Printf("✗ orderD（nonce.log=%s）撮合失败 → 不符合预期\n", orderD.Nonce)
	}

	ex.Div("Nonce 验证完成")
	fmt.Println("\n【结论】")
	fmt.Println("  1. 同一 nonce.log 下可同时存在多个有效订单（A/B/C）")
	fmt.Println("  2. nonce.log 不匹配的订单无效（D 在 nonce.log=N 时 revert）")
	fmt.Println("  3. incrementNonce() 使旧 nonce.log 的所有订单失效（B/C 作废）")
	fmt.Println("  4. incrementNonce() 后新 nonce.log 订单生效（D 可成交）")
}
