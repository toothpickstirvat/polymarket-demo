// Package exchange 提供 Polymarket BSC Testnet 演示程序的公共代码。
//
// 架构：CTFExchange（订单簿）+ ConditionalTokens（ERC1155 YES/NO 代币）+
// UmaCtfAdapter（Oracle 适配）+ MockOOv2（模拟 UMA OOv2）
//
// 运行方式（从 exchange/ 目录）：
//
//	go run ./cmd/normal   # 正常结算流程（步骤 1-9）
//	go run ./cmd/dispute  # 争议处理流程（步骤 1-12）
package exchange

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ──────────────────────────────────────────────────────────────────────────────
// 配置
// ──────────────────────────────────────────────────────────────────────────────

// Config 对应 config.json 的结构。
// 所有私钥以 hex 字符串存储（可带或不带 "0x" 前缀）。
type Config struct {
	// RPCURL 是默认使用的节点地址（建议使用 Alchemy 等稳定节点，避免公共 RPC 的随机 500 错误）
	RPCURL string `json:"rpc_url"`
	// LocalMode=true 时使用 LocalRPCURL，并用 evm_increaseTime 替代真实等待
	LocalMode   bool   `json:"local_mode"`
	LocalRPCURL string `json:"local_rpc_url"`

	Accounts struct {
		DeployerPrivateKey string `json:"deployer_private_key"` // deployer 同时担任 DVM 角色（调用 mockDvmSettle）
		User1PrivateKey    string `json:"user1_private_key"`    // 买 YES 的用户
		User2PrivateKey    string `json:"user2_private_key"`    // 买 NO 的用户，争议流程中充当质疑者
		OperatorPrivateKey string `json:"operator_private_key"` // CTFExchange operator，唯一有权调用 matchOrders
	} `json:"accounts"`

	Contracts struct {
		CTF      string `json:"ctf"`      // ConditionalTokens（Gnosis CTF）地址，BSC Testnet 已有部署
		Exchange string `json:"exchange"` // CTFExchange 地址，BSC Testnet 已有部署
		USDC     string `json:"usdc"`     // 测试 USDC（ChildERC20）地址
	} `json:"contracts"`

	Market struct {
		AncillaryData    string  `json:"ancillary_data"`     // 问题描述，UTF-8 字节，传给 OOv2 作为价格请求标识
		ProposalBondUSDC float64 `json:"proposal_bond_usdc"` // 提案/质疑时需要锁定的 USDC 保证金
		LivenessSeconds  uint64  `json:"liveness_seconds"`   // 提案通过前的无质疑等待时间（测试用 120，生产建议 7200）
	} `json:"market"`

	Gas struct {
		PriceGwei int64  `json:"price_gwei"` // Gas 价格（BSC Testnet 通常 3 Gwei 即可）
		Limit     uint64 `json:"limit"`      // Gas Limit 上限
	} `json:"gas"`
}

// LoadConfig 从 JSON 文件加载配置，设置缺省值后返回。
func LoadConfig(path string) *Config {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("读取配置文件 %s 失败: %v", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("解析配置文件失败: %v", err)
	}
	if cfg.LocalRPCURL == "" {
		cfg.LocalRPCURL = "http://127.0.0.1:8545"
	}
	if cfg.Gas.PriceGwei == 0 {
		cfg.Gas.PriceGwei = 3
	}
	if cfg.Gas.Limit == 0 {
		cfg.Gas.Limit = 3_000_000
	}
	return &cfg
}

// MustParseKey 将 hex 私钥字符串（可带 0x 前缀）解析为 ECDSA 私钥。
func MustParseKey(hexKey string) *ecdsa.PrivateKey {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(hexKey, "0x"))
	if err != nil {
		log.Fatalf("解析私钥失败: %v", err)
	}
	return key
}

// ──────────────────────────────────────────────────────────────────────────────
// ABI 定义
//
// 这里手写最小化 ABI，仅包含实际调用的函数和事件。
// 好处：不依赖 abigen 代码生成，便于快速迭代；坏处：返回 tuple 类型需要用反射处理。
// ──────────────────────────────────────────────────────────────────────────────

// CtfABI 是 ConditionalTokens（Gnosis CTF）的最小 ABI。
// CTF 是 ERC1155，每个 (conditionId, indexSet) 对应一个 token ID：
//   - indexSet=1（二进制 01）→ YES token
//   - indexSet=2（二进制 10）→ NO  token
//
// splitPosition 将 USDC 锁入 CTF，铸造等量 YES+NO 代币。
// redeemPositions 在市场结算后，用胜出代币兑换回 USDC。
const CtfABI = `[
  {"type":"function","name":"prepareCondition","inputs":[{"name":"oracle","type":"address"},{"name":"questionId","type":"bytes32"},{"name":"outcomeSlotCount","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"reportPayouts","inputs":[{"name":"questionId","type":"bytes32"},{"name":"payouts","type":"uint256[]"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"getConditionId","inputs":[{"name":"oracle","type":"address"},{"name":"questionId","type":"bytes32"},{"name":"outcomeSlotCount","type":"uint256"}],"outputs":[{"name":"","type":"bytes32"}],"stateMutability":"pure"},
  {"type":"function","name":"getCollectionId","inputs":[{"name":"parentCollectionId","type":"bytes32"},{"name":"conditionId","type":"bytes32"},{"name":"indexSet","type":"uint256"}],"outputs":[{"name":"","type":"bytes32"}],"stateMutability":"pure"},
  {"type":"function","name":"getPositionId","inputs":[{"name":"collateralToken","type":"address"},{"name":"collectionId","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"pure"},
  {"type":"function","name":"splitPosition","inputs":[{"name":"collateralToken","type":"address"},{"name":"parentCollectionId","type":"bytes32"},{"name":"conditionId","type":"bytes32"},{"name":"partition","type":"uint256[]"},{"name":"amount","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"redeemPositions","inputs":[{"name":"collateralToken","type":"address"},{"name":"parentCollectionId","type":"bytes32"},{"name":"conditionId","type":"bytes32"},{"name":"indexSets","type":"uint256[]"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"setApprovalForAll","inputs":[{"name":"operator","type":"address"},{"name":"approved","type":"bool"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"},{"name":"id","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
  {"type":"function","name":"isApprovedForAll","inputs":[{"name":"account","type":"address"},{"name":"operator","type":"address"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"}
]`

// Erc20ABI 是 ERC20 USDC 的最小 ABI。
// 此处的 USDC 是 BSC Testnet 上的 ChildERC20，deposit() 用于铸造测试代币。
const Erc20ABI = `[
  {"type":"function","name":"approve","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
  {"type":"function","name":"transfer","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"deposit","inputs":[{"name":"user","type":"address"},{"name":"depositData","type":"bytes"}],"outputs":[],"stateMutability":"nonpayable"}
]`

// AdapterABI 是 UmaCtfAdapter 的最小 ABI。
//
// initialize() 是市场创建的入口，内部会：
//  1. 调用 CTF.prepareCondition() 创建条件
//  2. 调用 OO.requestPrice() 发起价格请求
//
// resolve() 是市场结算的入口，内部会：
//  1. 查询 OO.getPrice()
//  2. 根据价格构造 payouts 数组（1e18→YES，0→NO）
//  3. 调用 CTF.reportPayouts()
//
// getQuestion() 返回的是 tuple 类型，go-ethereum 解码为匿名 struct，
// 需要用反射按字段名（RequestTime 等）提取值，不能直接类型断言。
const AdapterABI = `[
  {"type":"constructor","inputs":[{"name":"_ctf","type":"address"},{"name":"_optimisticOracle","type":"address"},{"name":"_collateralWhitelist","type":"address"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"initialize","inputs":[{"name":"ancillaryData","type":"bytes"},{"name":"rewardToken","type":"address"},{"name":"reward","type":"uint256"},{"name":"proposalBond","type":"uint256"},{"name":"liveness","type":"uint64"}],"outputs":[{"name":"questionId","type":"bytes32"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"resolve","inputs":[{"name":"questionId","type":"bytes32"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"ready","inputs":[{"name":"questionId","type":"bytes32"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"},
  {"type":"function","name":"getConditionId","inputs":[{"name":"questionId","type":"bytes32"}],"outputs":[{"name":"","type":"bytes32"}],"stateMutability":"view"},
  {"type":"function","name":"getQuestion","inputs":[{"name":"questionId","type":"bytes32"}],"outputs":[{"name":"","type":"tuple","components":[{"name":"requestTime","type":"uint256"},{"name":"reward","type":"uint256"},{"name":"proposalBond","type":"uint256"},{"name":"liveness","type":"uint64"},{"name":"resolved","type":"bool"},{"name":"paused","type":"bool"},{"name":"flagged","type":"bool"},{"name":"flagTime","type":"uint256"},{"name":"rewardToken","type":"address"},{"name":"ancillaryData","type":"bytes"},{"name":"reset","type":"bool"}]}],"stateMutability":"view"},
  {"type":"event","name":"QuestionInitialized","inputs":[{"name":"questionId","type":"bytes32","indexed":true},{"name":"ancillaryData","type":"bytes","indexed":false},{"name":"creator","type":"address","indexed":true},{"name":"rewardToken","type":"address","indexed":false},{"name":"reward","type":"uint256","indexed":false},{"name":"proposalBond","type":"uint256","indexed":false},{"name":"liveness","type":"uint64","indexed":false}]},
  {"type":"event","name":"QuestionResolved","inputs":[{"name":"questionId","type":"bytes32","indexed":true},{"name":"price","type":"int256","indexed":false},{"name":"payouts","type":"uint256[]","indexed":false}]}
]`

// OoV2ABI 是 MockOptimisticOracleV2 的最小 ABI。
//
// OOv2 的价格请求以 (requester, identifier, timestamp, ancillaryData) 四元组为 key。
// 争议发生后，adapter 会用新的 timestamp（T2）发起新的请求，原来的请求（T1）进入终态。
// mockDvmSettle 仅在我们的 Mock 合约中存在，真实 OOv2 由 DVM 投票决定。
const OoV2ABI = `[
  {"type":"constructor","inputs":[{"name":"_dvm","type":"address"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"proposePrice","inputs":[{"name":"requester","type":"address"},{"name":"identifier","type":"bytes32"},{"name":"timestamp","type":"uint256"},{"name":"ancillaryData","type":"bytes"},{"name":"proposedPrice","type":"int256"}],"outputs":[{"name":"totalBond","type":"uint256"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"disputePrice","inputs":[{"name":"requester","type":"address"},{"name":"identifier","type":"bytes32"},{"name":"timestamp","type":"uint256"},{"name":"ancillaryData","type":"bytes"}],"outputs":[{"name":"totalBond","type":"uint256"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"settle","inputs":[{"name":"requester","type":"address"},{"name":"identifier","type":"bytes32"},{"name":"timestamp","type":"uint256"},{"name":"ancillaryData","type":"bytes"}],"outputs":[{"name":"payout","type":"uint256"}],"stateMutability":"nonpayable"},
  {"type":"function","name":"mockDvmSettle","inputs":[{"name":"requester","type":"address"},{"name":"identifier","type":"bytes32"},{"name":"timestamp","type":"uint256"},{"name":"ancillaryData","type":"bytes"},{"name":"resolution","type":"bool"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"hasPrice","inputs":[{"name":"requester","type":"address"},{"name":"identifier","type":"bytes32"},{"name":"timestamp","type":"uint256"},{"name":"ancillaryData","type":"bytes"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"},
  {"type":"function","name":"getPrice","inputs":[{"name":"requester","type":"address"},{"name":"identifier","type":"bytes32"},{"name":"timestamp","type":"uint256"},{"name":"ancillaryData","type":"bytes"}],"outputs":[{"name":"","type":"int256"}],"stateMutability":"view"}
]`

// WhitelistABI 是 MockAddressWhitelist 的 ABI。
// 真实环境中 AddressWhitelist 由 UMA 治理维护，限制可作为抵押品的代币。
// Mock 版本 isOnWhitelist() 恒返回 true，跳过白名单校验。
const WhitelistABI = `[
  {"type":"constructor","inputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"isOnWhitelist","inputs":[{"name":"addr","type":"address"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"}
]`

// ExchangeABI 是 CTFExchange 的最小 ABI。
//
// matchOrders 是核心函数，由 operator 调用实现原子撮合。
// 注意事项：
//  - 调用前必须先调用 registerToken() 注册代币对，否则会静默 revert（data=0x）
//  - nonces 使用精确匹配（NonceManager），订单的 Nonce 字段必须等于链上当前值
//  - 只有 operator 才能调用 matchOrders；admin 才能调用 registerToken
const ExchangeABI = `[
  {"type":"function","name":"matchOrders","inputs":[
    {"name":"takerOrder","type":"tuple","components":[
      {"name":"salt","type":"uint256"},{"name":"maker","type":"address"},{"name":"signer","type":"address"},
      {"name":"taker","type":"address"},{"name":"tokenId","type":"uint256"},{"name":"makerAmount","type":"uint256"},
      {"name":"takerAmount","type":"uint256"},{"name":"expiration","type":"uint256"},{"name":"nonce","type":"uint256"},
      {"name":"feeRateBps","type":"uint256"},{"name":"side","type":"uint8"},{"name":"signatureType","type":"uint8"},
      {"name":"signature","type":"bytes"}
    ]},
    {"name":"makerOrders","type":"tuple[]","components":[
      {"name":"salt","type":"uint256"},{"name":"maker","type":"address"},{"name":"signer","type":"address"},
      {"name":"taker","type":"address"},{"name":"tokenId","type":"uint256"},{"name":"makerAmount","type":"uint256"},
      {"name":"takerAmount","type":"uint256"},{"name":"expiration","type":"uint256"},{"name":"nonce","type":"uint256"},
      {"name":"feeRateBps","type":"uint256"},{"name":"side","type":"uint8"},{"name":"signatureType","type":"uint8"},
      {"name":"signature","type":"bytes"}
    ]},
    {"name":"takerFillAmount","type":"uint256"},
    {"name":"makerFillAmounts","type":"uint256[]"}
  ],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"isOperator","inputs":[{"name":"operator","type":"address"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"},
  {"type":"function","name":"isAdmin","inputs":[{"name":"admin","type":"address"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"},
  {"type":"function","name":"nonces","inputs":[{"name":"user","type":"address"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
  {"type":"function","name":"addOperator","inputs":[{"name":"operator","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"registerToken","inputs":[{"name":"token0","type":"uint256"},{"name":"token1","type":"uint256"},{"name":"conditionId","type":"bytes32"}],"outputs":[],"stateMutability":"nonpayable"},
  {"type":"function","name":"domainSeparator","inputs":[],"outputs":[{"name":"","type":"bytes32"}],"stateMutability":"view"}
]`

// ──────────────────────────────────────────────────────────────────────────────
// EIP-712 签名
//
// CTFExchange 使用 EIP-712 结构化签名验证订单。订单在链下签名，链上通过
// ecrecover 验证 maker 地址，防止伪造。
// ──────────────────────────────────────────────────────────────────────────────

// OrderTypeHash 是 EIP-712 订单类型的 typehash，由 Order struct 的字段名和类型决定。
// 如果合约升级改变了 Order 结构，此 hash 也必须同步更新。
var OrderTypeHash = crypto.Keccak256Hash([]byte(
	"Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)",
))

// CTFOrder 表示一个 Polymarket CTFExchange 订单。
//   - Salt: 随机数，防止相同内容的订单产生相同 hash
//   - Maker/Signer: 通常相同（EOA 直接签名时）；代理钱包模式下 Maker=用户, Signer=代理
//   - Taker: 零地址表示任何人都可以作为对手方
//   - TokenId: ERC1155 代币 ID（YES 或 NO token 的 positionId）
//   - MakerAmount/TakerAmount: maker 提供/期望获得的数量（SELL 时提供代币，BUY 时提供 USDC）
//   - Side: 0=BUY, 1=SELL
//   - SignatureType: 0=EOA（标准 ECDSA），1=POLY_PROXY（代理钱包），2=POLY_GNOSIS（多签）
type CTFOrder struct {
	Salt          *big.Int
	Maker         common.Address
	Signer        common.Address
	Taker         common.Address
	TokenId       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Expiration    *big.Int
	Nonce         *big.Int
	FeeRateBps    *big.Int
	Side          uint8 // 0=BUY, 1=SELL
	SignatureType uint8 // 0=EOA
	Signature     []byte
}

// OrderTuple 是 CTFOrder 的镜像结构，用于 go-ethereum ABI 编码。
// go-ethereum 的 Transact() 方法要求 tuple 参数为 struct 类型（而非指针），
// 所以需要一个单独的值类型。
type OrderTuple struct {
	Salt          *big.Int
	Maker         common.Address
	Signer        common.Address
	Taker         common.Address
	TokenId       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Expiration    *big.Int
	Nonce         *big.Int
	FeeRateBps    *big.Int
	Side          uint8
	SignatureType uint8
	Signature     []byte
}

// ToOrderTuple 将 CTFOrder 转为 ABI 编码用的 OrderTuple。
func ToOrderTuple(o *CTFOrder) OrderTuple {
	return OrderTuple{
		Salt: o.Salt, Maker: o.Maker, Signer: o.Signer, Taker: o.Taker,
		TokenId: o.TokenId, MakerAmount: o.MakerAmount, TakerAmount: o.TakerAmount,
		Expiration: o.Expiration, Nonce: o.Nonce, FeeRateBps: o.FeeRateBps,
		Side: o.Side, SignatureType: o.SignatureType, Signature: o.Signature,
	}
}

// ComputeDomainSeparator 计算 EIP-712 domain separator。
//
// domain separator 将签名绑定到特定合约和链，防止跨链/跨合约重放攻击。
// CTFExchange 的 domain 参数：
//   - name    = "Polymarket CTF Exchange"
//   - version = "1"
//   - chainId = 实际链 ID（BSC Testnet = 97）
//   - verifyingContract = CTFExchange 合约地址
//
// 注意：buf 手动构造 ABI 编码（每个字段 32 字节，地址右对齐到后 20 字节）。
func ComputeDomainSeparator(chainID *big.Int, verifyingContract common.Address) common.Hash {
	domainTypeHash := crypto.Keccak256Hash([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))
	nameHash := crypto.Keccak256Hash([]byte("Polymarket CTF Exchange"))
	versionHash := crypto.Keccak256Hash([]byte("1"))

	// 5 个字段，每个 32 字节
	buf := make([]byte, 5*32)
	copy(buf[0:32], domainTypeHash.Bytes())
	copy(buf[32:64], nameHash.Bytes())
	copy(buf[64:96], versionHash.Bytes())
	chainID.FillBytes(buf[96:128])
	// address 是 20 字节，在 32 字节槽中右对齐（偏移 12 字节）
	copy(buf[140:160], verifyingContract.Bytes())
	return crypto.Keccak256Hash(buf)
}

// HashOrder 计算订单的 EIP-712 结构体 hash（structHash）。
//
// 布局：13 个字段，每个 32 字节（uint8 也填充为 32 字节）。
// address 类型右对齐到 32 字节槽（偏移 12 字节）。
func HashOrder(order *CTFOrder) common.Hash {
	buf := make([]byte, 13*32)
	copy(buf[0:32], OrderTypeHash.Bytes())
	order.Salt.FillBytes(buf[32:64])
	// address: 32 字节槽，地址占后 20 字节（[12:32] 范围，即 buf[slot*32+12 : slot*32+32]）
	copy(buf[76:96], order.Maker.Bytes())
	copy(buf[108:128], order.Signer.Bytes())
	copy(buf[140:160], order.Taker.Bytes())
	order.TokenId.FillBytes(buf[160:192])
	order.MakerAmount.FillBytes(buf[192:224])
	order.TakerAmount.FillBytes(buf[224:256])
	order.Expiration.FillBytes(buf[256:288])
	order.Nonce.FillBytes(buf[288:320])
	order.FeeRateBps.FillBytes(buf[320:352])
	new(big.Int).SetUint64(uint64(order.Side)).FillBytes(buf[352:384])
	new(big.Int).SetUint64(uint64(order.SignatureType)).FillBytes(buf[384:416])
	return crypto.Keccak256Hash(buf)
}

// SignOrder 对订单进行 EIP-712 签名，将签名写入 order.Signature。
//
// 完整签名流程：
//  1. 计算 domainSeparator（绑定到合约和链）
//  2. 计算 orderHash（structHash）
//  3. 按 EIP-712 组装: keccak256("\x19\x01" || domainSep || orderHash)
//  4. 用私钥 ECDSA 签名
//  5. 将 v 值从 {0,1} 转为 {27,28}（以太坊传统格式，CTFExchange 要求此格式）
//  6. 本地验证签名，确保 recover 回来的地址与 order.Signer 一致
func SignOrder(order *CTFOrder, key *ecdsa.PrivateKey, chainID *big.Int, exchangeAddr common.Address) error {
	domainSep := ComputeDomainSeparator(chainID, exchangeAddr)
	orderHash := HashOrder(order)

	// EIP-712 前缀：\x19\x01
	data := make([]byte, 2+32+32)
	data[0] = 0x19
	data[1] = 0x01
	copy(data[2:34], domainSep.Bytes())
	copy(data[34:66], orderHash.Bytes())

	finalHash := crypto.Keccak256(data)
	sig, err := crypto.Sign(finalHash, key)
	if err != nil {
		return err
	}
	// go-ethereum 返回 v∈{0,1}，以太坊合约期望 v∈{27,28}
	sig[64] += 27
	order.Signature = sig

	// 本地验证：恢复公钥并检查地址，提前发现签名错误，避免上链后才发现
	sigForRecover := make([]byte, 65)
	copy(sigForRecover, sig)
	sigForRecover[64] -= 27 // SigToPub 需要 v∈{0,1}
	recovered, err := crypto.SigToPub(finalHash, sigForRecover)
	if err != nil {
		return fmt.Errorf("签名验证失败: %v", err)
	}
	if recoveredAddr := crypto.PubkeyToAddress(*recovered); recoveredAddr != order.Signer {
		return fmt.Errorf("签名不匹配: recovered=%s expected=%s", recoveredAddr.Hex(), order.Signer.Hex())
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// 工具函数
// ──────────────────────────────────────────────────────────────────────────────

// Artifact 对应 forge build 输出的 JSON 结构（out/<Contract>.sol/<Contract>.json）。
type Artifact struct {
	ABI      json.RawMessage `json:"abi"`
	Bytecode struct {
		Object string `json:"object"`
	} `json:"bytecode"`
}

// LoadBytecode 从 forge 编译产物读取合约字节码。
// 路径相对于 exchange/ 目录（程序运行目录），指向 ../contracts/out/。
// 如果编译产物不存在，请先执行：cd ../contracts && forge build
func LoadBytecode(solFile, contractName string) ([]byte, error) {
	path := filepath.Join("..", "contracts", "out", solFile+".sol", contractName+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败（请先运行 cd ../contracts && forge build）: %w", contractName, err)
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, fmt.Errorf("解析 %s artifact: %w", contractName, err)
	}
	hexStr := strings.TrimPrefix(artifact.Bytecode.Object, "0x")
	return hexutil.Decode("0x" + hexStr)
}

// MustParseABI 将 JSON 字符串解析为 abi.ABI，解析失败则 panic。
func MustParseABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(fmt.Sprintf("解析 ABI: %v", err))
	}
	return a
}

// GasCfg 由 RunCommonSetup 初始化，供所有 NewAuth 调用使用。
// 统一 Gas 配置可避免因 Gas 估算差异导致部分交易失败。
var GasCfg struct {
	PriceWei *big.Int
	Limit    uint64
}

// NewAuth 创建一个带固定 Gas 配置的交易签名者。
// 每次创建都需要查询链上 chainID，略有 RPC 开销，但确保正确性。
func NewAuth(client *ethclient.Client, key *ecdsa.PrivateKey) *bind.TransactOpts {
	chainID, _ := client.ChainID(context.Background())
	auth, err := bind.NewKeyedTransactorWithChainID(key, chainID)
	if err != nil {
		log.Fatalf("newAuth: %v", err)
	}
	auth.GasPrice = GasCfg.PriceWei
	auth.GasLimit = GasCfg.Limit
	return auth
}

// Deploy 部署合约并等待交易确认，返回合约地址和绑定对象。
func Deploy(client *ethclient.Client, auth *bind.TransactOpts, contractABI abi.ABI, bytecode []byte, args ...interface{}) (common.Address, *bind.BoundContract) {
	addr, tx, contract, err := bind.DeployContract(auth, contractABI, bytecode, client, args...)
	if err != nil {
		log.Fatalf("部署失败: %v", err)
	}
	bind.WaitMined(context.Background(), client, tx)
	return addr, contract
}

// Send 发送状态变更交易并等待上链确认。
// 如果交易 revert（receipt.Status == 0），打印 txHash 后 Fatal 退出。
// 调试技巧：将 txHash 粘贴到 BSCScan 查看具体 revert 原因。
func Send(client *ethclient.Client, auth *bind.TransactOpts, contract *bind.BoundContract, method string, args ...interface{}) *types.Receipt {
	tx, err := contract.Transact(auth, method, args...)
	if err != nil {
		log.Fatalf("%s 交易失败: %v", method, err)
	}
	receipt, err := bind.WaitMined(context.Background(), client, tx)
	if err != nil {
		log.Fatalf("%s 等待确认失败: %v", method, err)
	}
	if receipt.Status == 0 {
		log.Fatalf("%s 链上 revert（txHash: %s）", method, tx.Hash().Hex())
	}
	return receipt
}

// CallView 执行只读调用（eth_call），带 5 次重试。
//
// BSC Testnet 公共 RPC 节点不稳定，随机返回 HTTP 500；Alchemy 节点也偶发。
// 加入重试可大幅提高脚本在测试网上的可靠性。
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

// ToUsdc 将 USDC 金额（人类可读的浮点数）转为链上的 uint256（6位精度）。
// 例：ToUsdc(100.5) → 100_500_000
func ToUsdc(f float64) *big.Int {
	i, _ := new(big.Float).SetFloat64(f * 1e6).Int(nil)
	return i
}

// FromUsdc 将链上 uint256 USDC 值转为人类可读的浮点数。
// 例：FromUsdc(100_500_000) → 100.5
func FromUsdc(i *big.Int) float64 {
	f, _ := new(big.Float).SetInt(i).Float64()
	return f / 1e6
}

// Div 打印一条分隔线，用于在控制台输出中标记步骤边界。
func Div(line string) {
	fmt.Printf("\n%s\n  %s\n%s\n", strings.Repeat("─", 60), line, strings.Repeat("─", 60))
}

// EvmIncreaseTime 通过 Hardhat/Anvil 的 RPC 扩展方法快速推进本地链时间。
// 仅在本地测试节点（LocalMode=true）下有效，BSC Testnet 不支持此方法。
func EvmIncreaseTime(client *ethclient.Client, seconds int64) {
	var r interface{}
	client.Client().Call(&r, "evm_increaseTime", seconds)
	client.Client().Call(&r, "evm_mine")
}

// WaitLiveness 等待 liveness 窗口结束。
//   - 本地模式：调用 evm_increaseTime 瞬间推进时间（测试快速运行）
//   - 测试网模式：真实等待 liveness 秒，每 15 秒打印进度
func WaitLiveness(client *ethclient.Client, cfg *Config) {
	if cfg.LocalMode {
		fmt.Print("→ 本地模式: evm_increaseTime...")
		EvmIncreaseTime(client, int64(cfg.Market.LivenessSeconds)+10)
		fmt.Println(" ✓")
	} else {
		liveness := cfg.Market.LivenessSeconds
		fmt.Printf("→ 等待 %d 秒...", liveness)
		for i := uint64(0); i < liveness; i++ {
			time.Sleep(time.Second)
			if i%15 == 14 {
				fmt.Printf(" %ds", i+1)
			}
		}
		fmt.Println(" ✓")
	}
}

// ExtractBytes32FromReceipt 从交易收据的事件日志中提取指定位置的 indexed bytes32 字段。
// topicIndex=0 对应事件的第一个 indexed 参数（Topics[1]，因为 Topics[0] 是事件 ID）。
func ExtractBytes32FromReceipt(receipt *types.Receipt, contractABI abi.ABI, eventName string, topicIndex int) [32]byte {
	event, ok := contractABI.Events[eventName]
	if !ok {
		log.Fatalf("事件 %s 未在 ABI 中找到", eventName)
	}
	for _, l := range receipt.Logs {
		if len(l.Topics) < 1 || l.Topics[0] != event.ID {
			continue
		}
		if len(l.Topics) < topicIndex+2 {
			continue
		}
		var result [32]byte
		copy(result[:], l.Topics[topicIndex+1].Bytes())
		return result
	}
	log.Fatalf("事件 %s 未在收据中找到", eventName)
	return [32]byte{}
}

// ──────────────────────────────────────────────────────────────────────────────
// MarketContext：公共流程（步骤 1-5）的产物
// ──────────────────────────────────────────────────────────────────────────────

// MarketContext 持有步骤 1-5 执行后的完整链上状态，传递给各演示程序继续执行。
type MarketContext struct {
	Client  *ethclient.Client
	ChainID *big.Int
	Cfg     *Config

	// 私钥（用于构造交易签名者）
	DeployerKey *ecdsa.PrivateKey
	User1Key    *ecdsa.PrivateKey
	User2Key    *ecdsa.PrivateKey
	OperatorKey *ecdsa.PrivateKey

	// 地址
	DeployerAddr common.Address
	User1Addr    common.Address
	User2Addr    common.Address
	OperatorAddr common.Address

	// 合约地址（部分固定来自 config，部分每次部署后获得）
	CTFAddr       common.Address // 固定，来自 config
	ExchangeAddr  common.Address // 固定，来自 config
	USDCAddr      common.Address // 固定，来自 config
	WhitelistAddr common.Address // 每次部署
	OOAddr        common.Address // 每次部署
	AdapterAddr   common.Address // 每次部署

	// go-ethereum 合约绑定（用于发交易和读取状态）
	CTFContract      *bind.BoundContract
	USDCContract     *bind.BoundContract
	ExchangeContract *bind.BoundContract
	OOContract       *bind.BoundContract
	AdapterContract  *bind.BoundContract

	// 市场状态
	QuestionId  [32]byte // adapter.initialize() 返回的问题 ID
	ConditionId [32]byte // CTF 中的条件 ID，由 (adapter地址, questionId, 2) 计算
	// RequestTime 是 OOv2 价格请求的时间戳（initialize 区块时间）。
	// 争议发生后，adapter 会将其更新为新的区块时间（T2），
	// 后续的 proposePrice/settle 必须使用新值。
	RequestTime   *big.Int
	AncillaryData []byte   // 问题描述字节（UTF-8），OOv2 请求的一部分
	ProposalBond  *big.Int // 提案/质疑所需的 USDC 保证金
	Identifier    [32]byte // "YES_OR_NO_QUERY"（右填充零到 32 字节）
	YesTokenId    *big.Int // ERC1155 YES token 的 positionId
	NoTokenId     *big.Int // ERC1155 NO  token 的 positionId
}

// RunCommonSetup 执行步骤 1-5，返回包含所有链上状态的 MarketContext。
//
// 步骤概述：
//  1. 部署 MockAddressWhitelist / MockOOv2 / UmaCtfAdapter
//  2. 铸造测试 USDC（User1/User2 各 10000）
//  3. 初始化市场（adapter.initialize → OO requestPrice → CTF prepareCondition）
//  4. 拆分头寸（splitPosition：每人 1000 USDC → 1000 YES + 1000 NO）
//  5. 订单撮合（User1 SELL 1000 NO，User2 BUY 1000 NO，价格 0.5 USDC/个）
func RunCommonSetup(configPath string) *MarketContext {
	cfg := LoadConfig(configPath)
	fmt.Printf("✓ 配置已加载: %s\n", configPath)

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

	// 从 config 加载已有合约地址（CTF 和 Exchange 在 BSC Testnet 已部署，无需重新部署）
	ctfAddr := common.HexToAddress(cfg.Contracts.CTF)
	exchangeAddr := common.HexToAddress(cfg.Contracts.Exchange)
	usdcAddr := common.HexToAddress(cfg.Contracts.USDC)

	ctfABIParsed := MustParseABI(CtfABI)
	erc20ABIParsed := MustParseABI(Erc20ABI)
	adapterABIParsed := MustParseABI(AdapterABI)
	ooABIParsed := MustParseABI(OoV2ABI)
	whitelistABIParsed := MustParseABI(WhitelistABI)
	exchangeABIParsed := MustParseABI(ExchangeABI)

	ctfContract := bind.NewBoundContract(ctfAddr, ctfABIParsed, client, client, client)
	usdcContract := bind.NewBoundContract(usdcAddr, erc20ABIParsed, client, client, client)
	exchangeContract := bind.NewBoundContract(exchangeAddr, exchangeABIParsed, client, client, client)

	// ── 步骤 1：部署自定义合约 ───────────────────────────────────────────────
	// 每次测试都全新部署 MockAddressWhitelist + MockOOv2 + UmaCtfAdapter，
	// 保证状态干净，不受上次测试残留影响。
	Div("步骤 1: 部署 MockOOv2 / MockAddressWhitelist / UmaCtfAdapter")
	deployerAuth := NewAuth(client, deployerKey)

	whitelistBytecode, err := LoadBytecode("MockAddressWhitelist", "MockAddressWhitelist")
	if err != nil {
		log.Fatal(err)
	}
	whitelistAddr, _ := Deploy(client, deployerAuth, whitelistABIParsed, whitelistBytecode)
	fmt.Printf("✓ MockAddressWhitelist: %s\n", whitelistAddr.Hex())

	ooBytecode, err := LoadBytecode("MockOOv2", "MockOptimisticOracleV2")
	if err != nil {
		log.Fatal(err)
	}
	// MockOOv2 构造函数接收 _dvm 地址，这里用 deployer 充当 DVM
	ooAddr, ooContract := Deploy(client, deployerAuth, ooABIParsed, ooBytecode, deployerAddr)
	fmt.Printf("✓ MockOOv2:             %s\n", ooAddr.Hex())

	adapterBytecode, err := LoadBytecode("UmaCtfAdapter", "UmaCtfAdapter")
	if err != nil {
		log.Fatal(err)
	}
	// UmaCtfAdapter 构造函数：(_ctf, _optimisticOracle, _collateralWhitelist)
	adapterAddr, adapterContract := Deploy(client, deployerAuth, adapterABIParsed, adapterBytecode,
		ctfAddr, ooAddr, whitelistAddr)
	fmt.Printf("✓ UmaCtfAdapter:        %s\n", adapterAddr.Hex())

	// 检查 operator 权限（CTFExchange 的 operator 才能调用 matchOrders）
	// operator 是针对整个 Exchange 合约的，不是单个市场，所以只需注册一次
	var opCheck []interface{}
	CallView(exchangeContract, "isOperator", &opCheck, operatorAddr)
	if !opCheck[0].(bool) {
		Send(client, deployerAuth, exchangeContract, "addOperator", operatorAddr)
		fmt.Printf("✓ 已注册 operator: %s\n", operatorAddr.Hex())
	} else {
		fmt.Println("✓ operator 权限已存在")
	}

	// ── 步骤 2：铸造 USDC ────────────────────────────────────────────────────
	// BSC Testnet 的 USDC 是 ChildERC20（来自 Polygon Bridge），
	// deposit(user, amount_as_32bytes) 由 MinterRole 持有者（deployer）铸造。
	Div("步骤 2: 铸造测试 USDC")
	mintAmount := ToUsdc(10000)
	for _, info := range []struct {
		addr common.Address
		name string
	}{{user1Addr, "User1"}, {user2Addr, "User2"}} {
		// deposit 的第二个参数是 ABI 编码的 uint256（32 字节大端）
		depositData := make([]byte, 32)
		mintAmount.FillBytes(depositData)
		Send(client, deployerAuth, usdcContract, "deposit", info.addr, depositData)
		var bal []interface{}
		CallView(usdcContract, "balanceOf", &bal, info.addr)
		fmt.Printf("✓ %s USDC 余额: %.2f\n", info.name, FromUsdc(bal[0].(*big.Int)))
	}

	// ── 步骤 3：初始化市场 ───────────────────────────────────────────────────
	// adapter.initialize() 内部做三件事：
	//  1. 生成 questionId = keccak256(ancillaryData)（简化版，实际 adapter 有更复杂的计算）
	//  2. 调用 CTF.prepareCondition(adapter, questionId, 2) 创建 YES/NO 两个 slot
	//  3. 调用 OO.requestPrice(identifier, block.timestamp, ancillaryData) 发起价格请求
	//
	// requestTime 取自 initialize 交易所在区块的时间戳（block.timestamp），
	// 后续 proposePrice/settle 必须使用相同的 requestTime。
	Div("步骤 3: 初始化市场（adapter.initialize）")
	ancillaryData := []byte(cfg.Market.AncillaryData)
	proposalBond := ToUsdc(cfg.Market.ProposalBondUSDC)
	liveness := cfg.Market.LivenessSeconds

	fmt.Printf("  问题原文:     %s\n", cfg.Market.AncillaryData)
	fmt.Printf("  proposalBond: %s USDC\n", proposalBond)
	fmt.Printf("  liveness:     %d 秒\n", liveness)

	receipt := Send(client, deployerAuth, adapterContract, "initialize",
		ancillaryData, usdcAddr, big.NewInt(0), proposalBond, liveness)

	// questionId 从 QuestionInitialized 事件的第一个 indexed 参数提取
	questionId := ExtractBytes32FromReceipt(receipt, adapterABIParsed, "QuestionInitialized", 0)
	fmt.Printf("✓ questionId:  0x%x\n", questionId)

	// requestTime = initialize 区块的时间戳，带重试以防 RPC 不稳定
	var initHeader *types.Header
	for i := 0; i < 5; i++ {
		initHeader, err = client.HeaderByNumber(context.Background(), receipt.BlockNumber)
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("获取初始化区块头: %v", err)
	}
	requestTime := new(big.Int).SetUint64(initHeader.Time)
	fmt.Printf("✓ requestTime: %s\n", requestTime)

	// 从 adapter 查询 conditionId（由 CTF.getConditionId(adapter, questionId, 2) 计算）
	var condResult []interface{}
	CallView(adapterContract, "getConditionId", &condResult, questionId)
	conditionId := condResult[0].([32]byte)
	fmt.Printf("✓ conditionId: 0x%x\n", conditionId)

	// 计算 YES/NO 的 ERC1155 tokenId：
	//   collectionId = CTF.getCollectionId(bytes32(0), conditionId, indexSet)
	//   positionId   = CTF.getPositionId(collateralToken, collectionId)
	//
	// indexSet 是位掩码：YES=1（0b01），NO=2（0b10）
	// positionId 是 CTFExchange 订单中的 tokenId 字段
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

	// 注册代币对（关键步骤，容易漏掉！）
	// CTFExchange.matchOrders() 在撮合前会校验 tokenId 是否已注册；
	// 未注册时合约直接 revert，且 revert data 为空（0x），难以定位原因。
	// registerToken 需要 admin 权限（deployer 在 Exchange 部署时默认是 admin）。
	var isAdminResult []interface{}
	CallView(exchangeContract, "isAdmin", &isAdminResult, deployerAddr)
	if isAdminResult[0].(bool) {
		var condBytes [32]byte
		copy(condBytes[:], conditionId[:])
		Send(client, NewAuth(client, deployerKey), exchangeContract, "registerToken",
			yesTokenId, noTokenId, condBytes)
		fmt.Println("✓ registerToken 完成")
	}

	// ── 步骤 4：拆分头寸 ─────────────────────────────────────────────────────
	// splitPosition 将 USDC 锁入 CTF 合约，铸造等量 YES+NO ERC1155 代币。
	// partition=[1,2] 表示将条件分成两个互斥 slot（YES 和 NO），各占 1 份。
	// 用户还需要 setApprovalForAll(Exchange, true)，授权 Exchange 转移其 ERC1155 代币（用于撮合）。
	Div("步骤 4: 拆分 USDC → YES/NO 代币（CTF.splitPosition）")
	splitAmount := ToUsdc(1000)
	partition := []*big.Int{big.NewInt(1), big.NewInt(2)}

	for _, info := range []struct {
		addr common.Address
		name string
		key  *ecdsa.PrivateKey
	}{{user1Addr, "User1", user1Key}, {user2Addr, "User2", user2Key}} {
		auth := NewAuth(client, info.key)
		// USDC 授权给 CTF（splitPosition 时 CTF 会从用户拉取 USDC）
		Send(client, auth, usdcContract, "approve", ctfAddr, splitAmount)
		// ERC1155 全局授权给 Exchange（matchOrders 时 Exchange 会转移代币）
		Send(client, auth, ctfContract, "setApprovalForAll", exchangeAddr, true)
		Send(client, auth, ctfContract, "splitPosition",
			usdcAddr, [32]byte{}, conditionId, partition, splitAmount)
		var yBal, nBal []interface{}
		CallView(ctfContract, "balanceOf", &yBal, info.addr, yesTokenId)
		CallView(ctfContract, "balanceOf", &nBal, info.addr, noTokenId)
		fmt.Printf("✓ %s: YES=%s  NO=%s\n", info.name, yBal[0].(*big.Int), nBal[0].(*big.Int))
	}

	// ── 步骤 5：订单撮合 ─────────────────────────────────────────────────────
	// 场景：User1（持有 YES+NO）以 0.5 USDC/个 卖出 1000 NO，
	//       User2（持有 YES+NO）以 0.5 USDC/个 买入 1000 NO。
	// 撮合后：User1 持有 YES 和 USDC；User2 持有 YES+NO（2000 NO）
	//
	// 订单在链下 EIP-712 签名，由 operator 调用 matchOrders 上链结算（混合模式）。
	Div("步骤 5: 订单撮合（CTFExchange.matchOrders）")
	fmt.Println("  User1 SELL 1000 NO @ 0.5 USDC，User2 BUY 1000 NO @ 0.5 USDC")

	// BUY 方（User2）需要预先 approve USDC 给 Exchange（撮合时 Exchange 从 User2 拉取 USDC）
	Send(client, NewAuth(client, user2Key), usdcContract, "approve", exchangeAddr, ToUsdc(500))

	tradeAmount := splitAmount // 1000e6（NO token 数量）
	usdcAmount := ToUsdc(500)  // 500e6（USDC 数量，1000 NO * 0.5 USDC/个）
	expiry := big.NewInt(time.Now().Unix() + 3600)

	// 关键：签名前先从链上读取当前 nonce。
	// CTFExchange NonceManager 使用精确匹配（nonces[maker] == order.nonce），
	// 而不是传统的单调递增（>=）。用户初始 nonce 为 0，不能假设是 1。
	var u1Nonce, u2Nonce []interface{}
	CallView(exchangeContract, "nonces", &u1Nonce, user1Addr)
	CallView(exchangeContract, "nonces", &u2Nonce, user2Addr)

	// makerOrder：User1 SELL NO
	//   makerAmount = 1000e6（User1 提供 NO token 数量）
	//   takerAmount = 500e6 （User1 期望获得的 USDC）
	//   Side = 1（SELL）
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

	// takerOrder：User2 BUY NO
	//   makerAmount = 500e6 （User2 提供的 USDC）
	//   takerAmount = 1000e6（User2 期望获得的 NO token 数量）
	//   Side = 0（BUY）
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

	// matchOrders 由 operator 调用（非 maker/taker）：
	//   takerFillAmount = 500e6（本次撮合 User2 出的 USDC 数量）
	//   makerFillAmounts = [1000e6]（本次撮合 User1 出的 NO token 数量）
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

	// identifier 是 OOv2 的价格请求标识符，固定为 "YES_OR_NO_QUERY"（右填充零到 32 字节）
	identifier := [32]byte{}
	copy(identifier[:], []byte("YES_OR_NO_QUERY"))

	return &MarketContext{
		Client: client, ChainID: chainID, Cfg: cfg,
		DeployerKey: deployerKey, User1Key: user1Key,
		User2Key: user2Key, OperatorKey: operatorKey,
		DeployerAddr: deployerAddr, User1Addr: user1Addr,
		User2Addr: user2Addr, OperatorAddr: operatorAddr,
		CTFAddr: ctfAddr, ExchangeAddr: exchangeAddr, USDCAddr: usdcAddr,
		WhitelistAddr: whitelistAddr, OOAddr: ooAddr, AdapterAddr: adapterAddr,
		CTFContract: ctfContract, USDCContract: usdcContract,
		ExchangeContract: exchangeContract, OOContract: ooContract,
		AdapterContract: adapterContract,
		QuestionId: questionId, ConditionId: conditionId,
		RequestTime: requestTime, AncillaryData: ancillaryData,
		ProposalBond: proposalBond, Identifier: identifier,
		YesTokenId: yesTokenId, NoTokenId: noTokenId,
	}
}
