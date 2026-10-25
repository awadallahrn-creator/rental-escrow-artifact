// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// @notice 6-decimal test stablecoin (USDC-like), including an issuer blocklist.
contract MockStablecoin is ERC20 {
    mapping(address => bool) public blocked;

    constructor() ERC20("Mock USD", "mUSD") {}

    function setBlocked(address who, bool isBlocked) external {
        blocked[who] = isBlocked;
    }

    function _update(address from, address to, uint256 value) internal override {
        require(!blocked[from] && !blocked[to], "blocked");
        super._update(from, to, value);
    }

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}

/// @notice Token that burns 1% on every transfer; used to test rejection of non-standard tokens.
contract FeeOnTransferToken is ERC20 {
    constructor() ERC20("Fee Token", "FEE") {}

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }

    function _update(address from, address to, uint256 value) internal override {
        if (from != address(0) && to != address(0)) {
            uint256 fee = value / 100;
            super._update(from, address(0), fee);
            super._update(from, to, value - fee);
        } else {
            super._update(from, to, value);
        }
    }
}

/// @notice Plain 6-decimal ERC-20 with no extra transfer logic; used for gas measurement so that
///         reported figures reflect the rental contracts, not token-specific features.
contract PlainStablecoin is ERC20 {
    constructor() ERC20("Plain USD", "pUSD") {}

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}
