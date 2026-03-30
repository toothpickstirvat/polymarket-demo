// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title UmaCtfAdapterV3
 * @notice 基于 UMA OptimisticOracleV3 的预测市场适配合约。
 *         桥接 UMA OOv3 和 Gnosis ConditionalTokens（CTF），
 *         实现预测市场的去中心化结算。
 *
 * 与 UmaCtfAdapter（OOv2 版本）的主要差异：
 *   - 初始化时只准备 CTF 条件，不发起 OO 价格请求
 *   - proposeResolution() 调用 OO.assertTruth() 提交断言（含 liveness）
 *   - settle() 在 liveness 结束后触发 OO.settleAssertion()
 *   - 通过 assertionResolvedCallback() 回调驱动 CTF 结算
 *   - 争议时 DVM 直接裁决（mockDvmResolve），无需二次提案
 *
 * 工作流程（正常）：
 *   1. initialize()         → CTF.prepareCondition()
 *   2. proposeResolution()  → OO.assertTruth()（提案者质押 bond）
 *   3. settle()             → OO.settleAssertion()
 *                           → assertionResolvedCallback(true)
 *                           → CTF.reportPayouts([1e18,0] 或 [0,1e18])
 *
 * 工作流程（争议）：
 *   1. initialize()         → CTF.prepareCondition()
 *   2. proposeResolution()  → OO.assertTruth()
 *   3. OO.disputeAssertion()→ assertionDisputedCallback()（状态记录）
 *   4. OO.mockDvmResolve()  → assertionResolvedCallback(false)
 *                           → CTF.reportPayouts（结果取反）
 */

interface IERC20 {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function transfer(address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
}

interface IConditionalTokens {
    function prepareCondition(address oracle, bytes32 questionId, uint256 outcomeSlotCount) external;
    function reportPayouts(bytes32 questionId, uint256[] calldata payouts) external;
    function getConditionId(address oracle, bytes32 questionId, uint256 outcomeSlotCount) external pure returns (bytes32);
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
}

contract UmaCtfAdapterV3 {
    // ─── 数据结构 ─────────────────────────────────────────────────────────

    struct Question {
        uint256 proposalBond;   // 提案/质疑所需 bond（USDC，6 位精度）
        uint64  liveness;       // 质疑窗口（秒）
        bool    resolved;       // 是否已完成 CTF 结算
        bool    proposed;       // 是否已提案（assertTruth 已调用）
        bool    proposedResult; // 提案结果（true=YES 赢，false=NO 赢）
        bytes32 assertionId;    // OOv3 断言 ID（proposeResolution 后填充）
        bytes   ancillaryData;  // 问题描述（UTF-8 字节）
        address proposer;       // 提案者地址（bond 来源，无争议时退回）
    }

    // ─── 状态变量 ─────────────────────────────────────────────────────────

    IConditionalTokens  public immutable ctf;
    IOptimisticOracleV3 public immutable oo;
    IERC20              public immutable usdc;
    address             public immutable admin;

    // OOv3 identifier，固定为 "ASSERT_TRUTH"（右填充零到 32 字节）
    bytes32 public constant ASSERT_TRUTH = bytes32("ASSERT_TRUTH");

    mapping(bytes32 => Question) public questions;
    // assertionId → questionId，用于回调中反查对应市场
    mapping(bytes32 => bytes32)  public assertionToQuestion;

    // ─── 事件 ─────────────────────────────────────────────────────────────

    event QuestionInitialized(
        bytes32 indexed questionId,
        bytes           ancillaryData,
        address indexed creator
    );

    event ResolutionProposed(
        bytes32 indexed questionId,
        bytes32 indexed assertionId,
        address         proposer,
        bool            proposedResult
    );

    event QuestionDisputed(
        bytes32 indexed questionId,
        bytes32 indexed assertionId
    );

    event QuestionResolved(
        bytes32 indexed questionId,
        bool            resolution
    );

    // ─── 构造函数 ─────────────────────────────────────────────────────────

    /**
     * @param _ctf   ConditionalTokens 地址（复用 BSC Testnet 已有部署）
     * @param _oo    MockOptimisticOracleV3 地址（每次测试自部署）
     * @param _usdc  USDC（ChildERC20）地址，bond 货币
     */
    constructor(address _ctf, address _oo, address _usdc) {
        ctf   = IConditionalTokens(_ctf);
        oo    = IOptimisticOracleV3(_oo);
        usdc  = IERC20(_usdc);
        admin = msg.sender;
    }

    // ─── 核心函数 ─────────────────────────────────────────────────────────

    /**
     * @notice 初始化预测市场
     * @dev 与 OOv2 版本的区别：此时不向 OO 发起请求，只准备 CTF 条件。
     *      proposalBond 和 liveness 暂存于 Question，在 proposeResolution 时使用。
     *
     * @param ancillaryData  问题描述（UTF-8 字节）
     * @param proposalBond   提案/质疑所需 USDC（6 位精度）
     * @param liveness       质疑窗口（秒）
     * @return questionId    问题唯一 ID（keccak256(ancillaryData)）
     */
    function initialize(
        bytes memory ancillaryData,
        uint256 proposalBond,
        uint64  liveness
    ) external returns (bytes32 questionId) {
        questionId = keccak256(ancillaryData);

        // 在 CTF 准备条件：2 个结果 slot（YES=indexSet 1，NO=indexSet 2）
        ctf.prepareCondition(address(this), questionId, 2);

        questions[questionId] = Question({
            proposalBond:   proposalBond,
            liveness:       liveness,
            resolved:       false,
            proposed:       false,
            proposedResult: false,
            assertionId:    bytes32(0),
            ancillaryData:  ancillaryData,
            proposer:       address(0)
        });

        emit QuestionInitialized(questionId, ancillaryData, msg.sender);
    }

    /**
     * @notice 提案市场结果
     * @dev 调用前：msg.sender 须 approve 本合约花费 proposalBond 数量的 USDC。
     *      本合约作为 asserter 和 callbackRecipient 调用 OOv3.assertTruth()。
     *      liveness 窗口内任何人可调用 OO.disputeAssertion() 质疑。
     *
     * @param questionId  市场 ID
     * @param result      提案结果（true=YES 赢，false=NO 赢）
     */
    function proposeResolution(bytes32 questionId, bool result) external {
        Question storage q = questions[questionId];
        require(q.ancillaryData.length > 0, "Question not found");
        require(!q.proposed,               "Already proposed");
        require(!q.resolved,               "Already resolved");

        // 从提案者拉取 bond，授权 OO 使用
        usdc.transferFrom(msg.sender, address(this), q.proposalBond);
        usdc.approve(address(oo), q.proposalBond);

        // 断言内容（自然语言，供人工核查）
        bytes memory claim = abi.encodePacked(
            "Market question: ", q.ancillaryData,
            ". Proposed result: ", result ? "YES" : "NO"
        );

        // 本合约既是 asserter 也是 callbackRecipient
        bytes32 assertionId = oo.assertTruth(
            claim,
            address(this), // asserter（bond 由本合约锁入 OO）
            address(this), // callbackRecipient（OO 回调本合约）
            address(0),    // 无 escalation manager
            q.liveness,
            address(usdc),
            q.proposalBond,
            ASSERT_TRUTH,
            bytes32(0)
        );

        q.proposed       = true;
        q.proposedResult = result;
        q.assertionId    = assertionId;
        q.proposer       = msg.sender;

        assertionToQuestion[assertionId] = questionId;

        emit ResolutionProposed(questionId, assertionId, msg.sender, result);
    }

    /**
     * @notice 结算断言（无质疑路径）
     * @dev liveness 结束后任何人可调用。
     *      内部调用 OO.settleAssertion()，OO 返还 bond 给本合约（asserter），
     *      然后触发 assertionResolvedCallback(assertionId, true)。
     *
     * @param questionId  市场 ID
     */
    function settle(bytes32 questionId) external {
        Question storage q = questions[questionId];
        require(q.proposed,  "Not proposed yet");
        require(!q.resolved, "Already resolved");
        oo.settleAssertion(q.assertionId);
    }

    // ─── OOv3 回调 ────────────────────────────────────────────────────────

    /**
     * @notice OOv3 结算回调（由 OO 合约调用）
     * @dev 无质疑路径：OO.settleAssertion() → 本函数（assertedTruthfully=true）
     *      争议路径：OO.mockDvmResolve(false) → 本函数（assertedTruthfully=false）
     *
     *      assertedTruthfully=true  → 断言成立，采用提案结果
     *      assertedTruthfully=false → 断言被 DVM 推翻，结果取反
     *
     * @param assertionId       断言 ID
     * @param assertedTruthfully true=断言成立；false=断言被推翻
     */
    function assertionResolvedCallback(
        bytes32 assertionId,
        bool    assertedTruthfully
    ) external {
        require(msg.sender == address(oo), "Only oracle");

        bytes32 questionId = assertionToQuestion[assertionId];
        Question storage q = questions[questionId];
        require(!q.resolved, "Already resolved");

        // 最终结果：成立则采用提案值，否则取反
        bool finalResult = assertedTruthfully ? q.proposedResult : !q.proposedResult;
        q.resolved = true;

        // CTF payouts：YES=index 0，NO=index 1（与 OOv2 版本保持一致）
        uint256[] memory payouts = new uint256[](2);
        if (finalResult) {
            payouts[0] = 1e18; // YES 赢
            payouts[1] = 0;
        } else {
            payouts[0] = 0;    // NO 赢
            payouts[1] = 1e18;
        }
        ctf.reportPayouts(questionId, payouts);

        // 无质疑结算时，OO 已将 bond 返还给本合约（asserter），
        // 再转给原始提案者（deployer）
        if (assertedTruthfully) {
            usdc.transfer(q.proposer, q.proposalBond);
        }
        // 有质疑时 bond 由 OO 直接转给质疑者（disputer），本合约无需处理

        emit QuestionResolved(questionId, finalResult);
    }

    /**
     * @notice OOv3 质疑回调（由 OO 合约在 disputeAssertion 时调用）
     * @dev 质疑发生后，等待 mockDvmResolve 调用，最终通过 assertionResolvedCallback 结算。
     *      此处只记录事件，无需额外操作。
     *
     * @param assertionId  被质疑的断言 ID
     */
    function assertionDisputedCallback(bytes32 assertionId) external {
        require(msg.sender == address(oo), "Only oracle");
        bytes32 questionId = assertionToQuestion[assertionId];
        emit QuestionDisputed(questionId, assertionId);
    }

    // ─── 只读函数 ─────────────────────────────────────────────────────────

    /// @notice 获取市场对应的 CTF conditionId
    function getConditionId(bytes32 questionId) external view returns (bytes32) {
        return ctf.getConditionId(address(this), questionId, 2);
    }

    /// @notice 获取当前提案的 assertionId（质疑流程中需要此 ID）
    function getAssertionId(bytes32 questionId) external view returns (bytes32) {
        return questions[questionId].assertionId;
    }
}
