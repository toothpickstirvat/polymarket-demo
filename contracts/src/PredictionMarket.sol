// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title PredictionMarket
 * @notice 简化版预测市场合约，演示 UMA OOv3 集成的完整流程。
 *
 * 功能：
 *   - createMarket()        创建二元预测市场（YES/NO）
 *   - betYes() / betNo()    用户下注
 *   - proposeResolution()   市场结束后，任何人提案结果（调用 OOv3.assertTruth）
 *   - settle()              liveness 结束后结算（无质疑路径）
 *   - claimWinnings()       赢家领奖
 *
 * OOv3 回调（由 oracle 合约调用）：
 *   - assertionResolvedCallback()  结算回调
 *   - assertionDisputedCallback()  质疑回调
 *
 * 部署：
 *   PredictionMarket(oracleAddress, usdcAddress)
 *
 * 生产迁移：将 oracleAddress 替换为真实 UMA OOv3 地址。
 */

interface IERC20 {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function transfer(address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

interface IOptimisticOracleV3 {
    function assertTruth(
        bytes  memory claim,
        address asserter,
        address callbackRecipient,
        address escalationManager,
        uint64  liveness,
        address currency,
        uint256 bond,
        bytes32 identifier,
        bytes32 domainId
    ) external returns (bytes32 assertionId);

    function settleAssertion(bytes32 assertionId) external;
    function getAssertionResult(bytes32 assertionId) external view returns (bool);
}

contract PredictionMarket {
    // ─── 数据结构 ───────────────────────────────────────────────────────────

    enum MarketState {
        Active,     // 开放下注
        Asserting,  // 已提案，等待 liveness
        Disputed,   // 被质疑，等待 DVM 仲裁
        Resolved    // 最终结果确认，可领奖
    }

    struct Market {
        string      description;      // 市场描述（问题）
        uint256     endTime;          // 下注截止时间
        uint256     totalYesBets;     // YES 总下注额
        uint256     totalNoBets;      // NO 总下注额
        MarketState state;
        bool        resolution;       // true=YES赢, false=NO赢
        bytes32     assertionId;      // 关联的 OOv3 assertion ID
        address     proposer;         // 提案者地址（用于退还或没收 bond）
        bool        proposedResult;   // 提案的结果
    }

    // ─── 状态变量 ──────────────────────────────────────────────────────────

    IOptimisticOracleV3 public immutable oracle;
    IERC20              public immutable usdc;

    /// @notice 提案 bond 金额（100 USDC，6 位精度）
    uint256 public constant BOND = 100e6;

    /// @notice liveness 时间（120 秒，测试用；生产建议 7200 秒）
    uint64 public constant LIVENESS = 120;

    /// @notice OOv3 identifier
    bytes32 public constant ASSERT_TRUTH = bytes32("ASSERT_TRUTH");

    mapping(bytes32 => Market)              public markets;
    mapping(bytes32 => mapping(address => uint256)) public yesBets;
    mapping(bytes32 => mapping(address => uint256)) public noBets;
    mapping(bytes32 => bytes32)             public assertionToMarket;

    // ─── 事件 ──────────────────────────────────────────────────────────────

    event MarketCreated(
        bytes32 indexed marketId,
        string  description,
        uint256 endTime
    );
    event BetPlaced(
        bytes32 indexed marketId,
        address indexed bettor,
        bool    isYes,
        uint256 amount
    );
    event ResolutionProposed(
        bytes32 indexed marketId,
        bytes32 indexed assertionId,
        address proposer,
        bool    proposedResult
    );
    event MarketDisputed(bytes32 indexed marketId, bytes32 indexed assertionId);
    event MarketResolved(bytes32 indexed marketId, bool resolution);
    event WinningsClaimed(bytes32 indexed marketId, address indexed claimer, uint256 amount);

    // ─── 构造函数 ──────────────────────────────────────────────────────────

    constructor(address _oracle, address _usdc) {
        oracle = IOptimisticOracleV3(_oracle);
        usdc   = IERC20(_usdc);
    }

    // ─── 市场管理 ───────────────────────────────────────────────────────────

    /**
     * @notice 创建预测市场
     * @param description  市场描述，例如 "Will BNB exceed $1000 by 2025-12-31?"
     * @param endTime      下注截止时间（Unix timestamp）
     * @return marketId    市场唯一标识
     */
    function createMarket(
        string memory description,
        uint256 endTime
    ) external returns (bytes32 marketId) {
        require(endTime > block.timestamp, "End time must be in future");

        marketId = keccak256(abi.encodePacked(description, endTime, block.timestamp, msg.sender));

        markets[marketId] = Market({
            description:    description,
            endTime:        endTime,
            totalYesBets:   0,
            totalNoBets:    0,
            state:          MarketState.Active,
            resolution:     false,
            assertionId:    bytes32(0),
            proposer:       address(0),
            proposedResult: false
        });

        emit MarketCreated(marketId, description, endTime);
    }

    // ─── 下注函数 ───────────────────────────────────────────────────────────

    /**
     * @notice 押 YES
     * @dev 调用前需 approve USDC 给本合约
     */
    function betYes(bytes32 marketId, uint256 amount) external {
        Market storage m = markets[marketId];
        require(m.endTime != 0,                 "Market not found");
        require(m.state == MarketState.Active,  "Market not active");
        require(block.timestamp < m.endTime,    "Market ended");
        require(amount > 0,                     "Amount must be > 0");

        usdc.transferFrom(msg.sender, address(this), amount);
        yesBets[marketId][msg.sender] += amount;
        m.totalYesBets += amount;

        emit BetPlaced(marketId, msg.sender, true, amount);
    }

    /**
     * @notice 押 NO
     * @dev 调用前需 approve USDC 给本合约
     */
    function betNo(bytes32 marketId, uint256 amount) external {
        Market storage m = markets[marketId];
        require(m.endTime != 0,                 "Market not found");
        require(m.state == MarketState.Active,  "Market not active");
        require(block.timestamp < m.endTime,    "Market ended");
        require(amount > 0,                     "Amount must be > 0");

        usdc.transferFrom(msg.sender, address(this), amount);
        noBets[marketId][msg.sender] += amount;
        m.totalNoBets += amount;

        emit BetPlaced(marketId, msg.sender, false, amount);
    }

    // ─── 解析流程 ───────────────────────────────────────────────────────────

    /**
     * @notice 提案市场结果（市场结束后任何人可调用）
     * @dev 调用前需 approve 100 USDC（bond）给本合约
     *
     * @param marketId  市场 ID
     * @param result    提案结果（true=YES赢，false=NO赢）
     */
    function proposeResolution(bytes32 marketId, bool result) external {
        Market storage m = markets[marketId];
        require(m.endTime != 0,                 "Market not found");
        require(m.state == MarketState.Active,  "Market not in Active state");
        require(block.timestamp >= m.endTime,   "Market not ended yet");

        // 从提案者拉取 bond
        usdc.transferFrom(msg.sender, address(this), BOND);

        // 授权 oracle 花费 bond
        usdc.approve(address(oracle), BOND);

        // 构建 claim（自然语言描述）
        bytes memory claim = abi.encodePacked(
            "Prediction market question: '",
            m.description,
            "'. The result is: ",
            result ? "YES." : "NO."
        );

        // 调用 OOv3.assertTruth，本合约作为 asserter 和 callbackRecipient
        bytes32 assertionId = oracle.assertTruth(
            claim,
            address(this),  // asserter = 本合约（代表 proposer）
            address(this),  // callbackRecipient = 本合约
            address(0),     // no escalation manager
            LIVENESS,
            address(usdc),
            BOND,
            ASSERT_TRUTH,
            bytes32(0)
        );

        m.state          = MarketState.Asserting;
        m.assertionId    = assertionId;
        m.proposer       = msg.sender;
        m.proposedResult = result;

        assertionToMarket[assertionId] = marketId;

        emit ResolutionProposed(marketId, assertionId, msg.sender, result);
    }

    /**
     * @notice 结算断言（liveness 结束后，无质疑路径）
     * @dev 调用 oracle.settleAssertion()，触发 assertionResolvedCallback
     */
    function settle(bytes32 marketId) external {
        Market storage m = markets[marketId];
        require(m.state == MarketState.Asserting, "Not in Asserting state");
        oracle.settleAssertion(m.assertionId);
    }

    // ─── OOv3 回调 ──────────────────────────────────────────────────────────

    /**
     * @notice OOv3 结算回调（由 oracle 合约调用）
     * @param assertionId       断言 ID
     * @param assertedTruthfully  true=断言成立，false=断言被 DVM 推翻
     */
    function assertionResolvedCallback(
        bytes32 assertionId,
        bool    assertedTruthfully
    ) external {
        require(msg.sender == address(oracle), "Caller must be oracle");

        bytes32 marketId = assertionToMarket[assertionId];
        Market storage m = markets[marketId];
        require(m.state == MarketState.Asserting || m.state == MarketState.Disputed,
                "Invalid market state");

        // 确定最终结果
        bool finalResult = assertedTruthfully
            ? m.proposedResult      // 断言成立 → 采用提案结果
            : !m.proposedResult;    // 断言被推翻 → 结果取反

        m.state      = MarketState.Resolved;
        m.resolution = finalResult;

        // 若断言成立，bond 已被 oracle 返回给 asserter（本合约）
        // 我们将 bond 退还给原始提案者
        if (assertedTruthfully) {
            usdc.transfer(m.proposer, BOND);
        }
        // 若断言被推翻，bond 被 DVM 没收（disputer 获得），此处不处理

        emit MarketResolved(marketId, finalResult);
    }

    /**
     * @notice OOv3 质疑回调（由 oracle 合约调用）
     * @param assertionId  被质疑的断言 ID
     */
    function assertionDisputedCallback(bytes32 assertionId) external {
        require(msg.sender == address(oracle), "Caller must be oracle");

        bytes32 marketId = assertionToMarket[assertionId];
        Market storage m = markets[marketId];
        m.state = MarketState.Disputed;

        emit MarketDisputed(marketId, assertionId);
    }

    // ─── 领奖 ───────────────────────────────────────────────────────────────

    /**
     * @notice 领取赢利（市场 Resolved 后调用）
     * @param marketId  市场 ID
     */
    function claimWinnings(bytes32 marketId) external {
        Market storage m = markets[marketId];
        require(m.state == MarketState.Resolved, "Market not resolved");

        uint256 totalPool = m.totalYesBets + m.totalNoBets;
        uint256 payout;

        if (m.resolution) {
            // YES 赢
            uint256 userBet = yesBets[marketId][msg.sender];
            require(userBet > 0, "No winning YES bet");
            payout = (userBet * totalPool) / m.totalYesBets;
            yesBets[marketId][msg.sender] = 0;
        } else {
            // NO 赢
            uint256 userBet = noBets[marketId][msg.sender];
            require(userBet > 0, "No winning NO bet");
            payout = (userBet * totalPool) / m.totalNoBets;
            noBets[marketId][msg.sender] = 0;
        }

        usdc.transfer(msg.sender, payout);
        emit WinningsClaimed(marketId, msg.sender, payout);
    }

    // ─── 只读函数 ───────────────────────────────────────────────────────────

    function getMarket(bytes32 marketId) external view returns (
        string  memory description,
        uint256 endTime,
        uint256 totalYesBets,
        uint256 totalNoBets,
        uint8   state,
        bool    resolution,
        bytes32 assertionId
    ) {
        Market storage m = markets[marketId];
        return (
            m.description,
            m.endTime,
            m.totalYesBets,
            m.totalNoBets,
            uint8(m.state),
            m.resolution,
            m.assertionId
        );
    }

    function getYesBet(bytes32 marketId, address bettor) external view returns (uint256) {
        return yesBets[marketId][bettor];
    }

    function getNoBet(bytes32 marketId, address bettor) external view returns (uint256) {
        return noBets[marketId][bettor];
    }
}
