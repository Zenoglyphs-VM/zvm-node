// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

/// Test-only external ERC-721/ERC-2981 implementation for the existing clone factory.
/// Token 1 reports a royalty above the exchange cap; token 2 reverts its royalty query.
contract RoyaltyFixtureCollection {
    bool private initialized;
    address public owner;
    string public name;
    string public symbol;
    string public contractURI;
    uint256 public nextTokenId;
    address private receiver;
    uint96 private reportedBps;
    mapping(uint256 => address) private owners;
    mapping(address => uint256) private balances;
    mapping(uint256 => address) public getApproved;
    mapping(address => mapping(address => bool)) public isApprovedForAll;
    mapping(uint256 => string) private uris;

    event Transfer(address indexed from, address indexed to, uint256 indexed tokenId);
    event Approval(address indexed owner, address indexed approved, uint256 indexed tokenId);
    event ApprovalForAll(address indexed owner, address indexed operator, bool approved);
    error RoyaltyUnavailable();

    constructor() {initialized = true;}
    modifier onlyOwner() {require(msg.sender == owner, "Only fixture owner"); _;}

    function initialize(address creator, string calldata name_, string calldata symbol_, string calldata contractURI_,
        string calldata, uint96) external {
        require(!initialized && creator != address(0), "Already initialized");
        initialized = true; owner = creator; name = name_; symbol = symbol_; contractURI = contractURI_; nextTokenId = 1;
    }
    function supportsInterface(bytes4 id) external pure returns (bool) {
        return id == 0x01ffc9a7 || id == 0x80ac58cd || id == 0x5b5e139f || id == 0x2a55205a;
    }
    function balanceOf(address account) external view returns (uint256) {
        require(account != address(0), "Zero owner"); return balances[account];
    }
    function ownerOf(uint256 id) public view returns (address) {
        address result = owners[id]; require(result != address(0), "Unknown token"); return result;
    }
    function tokenURI(uint256 id) external view returns (string memory) {ownerOf(id); return uris[id];}
    function mintBatch(address to, string[] calldata values) external onlyOwner returns (uint256 first) {
        require(to != address(0), "Zero recipient"); first = nextTokenId;
        for (uint256 i; i < values.length; i++) {
            uint256 id = nextTokenId++; owners[id] = to; balances[to]++; uris[id] = values[i];
            emit Transfer(address(0), to, id);
        }
    }
    function approve(address approved, uint256 id) external {
        address current = ownerOf(id); require(msg.sender == current || isApprovedForAll[current][msg.sender], "Not authorized");
        getApproved[id] = approved; emit Approval(current, approved, id);
    }
    function setApprovalForAll(address operator, bool approved) external {
        require(operator != msg.sender, "Self approval"); isApprovedForAll[msg.sender][operator] = approved;
        emit ApprovalForAll(msg.sender, operator, approved);
    }
    function transferFrom(address from, address to, uint256 id) public {
        require(ownerOf(id) == from && to != address(0), "Invalid transfer");
        require(msg.sender == from || getApproved[id] == msg.sender || isApprovedForAll[from][msg.sender], "Not authorized");
        delete getApproved[id]; owners[id] = to; balances[from]--; balances[to]++;
        emit Transfer(from, to, id);
    }
    function safeTransferFrom(address from, address to, uint256 id) external {safeTransferFrom(from, to, id, "");}
    function safeTransferFrom(address from, address to, uint256 id, bytes memory data) public {
        transferFrom(from, to, id);
        if (to.code.length != 0) {
            (bool ok, bytes memory result) = to.call(abi.encodeWithSignature("onERC721Received(address,address,uint256,bytes)", msg.sender, from, id, data));
            require(ok && result.length >= 32 && abi.decode(result, (bytes4)) == 0x150b7a02, "Receiver refused");
        }
    }
    function setRoyalty(address target, uint96 bps) external onlyOwner {
        require(bps <= 10_000, "Invalid fixture royalty"); receiver = target; reportedBps = bps;
    }
    function royaltyInfo(uint256 id, uint256 price) external view returns (address, uint256) {
        if (id == 2) revert RoyaltyUnavailable();
        return (receiver, price * reportedBps / 10_000);
    }
}
