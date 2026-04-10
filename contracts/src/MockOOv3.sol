// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title MockOptimisticOracleV3
 * @notice 模拟 UMA OptimisticOracleV3，用于在不支持 UMA 的链（如 BSC 测试网）上测试。
 *
 * 接口与官方 OptimisticOracleV3Interface 完全一致，生产迁移只需更换合约地址：
 *   - assertTruth()                提交断言
 *   - assertTruthWithDefaults()    使用默认参数提交断言
 *   - disputeAssertion()           质疑断言
 *   - settleAssertion()            结算断言（liveness 期结束后）
 *   - settleAndGetAssertionResult()结算并返回结果
 *   - getAssertionResult()         查询已结算的结果
 *   - getAssertion()               查询断言详情
 *   - defaultIdentifier()          返回默认 identifier
 *   - syncUmaParams()              同步 UMA 参数（Mock 中为空操作）
 *   - getMinimumBond()             返回最小 bond 金额
 *
 * 额外增加（测试专用）：
 *   - mockDvmResolve()             模拟 DVM 裁决（由 dvm 地址调用）
 */

interface IERC20 {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function transfer(address to, uint256 amount) external returns (bool);
}

interface ICallbackRecipient {
    function assertionResolvedCallback(bytes32 assertionId, bool assertedTruthfully) external;
    function assertionDisputedCallback(bytes32 assertionId) external;
}

contract MockOptimisticOracleV3 {
    // ─── 数据结构（与官方接口完全一致）──────────────────────────────────────

    struct EscalationManagerSettings {
        bool    arbitrateViaEscalationManager;
        bool    discardOracle;
        bool    validateDisputers;
        address assertingCaller;
        address escalationManager;
    }

    struct Assertion {
        EscalationManagerSettings escalationManagerSettings;
        address asserter;
        uint64  assertionTime;
        bool    settled;
        address currency;           // IERC20，用 address 存储以兼容 go-ethereum ABI 解码
        uint64  expirationTime;
        bool    settlementResolution;
        bytes32 domainId;
        bytes32 identifier;
        uint256 bond;
        address callbackRecipient;
        address disputer;
    }

    // ─── 状态变量 ──────────────────────────────────────────────────────────

    mapping(bytes32 => Assertion) public assertions;
    // assertionId → claim（存在单独 mapping，与官方 struct 一致，struct 中不含 claim）
    mapping(bytes32 => bytes)     public assertionClaims;
    uint256 private assertionCounter;

    /// @notice DVM 地址（测试用，模拟仲裁者，通常是 deployer）
    address public dvm;

    bytes32 public constant DEFAULT_IDENTIFIER = bytes32("ASSERT_TRUTH");

    // ─── 事件（与官方接口完全一致）────────────────────────────────────────

    event AssertionMade(
        bytes32 indexed assertionId,
        bytes32         domainId,
        bytes           claim,
        address indexed asserter,
        address         callbackRecipient,
        address         escalationManager,
        address         caller,
        uint64          expirationTime,
        address         currency,   // 官方为 IERC20，ABI 层等同 address
        uint256         bond,
        bytes32 indexed identifier
    );

    event AssertionDisputed(
        bytes32 indexed assertionId,
        address indexed caller,
        address indexed disputer
    );

    event AssertionSettled(
        bytes32 indexed assertionId,
        address indexed bondRecipient,
        bool            disputed,
        bool            settlementResolution,
        address         settleCaller
    );

    // ─── 构造函数 ──────────────────────────────────────────────────────────

    constructor(address _dvm) {
        dvm = _dvm;
    }

    // ─── 核心函数（与官方接口完全一致）────────────────────────────────────

    /**
     * @notice 提交断言（完整参数版本）
     * @dev bond 从 msg.sender 转入（asserter 是结算时收回 bond 的地址，可与 msg.sender 不同）
     */
    function assertTruth(
        bytes memory claim,
        address asserter,
        address callbackRecipient,
        address escalationManager,
        uint64  liveness,
        address currency,           // IERC20
        uint256 bond,
        bytes32 identifier,
        bytes32 domainId
    ) external returns (bytes32 assertionId) {
        assertionId = keccak256(
            abi.encode(assertionCounter++, claim, asserter, block.timestamp, block.chainid)
        );

        // bond 从 msg.sender 转入（与官方一致）
        require(
            IERC20(currency).transferFrom(msg.sender, address(this), bond),
            "Bond transfer failed"
        );

        uint64 expirationTime = uint64(block.timestamp) + liveness;

        assertions[assertionId] = Assertion({
            escalationManagerSettings: EscalationManagerSettings({
                arbitrateViaEscalationManager: false,
                discardOracle:                 false,
                validateDisputers:             false,
                assertingCaller:               msg.sender,
                escalationManager:             escalationManager
            }),
            asserter:             asserter,
            assertionTime:        uint64(block.timestamp),
            settled:              false,
            currency:             currency,
            expirationTime:       expirationTime,
            settlementResolution: false,
            domainId:             domainId,
            identifier:           identifier == bytes32(0) ? DEFAULT_IDENTIFIER : identifier,
            bond:                 bond,
            callbackRecipient:    callbackRecipient,
            disputer:             address(0)
        });

        assertionClaims[assertionId] = claim;

        emit AssertionMade(
            assertionId,
            domainId,
            claim,
            asserter,
            callbackRecipient,
            escalationManager,
            msg.sender,
            expirationTime,
            currency,
            bond,
            identifier
        );
    }

    /**
     * @notice 使用默认参数提交断言（无 callbackRecipient / escalationManager）
     */
    function assertTruthWithDefaults(
        bytes memory claim,
        address asserter
    ) external returns (bytes32 assertionId) {
        revert("assertTruthWithDefaults: not supported in Mock, use assertTruth");
    }

    /**
     * @notice 质疑断言
     * @dev bond 从 msg.sender 转入（disputer 是结算时收回 bond 的地址，可与 msg.sender 不同）
     */
    function disputeAssertion(bytes32 assertionId, address disputer) external {
        Assertion storage a = assertions[assertionId];
        require(a.asserter != address(0),             "Assertion not found");
        require(!a.settled,                           "Already settled");
        require(a.disputer == address(0),             "Already disputed");
        require(block.timestamp < a.expirationTime,   "Liveness expired");

        require(
            IERC20(a.currency).transferFrom(msg.sender, address(this), a.bond),
            "Disputer bond transfer failed"
        );

        a.disputer = disputer;

        emit AssertionDisputed(assertionId, msg.sender, disputer);

        if (a.callbackRecipient != address(0)) {
            ICallbackRecipient(a.callbackRecipient).assertionDisputedCallback(assertionId);
        }
    }

    /**
     * @notice 结算断言（无质疑且 liveness 结束后可调用）
     */
    function settleAssertion(bytes32 assertionId) external {
        Assertion storage a = assertions[assertionId];
        require(a.asserter != address(0),             "Assertion not found");
        require(!a.settled,                           "Already settled");
        require(a.disputer == address(0),             "Disputed: waiting for DVM");
        require(block.timestamp >= a.expirationTime,  "Liveness not elapsed");

        a.settled              = true;
        a.settlementResolution = true; // 无质疑 → 断言成立

        IERC20(a.currency).transfer(a.asserter, a.bond);

        emit AssertionSettled(assertionId, a.asserter, false, true, msg.sender);

        if (a.callbackRecipient != address(0)) {
            ICallbackRecipient(a.callbackRecipient).assertionResolvedCallback(assertionId, true);
        }
    }

    /**
     * @notice 结算并返回结果（convenience 函数）
     */
    function settleAndGetAssertionResult(bytes32 assertionId) external returns (bool) {
        this.settleAssertion(assertionId);
        return assertions[assertionId].settlementResolution;
    }

    /**
     * @notice 查询断言结果（已结算后有效）
     */
    function getAssertionResult(bytes32 assertionId) external view returns (bool) {
        require(assertions[assertionId].settled, "Assertion not settled");
        return assertions[assertionId].settlementResolution;
    }

    /**
     * @notice 查询断言详情
     */
    function getAssertion(bytes32 assertionId) external view returns (Assertion memory) {
        return assertions[assertionId];
    }

    /**
     * @notice 返回默认 identifier
     */
    function defaultIdentifier() external pure returns (bytes32) {
        return DEFAULT_IDENTIFIER;
    }

    /**
     * @notice 同步 UMA 参数（Mock 中为空操作，接口兼容）
     */
    function syncUmaParams(bytes32 /*identifier*/, address /*currency*/) external {
        // Mock 中无需同步，空实现保持接口兼容
    }

    /**
     * @notice 返回最小 bond 金额（Mock 中固定返回 0，接口兼容）
     */
    function getMinimumBond(address /*currency*/) external pure returns (uint256) {
        return 0;
    }

    // ─── 测试专用函数 ───────────────────────────────────────────────────────

    /**
     * @notice 模拟 DVM 仲裁（仅 dvm 地址可调用）
     * @dev 在真实 UMA 中，DVM 投票后自动触发结算回调。
     *      此 Mock 中由测试者手动调用，模拟 DVM 的仲裁结果。
     *
     * @param assertionId  被质疑的断言 ID
     * @param resolution   true=断言成立（asserter 赢），false=断言被推翻（disputer 赢）
     */
    function mockDvmResolve(bytes32 assertionId, bool resolution) external {
        require(msg.sender == dvm,            "Only DVM can resolve");
        Assertion storage a = assertions[assertionId];
        require(a.asserter != address(0),     "Assertion not found");
        require(!a.settled,                   "Already settled");
        require(a.disputer != address(0),     "Not disputed");

        a.settled              = true;
        a.settlementResolution = resolution;

        address winner = resolution ? a.asserter : a.disputer;
        IERC20(a.currency).transfer(winner, a.bond * 2);

        emit AssertionSettled(assertionId, winner, true, resolution, msg.sender);

        if (a.callbackRecipient != address(0)) {
            ICallbackRecipient(a.callbackRecipient).assertionResolvedCallback(assertionId, resolution);
        }
    }

    /**
     * @notice 更新 DVM 地址（仅当前 DVM 可调用）
     */
    function setDvm(address newDvm) external {
        require(msg.sender == dvm, "Only current DVM");
        dvm = newDvm;
    }
}
