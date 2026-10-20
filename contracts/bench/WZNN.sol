// SPDX-License-Identifier: GPL-3.0-or-later
pragma solidity 0.8.30;

/// @title Wrapped ZNN
/// @notice The ERC-20 wrapper for this chain's native ZNN.
///
/// A faithful port of WETH9, which is what Uniswap V2's periphery is written
/// against: the router calls `deposit`, `withdraw` and `transfer` on whatever
/// address it was constructed with, and expects exactly these semantics.
/// Deliberately unchanged from the original are the 2300-gas `transfer` on
/// withdrawal, `totalSupply` reading the contract balance rather than a stored
/// counter, and the infinite-allowance shortcut in `transferFrom`.
///
/// What is different is the pair of strings below. The chain previously
/// deployed WETH9 unmodified, so its wrapped native token introduced itself as
/// "Wrapped Ether (WETH)" on a chain with no ether on it: every explorer,
/// wallet and pool label inherited a name for an asset that does not exist
/// here. The bytecode is only reachable through CREATE, so correcting the name
/// does not move the address it deploys to.
contract WZNN {
    string public name = "Wrapped ZNN";
    string public symbol = "WZNN";
    uint8 public decimals = 18;

    event Approval(address indexed src, address indexed guy, uint256 wad);
    event Transfer(address indexed src, address indexed dst, uint256 wad);
    event Deposit(address indexed dst, uint256 wad);
    event Withdrawal(address indexed src, uint256 wad);

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    receive() external payable {
        deposit();
    }

    function deposit() public payable {
        balanceOf[msg.sender] += msg.value;
        emit Deposit(msg.sender, msg.value);
    }

    function withdraw(uint256 wad) public {
        require(balanceOf[msg.sender] >= wad, "WZNN: insufficient balance");
        balanceOf[msg.sender] -= wad;
        payable(msg.sender).transfer(wad);
        emit Withdrawal(msg.sender, wad);
    }

    function totalSupply() public view returns (uint256) {
        return address(this).balance;
    }

    function approve(address guy, uint256 wad) public returns (bool) {
        allowance[msg.sender][guy] = wad;
        emit Approval(msg.sender, guy, wad);
        return true;
    }

    function transfer(address dst, uint256 wad) public returns (bool) {
        return transferFrom(msg.sender, dst, wad);
    }

    function transferFrom(address src, address dst, uint256 wad) public returns (bool) {
        require(balanceOf[src] >= wad, "WZNN: insufficient balance");

        if (src != msg.sender && allowance[src][msg.sender] != type(uint256).max) {
            require(allowance[src][msg.sender] >= wad, "WZNN: insufficient allowance");
            allowance[src][msg.sender] -= wad;
        }

        balanceOf[src] -= wad;
        balanceOf[dst] += wad;

        emit Transfer(src, dst, wad);
        return true;
    }
}
