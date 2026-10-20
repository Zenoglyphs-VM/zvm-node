// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// Ordinary OpenZeppelin tokens, compiled unmodified: the point of the
// benchmark is that nothing about the ZVM asks a contract to be special.

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {ERC721} from "@openzeppelin/contracts/token/ERC721/ERC721.sol";
import {ERC1155} from "@openzeppelin/contracts/token/ERC1155/ERC1155.sol";

contract BenchERC20 is ERC20 {
    constructor(string memory name_, string memory symbol_, uint256 supply) ERC20(name_, symbol_) {
        _mint(msg.sender, supply);
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }

    /// @notice Funds many benchmark wallets in one transaction.
    function airdrop(address[] calldata recipients, uint256 amount) external {
        for (uint256 i; i < recipients.length; ++i) {
            _mint(recipients[i], amount);
        }
    }
}

contract BenchERC721 is ERC721 {
    uint256 public nextId = 1;

    constructor() ERC721("ZVM Bench NFT", "ZBN") {}

    function mint(address to) external returns (uint256 id) {
        id = nextId++;
        _mint(to, id);
    }
}

contract BenchERC1155 is ERC1155 {
    constructor() ERC1155("https://zvm.invalid/{id}.json") {}

    function mint(address to, uint256 id, uint256 amount) external {
        _mint(to, id, amount, "");
    }
}

/// @notice Writes cold storage slots: the axis a rollup pays for forever.
contract StorageChurn {
    mapping(uint256 => uint256) public slots;
    uint256 public counter;

    function churn(uint256 n) external {
        uint256 c = counter;
        for (uint256 i; i < n; ++i) {
            slots[c + i] = block.number + i + 1;
        }
        counter = c + n;
    }
}

/// @notice Funds many addresses in one transaction, so a benchmark's setup
/// does not itself become the load.
contract Disperse {
    function disperse(address[] calldata recipients, uint256 amount) external payable {
        for (uint256 i; i < recipients.length; ++i) {
            (bool ok,) = recipients[i].call{value: amount}("");
            require(ok, "send failed");
        }
        uint256 left = address(this).balance;
        if (left > 0) {
            (bool ok,) = msg.sender.call{value: left}("");
            require(ok, "refund failed");
        }
    }
}
