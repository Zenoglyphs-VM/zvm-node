// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

// A declared test account adapter, not a supported production wallet.
// Adapted from the retained PayoutWallet fixture with explicit owner authority.
contract CreditFixtureWallet {
    address public owner;
    bool public rejectsNative = true;

    event Received(address indexed from, uint256 amount);

    constructor(address owner_) {
        require(owner_ != address(0));
        owner = owner_;
    }

    function setRejectsNative(bool rejects) external {
        require(msg.sender == owner, "fixture owner required");
        rejectsNative = rejects;
    }

    function execute(address destination, bytes calldata input) external returns (bytes memory) {
        require(msg.sender == owner, "fixture owner required");
        (bool ok, bytes memory result) = destination.call(input);
        if (!ok) assembly {revert(add(result, 32), mload(result))}
        return result;
    }

    receive() external payable {
        require(!rejectsNative, "fixture rejects native");
        emit Received(msg.sender, msg.value);
    }
}
