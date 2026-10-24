// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

contract ControlledToken {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    uint256 public totalSupply;
    uint256 public taxBps;
    uint256 public calls;
    uint256 public failAt;
    address private hookTarget;
    bytes private hookData;
    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);
    event Hook(bool ok, bytes result);
    function mint(address to, uint256 amount) external {
        totalSupply += amount; balanceOf[to] += amount; emit Transfer(address(0), to, amount);
    }
    function configure(uint256 tax, uint256 failure, address target, bytes calldata data) external {
        require(tax <= 10_000); taxBps = tax; failAt = failure; hookTarget = target; hookData = data; calls = 0;
    }
    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount; emit Approval(msg.sender, spender, amount); return true;
    }
    function transfer(address to, uint256 amount) external returns (bool) { return move(msg.sender, to, amount); }
    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        uint256 approved = allowance[from][msg.sender];
        if (approved != type(uint256).max) allowance[from][msg.sender] = approved - amount;
        return move(from, to, amount);
    }
    function move(address from, address to, uint256 amount) private returns (bool) {
        balanceOf[from] -= amount;
        uint256 tax = amount * taxBps / 10_000;
        balanceOf[to] += amount - tax; totalSupply -= tax;
        emit Transfer(from, to, amount - tax);
        if (tax != 0) emit Transfer(from, address(0), tax);
        calls++;
        if (hookTarget != address(0)) {
            (bool ok, bytes memory result) = hookTarget.call(hookData); emit Hook(ok, result);
        }
        return failAt == 0 || calls != failAt;
    }
}

contract PayoutWallet {
    uint256 public mode;
    address private target;
    bytes private data;
    event Received(uint256 amount);
    event Callback(bool ok, bytes result);
    function configure(uint256 mode_, address target_, bytes calldata data_) external {mode = mode_; target = target_; data = data_;}
    function execute(address destination, bytes calldata input) external payable returns (bytes memory) {
        (bool ok, bytes memory result) = destination.call{value: msg.value}(input);
        if (!ok) assembly {revert(add(result, 32), mload(result))}
        return result;
    }
    receive() external payable {
        if (mode == 1) revert("fixture rejects native");
        if (mode == 2) { (bool ok, bytes memory result) = target.call(data); emit Callback(ok, result); }
        emit Received(msg.value);
    }
    function onERC721Received(address, address, uint256, bytes calldata) external pure returns (bytes4) {return 0x150b7a02;}
}

contract ContractSigner {
    bytes32 public approvedDigest;
    function setDigest(bytes32 digest) external {approvedDigest = digest;}
    function execute(address destination, bytes calldata input) external returns (bytes memory) {
        (bool ok, bytes memory result) = destination.call(input);
        if (!ok) assembly {revert(add(result, 32), mload(result))}
        return result;
    }
    function isValidSignature(bytes32 digest, bytes calldata signature) external view returns (bytes4) {
        return digest == approvedDigest && keccak256(signature) == keccak256(hex"1122") ? bytes4(0x1626ba7e) : bytes4(0xffffffff);
    }
}

contract RoyaltyNFT {
    mapping(uint256 => address) public ownerOf;
    mapping(uint256 => address) public getApproved;
    mapping(address => mapping(address => bool)) public isApprovedForAll;
    address private receiver;
    uint256 private royalty;
    event Transfer(address indexed from, address indexed to, uint256 indexed tokenId);
    function mint(address to, uint256 id) external {require(ownerOf[id] == address(0)); ownerOf[id] = to; emit Transfer(address(0), to, id);}
    function approve(address spender, uint256 id) external {require(ownerOf[id] == msg.sender); getApproved[id] = spender;}
    function setRoyalty(address to, uint256 amount) external {receiver = to; royalty = amount;}
    function royaltyInfo(uint256, uint256) external view returns (address, uint256) {return (receiver, royalty);}
    function transferFrom(address from, address to, uint256 id) external {
        require(ownerOf[id] == from && getApproved[id] == msg.sender); ownerOf[id] = to; delete getApproved[id]; emit Transfer(from, to, id);
    }
}
