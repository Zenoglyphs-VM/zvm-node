// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

/// @title WrappedQSR
/// @notice Predeployed at 0x4200000000000000000000000000000000000101. QSR
/// bridged from NoM, with NoM's 8 decimals so an amount means the same number
/// on both layers. Only the bridge mints and burns.
contract WrappedQSR {
    string public constant name = "Quasar";
    string public constant symbol = "QSR";
    uint8 public constant decimals = 8;
    address public constant BRIDGE = 0x4200000000000000000000000000000000000100;

    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    error OnlyBridge();
    error InsufficientBalance();
    error InsufficientAllowance();

    function transfer(address to, uint256 value) external returns (bool) {
        _move(msg.sender, to, value);
        return true;
    }

    function approve(address spender, uint256 value) external returns (bool) {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
        return true;
    }

    function transferFrom(address from, address to, uint256 value) external returns (bool) {
        uint256 allowed = allowance[from][msg.sender];
        if (allowed != type(uint256).max) {
            if (allowed < value) revert InsufficientAllowance();
            allowance[from][msg.sender] = allowed - value;
        }
        _move(from, to, value);
        return true;
    }

    function bridgeMint(address to, uint256 value) external {
        if (msg.sender != BRIDGE) revert OnlyBridge();
        totalSupply += value;
        balanceOf[to] += value;
        emit Transfer(address(0), to, value);
    }

    function bridgeBurn(address from, uint256 value) external {
        if (msg.sender != BRIDGE) revert OnlyBridge();
        uint256 bal = balanceOf[from];
        if (bal < value) revert InsufficientBalance();
        balanceOf[from] = bal - value;
        totalSupply -= value;
        emit Transfer(from, address(0), value);
    }

    function _move(address from, address to, uint256 value) internal {
        uint256 bal = balanceOf[from];
        if (bal < value) revert InsufficientBalance();
        balanceOf[from] = bal - value;
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}
