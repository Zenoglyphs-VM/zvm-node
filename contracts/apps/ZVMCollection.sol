// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

import {ERC721} from "@openzeppelin/contracts/token/ERC721/ERC721.sol";
import {ERC2981} from "@openzeppelin/contracts/token/common/ERC2981.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";
import {Strings} from "@openzeppelin/contracts/utils/Strings.sol";

/// @title ZVMCollection
/// @notice An ERC-721 collection built to be cloned. The factory deploys this
/// once; every collection after it is an EIP-1167 proxy initialised in the same
/// transaction. That is what lets a Zenon account create a collection on the
/// native lane, where gas is bought with one account block's plasma: a clone
/// and its initialisation fit that budget, a full ERC-721 deployment does not.
///
/// Metadata is per token (a creator's mint) or a base URI plus the token id (a
/// public drop). Royalties are ERC-2981, capped at 10%. Contract-level metadata
/// follows ERC-7572, and metadata changes are announced with ERC-4906 events.
contract ZVMCollection is ERC721, ERC2981, Ownable {
    using Strings for uint256;

    uint96 public constant MAX_ROYALTY_BPS = 1_000;

    struct Drop {
        uint128 price; // wei of ZNN per token
        uint32 maxSupply; // 0: no cap
        uint32 maxPerWallet; // 0: no cap
        uint32 start; // unix seconds; 0: no drop
        uint32 end; // unix seconds; 0: open-ended
    }

    bool private _initialized;
    string private _collectionName;
    string private _collectionSymbol;
    string private _contractURI;
    string private _baseTokenURI;
    mapping(uint256 => string) private _tokenURIs;

    uint256 public nextTokenId;
    uint256 public totalSupply;
    Drop public drop;
    mapping(address => uint256) public dropMinted;

    event MetadataUpdate(uint256 _tokenId);
    event BatchMetadataUpdate(uint256 _fromTokenId, uint256 _toTokenId);
    event ContractURIUpdated();
    event DropUpdated(uint256 price, uint256 maxSupply, uint256 maxPerWallet, uint256 start, uint256 end);

    error AlreadyInitialized();
    error RoyaltyTooHigh();
    error DropNotOpen();
    error DropSoldOut();
    error WalletLimit();
    error WrongPayment();
    error ZeroQuantity();
    error PayoutFailed();

    constructor() ERC721("", "") Ownable(msg.sender) {
        // The implementation is never used as a collection.
        _initialized = true;
    }

    function initialize(
        address creator,
        string calldata name_,
        string calldata symbol_,
        string calldata contractURI_,
        string calldata baseURI_,
        uint96 royaltyBps
    ) external {
        if (_initialized) revert AlreadyInitialized();
        if (royaltyBps > MAX_ROYALTY_BPS) revert RoyaltyTooHigh();
        _initialized = true;
        _collectionName = name_;
        _collectionSymbol = symbol_;
        _contractURI = contractURI_;
        _baseTokenURI = baseURI_;
        nextTokenId = 1;
        _transferOwnership(creator);
        if (royaltyBps > 0) _setDefaultRoyalty(creator, royaltyBps);
    }

    // --- metadata ---------------------------------------------------------

    function name() public view override returns (string memory) {
        return _collectionName;
    }

    function symbol() public view override returns (string memory) {
        return _collectionSymbol;
    }

    function contractURI() external view returns (string memory) {
        return _contractURI;
    }

    function baseURI() external view returns (string memory) {
        return _baseTokenURI;
    }

    function tokenURI(uint256 tokenId) public view override returns (string memory) {
        _requireOwned(tokenId);
        string memory uri = _tokenURIs[tokenId];
        if (bytes(uri).length > 0) return uri;
        if (bytes(_baseTokenURI).length == 0) return "";
        return string.concat(_baseTokenURI, tokenId.toString());
    }

    function setTokenURI(uint256 tokenId, string calldata uri) external onlyOwner {
        _requireOwned(tokenId);
        _tokenURIs[tokenId] = uri;
        emit MetadataUpdate(tokenId);
    }

    function setBaseURI(string calldata uri) external onlyOwner {
        _baseTokenURI = uri;
        if (nextTokenId > 1) emit BatchMetadataUpdate(1, nextTokenId - 1);
    }

    function setContractURI(string calldata uri) external onlyOwner {
        _contractURI = uri;
        emit ContractURIUpdated();
    }

    function setRoyalty(address receiver, uint96 bps) external onlyOwner {
        if (bps > MAX_ROYALTY_BPS) revert RoyaltyTooHigh();
        if (bps == 0) _deleteDefaultRoyalty();
        else _setDefaultRoyalty(receiver, bps);
    }

    // --- minting ----------------------------------------------------------

    function mint(address to, string calldata uri) external onlyOwner returns (uint256 id) {
        id = _nextId();
        _mint(to, id);
        if (bytes(uri).length > 0) _tokenURIs[id] = uri;
    }

    function mintBatch(address to, string[] calldata uris) external onlyOwner returns (uint256 firstId) {
        firstId = nextTokenId;
        for (uint256 i; i < uris.length; ++i) {
            uint256 id = _nextId();
            _mint(to, id);
            if (bytes(uris[i]).length > 0) _tokenURIs[id] = uris[i];
        }
    }

    function setDrop(uint128 price, uint32 maxSupply, uint32 maxPerWallet, uint32 start, uint32 end) external onlyOwner {
        drop = Drop(price, maxSupply, maxPerWallet, start, end);
        emit DropUpdated(price, maxSupply, maxPerWallet, start, end);
    }

    /// A public mint against the drop. Payment goes straight to the owner.
    function publicMint(uint256 quantity) external payable returns (uint256 firstId) {
        Drop memory d = drop;
        if (quantity == 0) revert ZeroQuantity();
        if (d.start == 0 || block.timestamp < d.start || (d.end != 0 && block.timestamp >= d.end)) revert DropNotOpen();
        if (d.maxSupply != 0 && nextTokenId - 1 + quantity > d.maxSupply) revert DropSoldOut();
        if (d.maxPerWallet != 0 && dropMinted[msg.sender] + quantity > d.maxPerWallet) revert WalletLimit();
        if (msg.value != uint256(d.price) * quantity) revert WrongPayment();
        dropMinted[msg.sender] += quantity;
        firstId = nextTokenId;
        for (uint256 i; i < quantity; ++i) {
            _mint(msg.sender, _nextId());
        }
        if (msg.value > 0) {
            (bool ok,) = payable(owner()).call{value: msg.value}("");
            if (!ok) revert PayoutFailed();
        }
    }

    function burn(uint256 tokenId) external {
        _update(address(0), tokenId, msg.sender);
        totalSupply--;
        delete _tokenURIs[tokenId];
    }

    function _nextId() private returns (uint256 id) {
        id = nextTokenId++;
        totalSupply++;
    }

    function supportsInterface(bytes4 interfaceId) public view override(ERC721, ERC2981) returns (bool) {
        return interfaceId == 0x49064906 || super.supportsInterface(interfaceId);
    }
}
