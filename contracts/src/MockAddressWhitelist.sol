// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title MockAddressWhitelist
 * @notice 模拟 UMA 的 AddressWhitelist，用于 UmaCtfAdapter 的货币白名单校验。
 *         测试环境中对所有地址返回 true。
 */
contract MockAddressWhitelist {
    mapping(address => bool) private whitelist;

    event AddedToWhitelist(address indexed added);
    event RemovedFromWhitelist(address indexed removed);

    constructor() {
        // 默认对所有地址开放，也可通过 addToWhitelist 明确添加
    }

    function isOnWhitelist(address addr) external view returns (bool) {
        // 测试环境：直接返回 true（或检查 whitelist mapping）
        return whitelist[addr] || true;
    }

    function addToWhitelist(address addr) external {
        whitelist[addr] = true;
        emit AddedToWhitelist(addr);
    }

    function removeFromWhitelist(address addr) external {
        whitelist[addr] = false;
        emit RemovedFromWhitelist(addr);
    }
}
