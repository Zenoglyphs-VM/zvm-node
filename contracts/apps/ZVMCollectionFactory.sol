// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

import {Clones} from "@openzeppelin/contracts/proxy/Clones.sol";
import {ZVMCollection} from "./ZVMCollection.sol";

/// @title ZVMCollectionFactory
/// @notice Creates collections as EIP-1167 clones of one ZVMCollection. A clone
/// costs a few hundred thousand gas with its initialisation, which one native
/// account block's plasma can buy; the caller becomes the owner and the royalty
/// receiver. The same pattern thirdweb, Highlight and Zora's drop creator use.
contract ZVMCollectionFactory {
    address public immutable implementation;
    address[] public collections;
    mapping(address => bool) public isCollection;

    event CollectionCreated(address indexed collection, address indexed creator, string name, string symbol);

    constructor(address implementation_) {
        implementation = implementation_;
    }

    function createCollection(
        string calldata name,
        string calldata symbol,
        string calldata contractURI,
        string calldata baseURI,
        uint96 royaltyBps
    ) external returns (address collection) {
        collection = Clones.clone(implementation);
        ZVMCollection(collection).initialize(msg.sender, name, symbol, contractURI, baseURI, royaltyBps);
        collections.push(collection);
        isCollection[collection] = true;
        emit CollectionCreated(collection, msg.sender, name, symbol);
    }

    function collectionsLength() external view returns (uint256) {
        return collections.length;
    }
}
