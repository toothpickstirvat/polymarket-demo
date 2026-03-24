// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title MockOptimisticOracleV3
 * @notice 模拟 UMA OptimisticOracleV3，用于在不支持 UMA 的链（如 BSC 测试网）上测试。
 *
 * 与真实 OOv3 的接口完全兼容：
 *   - assertTruth()         提交断言
 *   - disputeAssertion()    质疑断言
 *   - settleAssertion()     结算断言（liveness 期结束后）
 *   - getAssertionResult()  查询结果
 *   - getAssertion()        查询断言详情
 *
 * 额外增加（测试专用）：
 *   - mockDvmResolve()      模拟 DVM 裁决（由 dvm 地址调用）
 *
 * 生产迁移：将 oracle 地址替换为真实 UMA OOv3 地址即可，无需修改业务合约。
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
    // ─── 数据结构 ───────────────────────────────────────────────────────────

    struct Assertion {
        address asserter;           // 断言者（bond 来源）
        address callbackRecipient;  // 回调合约地址
        address disputer;           // 质疑者（0 表示未被质疑）
        address currency;           // bond 货币（通常 USDC）
        uint256 bond;               // bond 金额
        uint64  expirationTime;     // liveness 结束时间
        bool    settled;            // 是否已结算
        bool    settlementResolution; // 最终结果（true=断言成立）
        bytes   claim;              // 断言内容（自然语言）
    }

    // ─── 状态变量 ──────────────────────────────────────────────────────────

    mapping(bytes32 => Assertion) public assertions;
    uint256 private assertionCounter;

    /// @notice DVM 地址（测试用，模拟仲裁者，通常是 deployer）
    address public dvm;

    // ─── 事件 ──────────────────────────────────────────────────────────────

    event AssertionMade(
        bytes32 indexed assertionId,
        bytes   claim,
        address indexed asserter,
        address callbackRecipient,
        uint64  expirationTime,
        address currency,
        uint256 bond
    );

    event AssertionDisputed(
        bytes32 indexed assertionId,
        address indexed disputer
    );

    event AssertionSettled(
        bytes32 indexed assertionId,
        bool    resolution,
        address bondRecipient
    );

    // ─── 构造函数 ──────────────────────────────────────────────────────────

    constructor(address _dvm) {
        dvm = _dvm;
    }

    // ─── 核心函数（与 UMA OOv3 接口兼容）────────────────────────────────────

    /**
     * @notice 提交断言
     * @dev 调用前：asserter 必须 approve 本合约花费 bond 数量的 currency
     *
     * @param claim              断言内容（自然语言，UTF-8 bytes）
     * @param asserter           断言者地址（bond 从此地址转入）
     * @param callbackRecipient  结算时接收回调的合约（0 表示不需要回调）
     * @param escalationManager  自定义升级管理器（此 Mock 中忽略，填 address(0)）
     * @param liveness           质疑窗口（秒）
     * @param currency           bond 货币（ERC20 地址）
     * @param bond               bond 金额
     * @param identifier         数据类型标识（此 Mock 中忽略，填 bytes32(0)）
     * @param domainId           域 ID（此 Mock 中忽略，填 bytes32(0)）
     * @return assertionId       唯一断言 ID
     */
    function assertTruth(
        bytes memory claim,
        address asserter,
        address callbackRecipient,
        address escalationManager,
        uint64  liveness,
        address currency,
        uint256 bond,
        bytes32 identifier,
        bytes32 domainId
    ) external returns (bytes32 assertionId) {
        // 生成唯一 assertionId
        assertionId = keccak256(
            abi.encode(assertionCounter++, claim, asserter, block.timestamp, block.chainid)
        );

        // 从 asserter 转入 bond
        require(
            IERC20(currency).transferFrom(asserter, address(this), bond),
            "Bond transfer failed"
        );

        assertions[assertionId] = Assertion({
            asserter:             asserter,
            callbackRecipient:    callbackRecipient,
            disputer:             address(0),
            currency:             currency,
            bond:                 bond,
            expirationTime:       uint64(block.timestamp) + liveness,
            settled:              false,
            settlementResolution: false,
            claim:                claim
        });

        emit AssertionMade(
            assertionId, claim, asserter, callbackRecipient,
            uint64(block.timestamp) + liveness, currency, bond
        );
    }

    /**
     * @notice 质疑断言
     * @dev 调用前：disputer 必须 approve 本合约花费 bond 数量的 currency
     *
     * @param assertionId  被质疑的断言 ID
     * @param disputer     质疑者地址（bond 从此地址转入）
     */
    function disputeAssertion(bytes32 assertionId, address disputer) external {
        Assertion storage a = assertions[assertionId];
        require(a.asserter != address(0),   "Assertion not found");
        require(!a.settled,                 "Already settled");
        require(a.disputer == address(0),   "Already disputed");
        require(block.timestamp < a.expirationTime, "Liveness expired");

        // 从质疑者转入相同数额的 bond
        require(
            IERC20(a.currency).transferFrom(disputer, address(this), a.bond),
            "Disputer bond transfer failed"
        );

        a.disputer = disputer;

        emit AssertionDisputed(assertionId, disputer);

        // 触发质疑回调
        if (a.callbackRecipient != address(0)) {
            ICallbackRecipient(a.callbackRecipient).assertionDisputedCallback(assertionId);
        }
    }

    /**
     * @notice 结算断言（仅在无质疑且 liveness 结束后可调用）
     * @param assertionId  要结算的断言 ID
     */
    function settleAssertion(bytes32 assertionId) external {
        Assertion storage a = assertions[assertionId];
        require(a.asserter != address(0),   "Assertion not found");
        require(!a.settled,                 "Already settled");
        require(a.disputer == address(0),   "Disputed: waiting for DVM");
        require(block.timestamp >= a.expirationTime, "Liveness not elapsed");

        a.settled              = true;
        a.settlementResolution = true; // 无质疑 → 断言成立

        // 返还 bond 给断言者
        IERC20(a.currency).transfer(a.asserter, a.bond);

        emit AssertionSettled(assertionId, true, a.asserter);

        // 触发结算回调
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
        return assertions[assertionId].settlementResolution;
    }

    /**
     * @notice 查询断言详情
     */
    function getAssertion(bytes32 assertionId) external view returns (Assertion memory) {
        return assertions[assertionId];
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
        require(msg.sender == dvm,          "Only DVM can resolve");
        Assertion storage a = assertions[assertionId];
        require(a.asserter != address(0),   "Assertion not found");
        require(!a.settled,                 "Already settled");
        require(a.disputer != address(0),   "Not disputed");

        a.settled              = true;
        a.settlementResolution = resolution;

        // Bond 分配：胜者获得全部 bond（asserter 的 + disputer 的）
        address winner = resolution ? a.asserter : a.disputer;
        IERC20(a.currency).transfer(winner, a.bond * 2);

        emit AssertionSettled(assertionId, resolution, winner);

        // 触发结算回调
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
