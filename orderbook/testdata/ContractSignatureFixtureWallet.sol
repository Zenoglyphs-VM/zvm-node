// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

// A declared test account: the signing owner and the NFT trader are distinct.
// The modes exercise actual ERC-1271 replies, not RPC answer substitution.
contract ContractSignatureFixtureWallet {
    address public owner;
    uint8 public signatureMode;
    bytes4 private constant MAGIC = 0x1626ba7e;
    bytes4 private constant INVALID = 0xffffffff;

    error FixtureOwnerRequired();
    error FixtureSignatureDisabled();
    event SignatureModeChanged(uint8 mode);
    event Received(address indexed from, uint256 amount);

    constructor(address owner_) {
        require(owner_ != address(0), "fixture owner required");
        owner = owner_;
    }

    function setSignatureMode(uint8 mode) external {
        if (msg.sender != owner) revert FixtureOwnerRequired();
        require(mode <= 3, "fixture mode bound");
        signatureMode = mode;
        emit SignatureModeChanged(mode);
    }

    function isValidSignature(bytes32 digest, bytes calldata signature) external view returns (bytes4) {
        if (signatureMode == 1) return INVALID;
        if (signatureMode == 2) revert FixtureSignatureDisabled();
        if (signatureMode == 3) {
            assembly {
                mstore(0, shl(224, 0x1626ba7e))
                return(0, 4)
            }
        }
        if (signature.length != 65) return INVALID;
        bytes32 r;
        bytes32 s;
        uint8 v;
        assembly {
            r := calldataload(signature.offset)
            s := calldataload(add(signature.offset, 32))
            v := byte(0, calldataload(add(signature.offset, 64)))
        }
        if (v != 27 && v != 28) return INVALID;
        if (uint256(s) > 0x7fffffffffffffffffffffffffffffff5d576e7357a4501ddfe92f46681b20a0) return INVALID;
        return ecrecover(digest, v, r, s) == owner ? MAGIC : INVALID;
    }

    function execute(address destination, bytes calldata input) external returns (bytes memory) {
        if (msg.sender != owner) revert FixtureOwnerRequired();
        (bool ok, bytes memory result) = destination.call(input);
        if (!ok) assembly {revert(add(result, 32), mload(result))}
        return result;
    }

    function onERC721Received(address, address, uint256, bytes calldata) external pure returns (bytes4) {
        return 0x150b7a02;
    }

    receive() external payable {
        emit Received(msg.sender, msg.value);
    }
}
