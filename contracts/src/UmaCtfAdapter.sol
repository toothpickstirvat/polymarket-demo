// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title UmaCtfAdapter
 * @notice Polymarket UmaCtfAdapter 的忠实复现版本。
 *         桥接 UMA OptimisticOracleV2 和 Gnosis ConditionalTokens（CTF），
 *         实现预测市场的去中心化结算。
 *
 * 工作流程：
 *   1. initialize()   → 创建 CTF 条件 + 向 OOv2 发起价格请求
 *   2. proposePrice() → 提案者通过 OOv2 提交 YES/NO 结果
 *   3. resolve()      → liveness 结束后任何人调用，从 OOv2 取价格，结算 CTF 条件
 *
 * 争议流程：
 *   1. disputePrice() → 质疑者质疑提案
 *   2. priceDisputed()→ OOv2 调用此回调，重置价格请求
 *   3. 新提案者重新提案
 *
 * 参考合约：https://github.com/Polymarket/uma-ctf-adapter
 */

interface IOptimisticOracleV2 {
    function requestPrice(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        address currency,
        uint256 reward
    ) external returns (uint256 totalBond);

    function setBond(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        uint256 bond
    ) external returns (uint256 totalBond);

    function setCustomLiveness(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        uint256 customLiveness
    ) external;

    function setRefundOnDispute(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external;

    function hasPrice(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external view returns (bool);

    function getPrice(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external view returns (int256);

    function settle(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external returns (uint256 payout);
}

interface IConditionalTokens {
    function prepareCondition(address oracle, bytes32 questionId, uint outcomeSlotCount) external;
    function reportPayouts(bytes32 questionId, uint[] calldata payouts) external;
    function getConditionId(address oracle, bytes32 questionId, uint outcomeSlotCount) external pure returns (bytes32);
}

interface IAddressWhitelist {
    function isOnWhitelist(address addr) external view returns (bool);
}

interface IERC20 {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function transfer(address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
}

contract UmaCtfAdapter {
    // ─── 常量 ────────────────────────────────────────────────────────────────

    /// @notice OOv2 identifier，Polymarket 标准
    bytes32 public constant YES_OR_NO_IDENTIFIER = "YES_OR_NO_QUERY";

    /// @notice 价格值定义（与 OOv2 的 int256 价格对应）
    int256 private constant YES_PRICE  = 1e18;
    int256 private constant TIE_PRICE  = 0.5e18;
    int256 private constant NO_PRICE   = 0;

    /// @notice 人工干预安全期（flag 后需等待此时间才能 resolveManually）
    uint256 public constant SAFETY_PERIOD = 1 hours;

    /// @notice 默认 liveness（秒）。可在 initialize 时指定自定义值。
    uint64 public constant DEFAULT_LIVENESS = 7200;

    /// @notice ancillaryData 最大长度（OOv2 限制）
    uint256 public constant MAX_ANCILLARY_DATA = 8139;

    // ─── 状态 ────────────────────────────────────────────────────────────────

    IConditionalTokens public immutable ctf;
    IOptimisticOracleV2 public immutable optimisticOracle;
    IAddressWhitelist public immutable collateralWhitelist;

    struct QuestionData {
        uint256  requestTime;           // 首次向 OO 发起请求的时间
        uint256  reward;                // 解析成功后给 proposer 的奖励
        uint256  proposalBond;          // 提案 bond 金额
        uint64   liveness;             // 质疑窗口（秒）
        bool     resolved;             // 是否已结算
        bool     paused;               // 是否已暂停
        bool     flagged;              // 是否已标记（等待人工干预）
        uint256  flagTime;             // 标记时间（用于 SAFETY_PERIOD 校验）
        address  rewardToken;          // 奖励货币
        bytes    ancillaryData;        // 问题描述
        bool     reset;                // 是否已经经历过一次 reset（第二次质疑才上 DVM）
    }

    /// @notice questionId → 问题数据
    mapping(bytes32 => QuestionData) public questions;

    address public admin;

    // ─── 事件 ────────────────────────────────────────────────────────────────

    event QuestionInitialized(
        bytes32 indexed questionId,
        bytes   ancillaryData,
        address indexed creator,
        address rewardToken,
        uint256 reward,
        uint256 proposalBond,
        uint64  liveness
    );
    event QuestionResolved(bytes32 indexed questionId, int256 price, uint256[] payouts);
    event QuestionReset(bytes32 indexed questionId);
    event QuestionPaused(bytes32 indexed questionId);
    event QuestionUnpaused(bytes32 indexed questionId);
    event QuestionFlagged(bytes32 indexed questionId);
    event QuestionResolvedManually(bytes32 indexed questionId, uint256[] payouts);

    // ─── 构造函数 ────────────────────────────────────────────────────────────

    constructor(
        address _ctf,
        address _optimisticOracle,
        address _collateralWhitelist
    ) {
        ctf                = IConditionalTokens(_ctf);
        optimisticOracle   = IOptimisticOracleV2(_optimisticOracle);
        collateralWhitelist = IAddressWhitelist(_collateralWhitelist);
        admin              = msg.sender;
    }

    // ─── 核心函数 ────────────────────────────────────────────────────────────

    /**
     * @notice 初始化市场问题
     * @dev 会在 CTF 上创建条件，并向 OOv2 发起价格请求。
     *      questionId = keccak256(ancillaryData)，由调用者计算并传入（或从事件中获取）。
     *
     * @param ancillaryData 问题描述（UTF-8 字节），例如 "Will BNB exceed $1000?"
     * @param rewardToken   奖励代币（通常 USDC），必须在 whitelist 中
     * @param reward        成功解析的奖励金额
     * @param proposalBond  提案者需要锁定的 bond
     * @param liveness      质疑窗口（0 表示使用默认 7200 秒）
     * @return questionId   问题唯一标识
     */
    function initialize(
        bytes memory ancillaryData,
        address rewardToken,
        uint256 reward,
        uint256 proposalBond,
        uint64  liveness
    ) external returns (bytes32 questionId) {
        require(ancillaryData.length > 0, "Empty ancillaryData");
        require(ancillaryData.length <= MAX_ANCILLARY_DATA, "ancillaryData too long");
        require(collateralWhitelist.isOnWhitelist(rewardToken), "Token not whitelisted");

        questionId = keccak256(ancillaryData);
        require(!_isInitialized(questionId), "Already initialized");

        uint64 actualLiveness = liveness == 0 ? DEFAULT_LIVENESS : liveness;

        questions[questionId] = QuestionData({
            requestTime:   block.timestamp,
            reward:        reward,
            proposalBond:  proposalBond,
            liveness:      actualLiveness,
            resolved:      false,
            paused:        false,
            flagged:       false,
            flagTime:      0,
            rewardToken:   rewardToken,
            ancillaryData: ancillaryData,
            reset:         false
        });

        // 在 CTF 上准备条件（2个结果槽：YES 和 NO）
        ctf.prepareCondition(address(this), questionId, 2);

        // 向 OOv2 发起价格请求
        _requestPrice(questionId);

        emit QuestionInitialized(
            questionId, ancillaryData, msg.sender,
            rewardToken, reward, proposalBond, actualLiveness
        );
    }

    /**
     * @notice 从 OOv2 获取价格并结算 CTF 条件
     * @dev 需要 OOv2 已有价格（hasPrice == true）才能调用。
     *      通常在 proposePrice 后 liveness 结束、无质疑时调用。
     */
    function resolve(bytes32 questionId) external {
        QuestionData storage q = questions[questionId];
        require(_isInitialized(questionId), "Not initialized");
        require(!q.resolved, "Already resolved");
        require(!q.paused,   "Paused");
        require(ready(questionId), "Not ready to resolve");

        // 从 OOv2 取最终价格
        int256 price = optimisticOracle.getPrice(
            address(this),
            YES_OR_NO_IDENTIFIER,
            q.requestTime,
            q.ancillaryData
        );

        uint256[] memory payouts = _constructPayouts(price);
        q.resolved = true;

        // 向 CTF 上报结果
        ctf.reportPayouts(questionId, payouts);

        emit QuestionResolved(questionId, price, payouts);
    }

    /**
     * @notice 检查问题是否可以结算
     */
    function ready(bytes32 questionId) public view returns (bool) {
        if (!_isInitialized(questionId)) return false;
        QuestionData storage q = questions[questionId];
        if (q.resolved || q.paused) return false;
        return optimisticOracle.hasPrice(
            address(this),
            YES_OR_NO_IDENTIFIER,
            q.requestTime,
            q.ancillaryData
        );
    }

    /**
     * @notice OOv2 价格被质疑时的回调（由 OOv2 合约调用）
     * @dev 第一次质疑时：重置 OO 请求（不上 DVM）
     *      第二次质疑时：真正上 DVM（在真实 UMA 中；此 Mock 里由 mockDvmSettle 处理）
     */
    function priceDisputed(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        uint256 refund
    ) external {
        require(msg.sender == address(optimisticOracle), "Only OO");

        bytes32 questionId = keccak256(ancillaryData);
        QuestionData storage q = questions[questionId];
        require(_isInitialized(questionId), "Question not found");

        if (q.resolved) return; // 已结算，忽略
        if (q.paused)   return; // 已暂停，忽略

        // 第一次质疑：reset（重新发起 OO 请求）
        // 第二次质疑：这里简化，也走 reset；真实 UMA 中会上 DVM
        q.reset       = true;
        q.requestTime = block.timestamp; // 新请求时间

        // 重新向 OOv2 发起价格请求
        _requestPrice(questionId);

        emit QuestionReset(questionId);
    }

    // ─── 管理员函数 ──────────────────────────────────────────────────────────

    modifier onlyAdmin() {
        require(msg.sender == admin, "Only admin");
        _;
    }

    function pause(bytes32 questionId)   external onlyAdmin {
        questions[questionId].paused = true;
        emit QuestionPaused(questionId);
    }

    function unpause(bytes32 questionId) external onlyAdmin {
        questions[questionId].paused = false;
        emit QuestionUnpaused(questionId);
    }

    /// @notice 标记问题，准备人工解析（需等待 SAFETY_PERIOD 后才能 resolveManually）
    function flag(bytes32 questionId) external onlyAdmin {
        require(_isInitialized(questionId), "Not initialized");
        questions[questionId].flagged  = true;
        questions[questionId].flagTime = block.timestamp;
        emit QuestionFlagged(questionId);
    }

    /// @notice 人工解析（flag 后等待 SAFETY_PERIOD，用于紧急情况）
    function resolveManually(bytes32 questionId, uint256[] calldata payouts)
        external onlyAdmin
    {
        QuestionData storage q = questions[questionId];
        require(q.flagged,                                      "Not flagged");
        require(block.timestamp >= q.flagTime + SAFETY_PERIOD, "Safety period not elapsed");
        require(!q.resolved,                                    "Already resolved");
        require(payouts.length == 2,                            "Invalid payouts");

        q.resolved = true;
        ctf.reportPayouts(questionId, payouts);
        emit QuestionResolvedManually(questionId, payouts);
    }

    function setAdmin(address newAdmin) external onlyAdmin {
        admin = newAdmin;
    }

    // ─── 查询 ─────────────────────────────────────────────────────────────────

    function isInitialized(bytes32 questionId) external view returns (bool) {
        return _isInitialized(questionId);
    }

    function getQuestion(bytes32 questionId) external view returns (QuestionData memory) {
        return questions[questionId];
    }

    /// @notice 获取 CTF conditionId（供前端计算 tokenId 使用）
    function getConditionId(bytes32 questionId) external view returns (bytes32) {
        return ctf.getConditionId(address(this), questionId, 2);
    }

    // ─── 内部函数 ────────────────────────────────────────────────────────────

    function _isInitialized(bytes32 questionId) internal view returns (bool) {
        return questions[questionId].rewardToken != address(0);
    }

    /**
     * @notice 向 OOv2 发起价格请求，设置 bond、liveness、refundOnDispute
     */
    function _requestPrice(bytes32 questionId) internal {
        QuestionData storage q = questions[questionId];

        // 拉取 reward（如果有）并授权 OO 使用
        if (q.reward > 0) {
            IERC20(q.rewardToken).transferFrom(msg.sender, address(this), q.reward);
            IERC20(q.rewardToken).approve(address(optimisticOracle), q.reward);
        }

        optimisticOracle.requestPrice(
            YES_OR_NO_IDENTIFIER,
            q.requestTime,
            q.ancillaryData,
            q.rewardToken,
            q.reward
        );

        if (q.proposalBond > 0) {
            optimisticOracle.setBond(
                YES_OR_NO_IDENTIFIER,
                q.requestTime,
                q.ancillaryData,
                q.proposalBond
            );
        }

        optimisticOracle.setCustomLiveness(
            YES_OR_NO_IDENTIFIER,
            q.requestTime,
            q.ancillaryData,
            q.liveness
        );

        // 质疑时退还 reward 给 requester（adapter 本身），避免 reward 被锁死
        optimisticOracle.setRefundOnDispute(
            YES_OR_NO_IDENTIFIER,
            q.requestTime,
            q.ancillaryData
        );
    }

    /**
     * @notice 将 OOv2 价格值转换为 CTF payouts 数组
     * @dev YES_PRICE(1e18) → [1e18, 0]（YES赢）
     *      NO_PRICE(0)     → [0, 1e18]（NO赢）
     *      TIE_PRICE       → [1e18, 1e18]（平局，实际上各得 50%）
     *      其他            → revert（无效价格）
     */
    function _constructPayouts(int256 price) internal pure returns (uint256[] memory payouts) {
        payouts = new uint256[](2);
        if (price == YES_PRICE) {
            payouts[0] = 1e18;
            payouts[1] = 0;
        } else if (price == NO_PRICE) {
            payouts[0] = 0;
            payouts[1] = 1e18;
        } else if (price == TIE_PRICE) {
            payouts[0] = 1e18;
            payouts[1] = 1e18;
        } else {
            revert("Invalid price from OO");
        }
    }
}
