// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title MockOptimisticOracleV2
 * @notice 模拟 UMA OptimisticOracleV2，供 UmaCtfAdapter 在 BSC 测试网使用。
 *
 * OOv2 与 OOv3 的核心差异：
 *   - 采用"请求-提案-质疑"三段式，而非 OOv3 的"直接断言"
 *   - 返回 int256 价格值（1e18=YES, 0=NO, 0.5e18=平局）
 *   - Identifier 使用 "YES_OR_NO_QUERY"
 *   - 回调接口为 priceDisputed()，而非 OOv3 的 assertionDisputedCallback()
 *
 * 接口完全兼容真实 UMA OOv2，UmaCtfAdapter 无需修改即可使用。
 * 测试专用：mockDvmSettle() 由 dvm 账户调用，模拟 DVM 仲裁结果。
 */

interface IERC20 {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function transfer(address to, uint256 amount) external returns (bool);
}

interface IPriceDisputedCallback {
    function priceDisputed(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        uint256 refund
    ) external;
}

contract MockOptimisticOracleV2 {
    // ─── 状态 ────────────────────────────────────────────────────────────────

    enum RequestState {
        Invalid,       // 不存在
        Requested,     // 已请求，等待提案
        Proposed,      // 已提案，等待质疑窗口结束
        Disputed,      // 已质疑，等待 DVM
        Resolved       // 已结算（有价格）
    }

    struct Request {
        address requester;        // 谁发起的请求（通常是 UmaCtfAdapter）
        address currency;         // bond 货币
        uint256 reward;           // 解析成功奖励
        uint256 bond;             // 提案 bond 金额
        uint64  liveness;         // 质疑窗口（秒）
        int256  proposedPrice;    // 提案价格
        address proposer;         // 提案者
        address disputer;         // 质疑者
        uint64  proposedAt;       // 提案时间
        RequestState state;
        bool    refundOnDispute;  // 质疑时是否退还 reward 给 requester
    }

    mapping(bytes32 => Request) public requests;

    address public dvm;           // 模拟 DVM，测试用
    uint64  public defaultLiveness = 7200; // 2 小时（秒）

    // ─── 事件 ────────────────────────────────────────────────────────────────

    event RequestPrice(
        address indexed requester,
        bytes32 indexed identifier,
        uint256 timestamp,
        bytes ancillaryData,
        address currency,
        uint256 reward,
        uint256 finalFee
    );
    event ProposePrice(
        address indexed requester,
        bytes32 indexed identifier,
        uint256 timestamp,
        bytes ancillaryData,
        int256 proposedPrice,
        uint256 expirationTimestamp,
        address currency,
        address indexed proposer
    );
    event DisputePrice(
        address indexed requester,
        bytes32 indexed identifier,
        uint256 timestamp,
        bytes ancillaryData,
        address indexed proposer,
        address disputer,
        int256 proposedPrice
    );
    event Settle(
        address indexed requester,
        bytes32 indexed identifier,
        uint256 timestamp,
        bytes ancillaryData,
        int256 price,
        uint256 payout,
        address indexed settler
    );

    // ─── 构造函数 ────────────────────────────────────────────────────────────

    constructor(address _dvm) {
        dvm = _dvm;
    }

    // ─── 工具函数 ────────────────────────────────────────────────────────────

    function _requestKey(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) internal pure returns (bytes32) {
        return keccak256(abi.encode(requester, identifier, timestamp, keccak256(ancillaryData)));
    }

    // ─── UmaCtfAdapter 调用（初始化阶段）────────────────────────────────────

    /**
     * @notice 注册一个新的价格请求（由 UmaCtfAdapter.initialize() 调用）
     */
    function requestPrice(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        address currency,
        uint256 reward
    ) external returns (uint256 totalBond) {
        bytes32 key = _requestKey(msg.sender, identifier, timestamp, ancillaryData);
        require(requests[key].state == RequestState.Invalid, "Request exists");

        // 拉取 reward（如果有）
        if (reward > 0) {
            IERC20(currency).transferFrom(msg.sender, address(this), reward);
        }

        requests[key] = Request({
            requester:       msg.sender,
            currency:        currency,
            reward:          reward,
            bond:            0,
            liveness:        defaultLiveness,
            proposedPrice:   0,
            proposer:        address(0),
            disputer:        address(0),
            proposedAt:      0,
            state:           RequestState.Requested,
            refundOnDispute: false
        });

        emit RequestPrice(msg.sender, identifier, timestamp, ancillaryData, currency, reward, 0);
        return 0; // finalFee（mock 中为 0）
    }

    /**
     * @notice 设置 bond 金额（由 UmaCtfAdapter.initialize() 调用）
     */
    function setBond(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        uint256 bond
    ) external returns (uint256 totalBond) {
        bytes32 key = _requestKey(msg.sender, identifier, timestamp, ancillaryData);
        require(requests[key].state == RequestState.Requested, "Invalid state");
        requests[key].bond = bond;
        return bond;
    }

    /**
     * @notice 设置自定义 liveness（由 UmaCtfAdapter.initialize() 调用）
     */
    function setCustomLiveness(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        uint256 customLiveness
    ) external {
        bytes32 key = _requestKey(msg.sender, identifier, timestamp, ancillaryData);
        require(requests[key].state == RequestState.Requested, "Invalid state");
        requests[key].liveness = uint64(customLiveness);
    }

    /**
     * @notice 设置质疑时退款（由 UmaCtfAdapter.initialize() 调用）
     */
    function setRefundOnDispute(
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external {
        bytes32 key = _requestKey(msg.sender, identifier, timestamp, ancillaryData);
        require(requests[key].state == RequestState.Requested, "Invalid state");
        requests[key].refundOnDispute = true;
    }

    // ─── 提案者调用 ──────────────────────────────────────────────────────────

    /**
     * @notice 提案价格（任何人可调用）
     * @param requester     发起请求的合约（UmaCtfAdapter 地址）
     * @param identifier    通常为 "YES_OR_NO_QUERY"
     * @param timestamp     请求时间戳
     * @param ancillaryData 问题描述
     * @param proposedPrice 提案价格：1e18=YES, 0=NO, 0.5e18=平局
     */
    function proposePrice(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        int256 proposedPrice
    ) external returns (uint256 totalBond) {
        bytes32 key = _requestKey(requester, identifier, timestamp, ancillaryData);
        Request storage req = requests[key];
        require(req.state == RequestState.Requested, "Not in Requested state");

        // 拉取 bond
        if (req.bond > 0) {
            IERC20(req.currency).transferFrom(msg.sender, address(this), req.bond);
        }

        req.proposedPrice = proposedPrice;
        req.proposer      = msg.sender;
        req.proposedAt    = uint64(block.timestamp);
        req.state         = RequestState.Proposed;

        uint64 expiry = uint64(block.timestamp) + req.liveness;
        emit ProposePrice(requester, identifier, timestamp, ancillaryData,
            proposedPrice, expiry, req.currency, msg.sender);

        return req.bond;
    }

    // ─── 质疑者调用 ──────────────────────────────────────────────────────────

    /**
     * @notice 质疑提案（任何人可在 liveness 内调用）
     * @dev 会触发 requester.priceDisputed() 回调，通知 UmaCtfAdapter 进行 reset
     */
    function disputePrice(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external returns (uint256 totalBond) {
        bytes32 key = _requestKey(requester, identifier, timestamp, ancillaryData);
        Request storage req = requests[key];
        require(req.state == RequestState.Proposed, "Not in Proposed state");
        require(
            block.timestamp < uint256(req.proposedAt) + uint256(req.liveness),
            "Liveness elapsed"
        );

        // 拉取质疑者 bond
        if (req.bond > 0) {
            IERC20(req.currency).transferFrom(msg.sender, address(this), req.bond);
        }

        req.disputer = msg.sender;
        req.state    = RequestState.Disputed;

        emit DisputePrice(requester, identifier, timestamp, ancillaryData,
            req.proposer, msg.sender, req.proposedPrice);

        // 计算退款（如果设置了 refundOnDispute 则退还 reward 给 requester）
        uint256 refund = req.refundOnDispute ? req.reward : 0;
        if (refund > 0) {
            IERC20(req.currency).transfer(requester, refund);
        }

        // 回调 requester.priceDisputed()（UmaCtfAdapter 用此 reset 市场）
        IPriceDisputedCallback(requester).priceDisputed(
            identifier,
            timestamp,
            ancillaryData,
            refund
        );

        return req.bond;
    }

    // ─── 结算（liveness 结束后，无质疑）────────────────────────────────────

    /**
     * @notice 结算价格请求（liveness 结束后任何人可调用）
     */
    function settle(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external returns (uint256 payout) {
        bytes32 key = _requestKey(requester, identifier, timestamp, ancillaryData);
        Request storage req = requests[key];
        require(req.state == RequestState.Proposed, "Not in Proposed state");
        require(
            block.timestamp >= uint256(req.proposedAt) + uint256(req.liveness),
            "Liveness not elapsed"
        );

        req.state = RequestState.Resolved;

        // 退还 proposer bond + reward
        uint256 totalPayout = req.bond + req.reward;
        if (totalPayout > 0 && req.proposer != address(0)) {
            IERC20(req.currency).transfer(req.proposer, totalPayout);
        }

        emit Settle(requester, identifier, timestamp, ancillaryData,
            req.proposedPrice, totalPayout, msg.sender);

        return totalPayout;
    }

    // ─── 查询函数 ────────────────────────────────────────────────────────────

    /**
     * @notice 检查价格是否已可用（由 UmaCtfAdapter.ready() 调用）
     */
    function hasPrice(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external view returns (bool) {
        bytes32 key = _requestKey(requester, identifier, timestamp, ancillaryData);
        Request storage req = requests[key];
        if (req.state == RequestState.Resolved) return true;
        // Proposed 且 liveness 已过 → 可结算
        if (req.state == RequestState.Proposed &&
            block.timestamp >= uint256(req.proposedAt) + uint256(req.liveness)) {
            return true;
        }
        return false;
    }

    /**
     * @notice 获取价格（由 UmaCtfAdapter.resolve() 调用，仅在 hasPrice=true 后调用）
     */
    function getPrice(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external view returns (int256) {
        bytes32 key = _requestKey(requester, identifier, timestamp, ancillaryData);
        Request storage req = requests[key];
        require(
            req.state == RequestState.Resolved ||
            (req.state == RequestState.Proposed &&
             block.timestamp >= uint256(req.proposedAt) + uint256(req.liveness)),
            "Price not available"
        );
        return req.proposedPrice;
    }

    /**
     * @notice 获取完整请求信息
     */
    function getRequest(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData
    ) external view returns (Request memory) {
        bytes32 key = _requestKey(requester, identifier, timestamp, ancillaryData);
        return requests[key];
    }

    // ─── 测试专用：MockDVM 仲裁 ─────────────────────────────────────────────

    /**
     * @notice 模拟 DVM 仲裁结果（质疑后由 dvm 账户调用）
     * @param resolution true=原提案正确（proposer赢），false=原提案错误（disputer赢）
     */
    function mockDvmSettle(
        address requester,
        bytes32 identifier,
        uint256 timestamp,
        bytes memory ancillaryData,
        bool resolution
    ) external {
        require(msg.sender == dvm, "Only DVM");
        bytes32 key = _requestKey(requester, identifier, timestamp, ancillaryData);
        Request storage req = requests[key];
        require(req.state == RequestState.Disputed, "Not disputed");

        req.state = RequestState.Resolved;

        // 胜者获得两份 bond
        address winner = resolution ? req.proposer : req.disputer;
        uint256 totalBond = req.bond * 2;
        if (totalBond > 0) {
            IERC20(req.currency).transfer(winner, totalBond);
        }
        // reward 给 winner（或质疑时已退款给 requester，此处不重复）
        if (!req.refundOnDispute && req.reward > 0) {
            IERC20(req.currency).transfer(winner, req.reward);
        }

        emit Settle(requester, identifier, timestamp, ancillaryData,
            resolution ? req.proposedPrice : int256(0), totalBond, msg.sender);

        // 若 DVM 推翻提案：需要通知 UmaCtfAdapter（通过 priceDisputed 机制 reset，再用新价格提案）
        // 注：真实 UMA 中此流程更复杂，这里简化为：若推翻则将价格设为相反值
        if (!resolution) {
            // 质疑者赢了，推翻了提案，需要让 UmaCtfAdapter 知道
            // 重新将状态设为 Requested 并允许新的提案（反向结果）
            req.state = RequestState.Requested;
            req.proposer = address(0);
            req.disputer = address(0);
        }
    }

    function setDvm(address newDvm) external {
        require(msg.sender == dvm, "Only DVM");
        dvm = newDvm;
    }
}
