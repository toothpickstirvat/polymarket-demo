// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title MockFinder
 * @notice 模拟 UMA Finder 合约，用于测试环境。
 *         维护 interfaceName → address 映射，供 UmaCtfAdapterV3 动态查询 OO 地址。
 *
 * 官方 UMA Finder（Polygon mainnet: 0x09aea4b2242abC8bb4BB78D537A67a245A7bEC64）
 * 提供完全相同的接口，是 UMA 合约体系的核心升级机制——
 * 当 OO 升级时，只需在 Finder 中更新一条记录，所有依赖 Finder 的合约
 * 下次调用时会自动使用新地址，无需重新部署适配层合约。
 *
 * 本合约只实现测试所需的最小接口，省略了官方版本中的治理权限控制。
 */
contract MockFinder {
    // interfaceName（bytes32）→ 合约地址
    mapping(bytes32 => address) private implementations;

    event InterfaceImplementationChanged(
        bytes32 indexed interfaceName,
        address indexed newImplementationAddress
    );

    /**
     * @notice 注册或更新接口实现地址
     * @param interfaceName          接口名称（右填充零到 32 字节，如 "OptimisticOracleV3"）
     * @param implementationAddress  对应合约地址
     */
    function changeImplementationAddress(
        bytes32 interfaceName,
        address implementationAddress
    ) external {
        implementations[interfaceName] = implementationAddress;
        emit InterfaceImplementationChanged(interfaceName, implementationAddress);
    }

    /**
     * @notice 查询接口实现地址
     * @param interfaceName  接口名称
     * @return               注册的合约地址（未注册则返回零地址）
     */
    function getImplementationAddress(bytes32 interfaceName) external view returns (address) {
        return implementations[interfaceName];
    }
}
