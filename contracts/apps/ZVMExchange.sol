// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {IERC721} from "@openzeppelin/contracts/token/ERC721/IERC721.sol";
import {IERC2981} from "@openzeppelin/contracts/interfaces/IERC2981.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";
import {EIP712} from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import {SignatureChecker} from "@openzeppelin/contracts/utils/cryptography/SignatureChecker.sol";
import {MerkleProof} from "@openzeppelin/contracts/utils/cryptography/MerkleProof.sol";

/// @title ZVMExchange
/// @notice ERC-721 orders signed off chain and settled on chain, the way
/// OpenSea's Seaport and Blur's exchange work (DECISIONS.md D28).
///
/// A listing or a bid is an EIP-712 signature, which costs nothing and needs no
/// transaction; only filling one does. The shape is Blur's: one flat `Order`, a
/// per-trader nonce whose bump cancels everything the trader has signed, and
/// **bulk signing** — one signature over the Merkle root of many orders, each
/// then filled with its own proof, so listing fifty items is one wallet prompt.
///
/// Two ways in besides an ECDSA signature, both taken from Seaport:
///
///   - a contract wallet signs through ERC-1271 (`SignatureChecker`), and
///   - `approveOrders` puts an order on chain, as Seaport's `validate` does, so
///     an account with no secp256k1 key can still list. On this chain that
///     includes a Zenon account on the native lane, whose only signature is
///     the NoM block itself (D6): the approval *is* that block.
///
/// Listings do not escrow the token, as on OpenSea: a listing is fillable while
/// the seller still holds the token and still approves this contract. Bids do
/// not escrow either: they are paid in an ERC-20 — this chain's wrapped ZNN —
/// with an allowance, as OpenSea's offers are paid in WETH. Royalties are
/// ERC-2981, paid by default and capped at 10%.
contract ZVMExchange is Ownable, ReentrancyGuard, EIP712 {
    using SafeERC20 for IERC20;

    enum Side {
        Sell,
        Buy
    }

    /// How an order's trader agreed to it.
    enum SigKind {
        Single, // an EIP-712 signature over this order
        Bulk, // an EIP-712 signature over a Merkle root this order is a leaf of
        OnChain // approveOrders, sent by the trader
    }

    struct Order {
        address trader;
        Side side;
        address collection;
        uint256 tokenId; // ignored when anyToken is set
        bool anyToken; // a bid on any token of the collection
        uint256 amount; // items this order may fill: 1 for a listing
        address paymentToken; // address(0) is ZNN; a bid names an ERC-20
        uint256 price; // per item
        uint64 listingTime;
        uint64 expirationTime;
        uint256 nonce; // the trader's nonce when the order was made
        uint256 salt;
    }

    struct Input {
        Order order;
        SigKind kind;
        bytes signature;
        bytes32[] proof;
    }

    bytes32 public constant ORDER_TYPEHASH = keccak256(
        "Order(address trader,uint8 side,address collection,uint256 tokenId,bool anyToken,uint256 amount,address paymentToken,uint256 price,uint64 listingTime,uint64 expirationTime,uint256 nonce,uint256 salt)"
    );
    bytes32 public constant ROOT_TYPEHASH = keccak256("OrderRoot(bytes32 root)");

    address public constant NATIVE = address(0);
    uint256 public constant MAX_FEE_BPS = 250;
    uint256 public constant MAX_ROYALTY_BPS = 1_000;
    /// Enough for a smart-contract wallet to accept ZNN, not enough to grief a sale.
    uint256 private constant PAYOUT_GAS = 100_000;

    /// Why checkOrder says an order cannot be filled; zero means it can.
    uint8 public constant OK = 0;
    uint8 public constant BAD_SIGNATURE = 1;
    uint8 public constant CANCELLED = 2;
    uint8 public constant STALE_NONCE = 3;
    uint8 public constant NOT_STARTED = 4;
    uint8 public constant EXPIRED = 5;
    uint8 public constant FILLED = 6;
    uint8 public constant NOT_HELD = 7; // a listing's seller no longer holds or approves the token
    uint8 public constant NOT_FUNDED = 8; // a bid's trader lacks the balance or the allowance
    uint8 public constant MALFORMED = 9;

    uint256 public feeBps;
    address public feeRecipient;

    mapping(address => uint256) public nonces;
    mapping(bytes32 => uint256) public filled;
    mapping(bytes32 => bool) public cancelled;
    mapping(bytes32 => bool) public approvedOnChain;
    /// ZNN owed to an address that refused a direct payment.
    mapping(address => uint256) public credits;

    event OrderFilled(
        bytes32 indexed orderHash,
        address indexed maker,
        address indexed taker,
        Side side,
        address collection,
        uint256 tokenId,
        address paymentToken,
        uint256 price,
        uint256 royalty,
        uint256 fee
    );
    event OrderCancelled(bytes32 indexed orderHash, address indexed trader);
    event NonceIncremented(address indexed trader, uint256 nonce);
    /// Carries the whole order, so an order approved on chain reaches every
    /// order book from the log alone.
    event OrderApproved(bytes32 indexed orderHash, address indexed trader, Order order);
    event Credited(address indexed account, uint256 amount);
    event FeeUpdated(address recipient, uint256 bps);

    error Unfillable(bytes32 orderHash, uint8 code);
    error NotTrader();
    error WrongPayment();
    error WrongSide();
    error FeeTooHigh();
    error NothingOwed();
    error PayoutFailed();

    constructor(address owner_, address feeRecipient_, uint256 feeBps_) Ownable(owner_) EIP712("ZVM Exchange", "1") {
        _setFee(feeRecipient_, feeBps_);
    }

    // --- hashing ------------------------------------------------------------

    function hashOrder(Order calldata o) public pure returns (bytes32) {
        return keccak256(
            abi.encode(
                ORDER_TYPEHASH,
                o.trader,
                o.side,
                o.collection,
                o.tokenId,
                o.anyToken,
                o.amount,
                o.paymentToken,
                o.price,
                o.listingTime,
                o.expirationTime,
                o.nonce,
                o.salt
            )
        );
    }

    /// What a wallet signs for one order.
    function orderDigest(bytes32 orderHash) public view returns (bytes32) {
        return _hashTypedDataV4(orderHash);
    }

    /// What a wallet signs for a Merkle root of orders.
    function rootDigest(bytes32 root) public view returns (bytes32) {
        return _hashTypedDataV4(keccak256(abi.encode(ROOT_TYPEHASH, root)));
    }

    function domainSeparator() external view returns (bytes32) {
        return _domainSeparatorV4();
    }

    // --- filling ------------------------------------------------------------

    /// Buys one item from a listing. A ZNN listing is paid with exactly its
    /// price; an ERC-20 listing with an allowance and no ZNN.
    function buy(Input calldata sell) external payable nonReentrant {
        if (msg.value != _buy(sell, msg.sender)) revert WrongPayment();
    }

    /// Buys every listing that can still be filled and skips the rest, the way
    /// Seaport's fulfillAvailable does, so one listing sold a moment earlier
    /// does not sink a sweep. Unspent ZNN is returned.
    function buyMany(Input[] calldata sells) external payable nonReentrant returns (uint256 bought) {
        uint256 spent;
        for (uint256 i; i < sells.length; ++i) {
            (uint8 code,) = _code(sells[i]);
            if (code != OK || sells[i].order.side != Side.Sell) continue;
            spent += _buy(sells[i], msg.sender);
            ++bought;
        }
        if (spent > msg.value) revert WrongPayment();
        if (msg.value > spent) _sendNative(msg.sender, msg.value - spent);
    }

    /// Sells `tokenId`, which the caller holds, into a bid.
    function acceptBid(Input calldata bid, uint256 tokenId) external nonReentrant {
        Order calldata o = bid.order;
        if (o.side != Side.Buy) revert WrongSide();
        bytes32 h = _consume(bid);
        if (!o.anyToken && o.tokenId != tokenId) revert Unfillable(h, MALFORMED);
        IERC721(o.collection).transferFrom(msg.sender, o.trader, tokenId);
        (uint256 royalty, uint256 fee) = _settle(o.collection, tokenId, o.paymentToken, o.price, o.trader, msg.sender);
        emit OrderFilled(h, o.trader, msg.sender, Side.Buy, o.collection, tokenId, o.paymentToken, o.price, royalty, fee);
    }

    // --- cancelling and approving ------------------------------------------

    function cancelOrders(Order[] calldata orders) external {
        for (uint256 i; i < orders.length; ++i) {
            if (orders[i].trader != msg.sender) revert NotTrader();
            bytes32 h = hashOrder(orders[i]);
            cancelled[h] = true;
            emit OrderCancelled(h, msg.sender);
        }
    }

    /// Cancels every order the caller has signed so far.
    function incrementNonce() external {
        uint256 n = ++nonces[msg.sender];
        emit NonceIncremented(msg.sender, n);
    }

    /// Agrees to orders with a transaction instead of a signature.
    function approveOrders(Order[] calldata orders) external {
        for (uint256 i; i < orders.length; ++i) {
            if (orders[i].trader != msg.sender) revert NotTrader();
            bytes32 h = hashOrder(orders[i]);
            approvedOnChain[h] = true;
            emit OrderApproved(h, msg.sender, orders[i]);
        }
    }

    // --- reading ------------------------------------------------------------

    /// Whether an order can be filled right now, and if not, why. An order
    /// book calls this before it shows an order, which is the same check a
    /// fill makes.
    function checkOrder(Input calldata input) external view returns (uint8 code, uint256 remaining) {
        return _code(input);
    }

    function withdrawCredits() external nonReentrant {
        uint256 amount = credits[msg.sender];
        if (amount == 0) revert NothingOwed();
        credits[msg.sender] = 0;
        (bool ok,) = payable(msg.sender).call{value: amount}("");
        if (!ok) revert PayoutFailed();
    }

    // --- owner --------------------------------------------------------------

    function setFee(address recipient, uint256 bps) external onlyOwner {
        _setFee(recipient, bps);
    }

    // --- internals ----------------------------------------------------------

    function _setFee(address recipient, uint256 bps) private {
        if (bps > MAX_FEE_BPS) revert FeeTooHigh();
        feeRecipient = recipient;
        feeBps = bps;
        emit FeeUpdated(recipient, bps);
    }

    /// Returns the ZNN this purchase takes from msg.value.
    function _buy(Input calldata sell, address buyer) private returns (uint256) {
        Order calldata o = sell.order;
        if (o.side != Side.Sell) revert WrongSide();
        if (o.trader == buyer) revert Unfillable(hashOrder(o), MALFORMED);
        bytes32 h = _consume(sell);
        // Reverts if the seller no longer holds the token or has withdrawn the approval.
        IERC721(o.collection).transferFrom(o.trader, buyer, o.tokenId);
        (uint256 royalty, uint256 fee) = _settle(o.collection, o.tokenId, o.paymentToken, o.price, buyer, o.trader);
        emit OrderFilled(h, o.trader, buyer, Side.Sell, o.collection, o.tokenId, o.paymentToken, o.price, royalty, fee);
        return o.paymentToken == NATIVE ? o.price : 0;
    }

    /// Checks an order and records one item of it as filled.
    function _consume(Input calldata input) private returns (bytes32 h) {
        uint8 code;
        (code,) = _code(input);
        h = hashOrder(input.order);
        if (code != OK) revert Unfillable(h, code);
        filled[h] += 1;
    }

    function _code(Input calldata input) private view returns (uint8, uint256) {
        Order calldata o = input.order;
        bytes32 h = hashOrder(o);
        if (o.amount == 0 || o.price == 0 || o.trader == address(0)) return (MALFORMED, 0);
        if (o.side == Side.Sell && (o.amount != 1 || o.anyToken)) return (MALFORMED, 0);
        if (o.side == Side.Buy && o.paymentToken == NATIVE) return (MALFORMED, 0);
        if (!_authorised(input, h)) return (BAD_SIGNATURE, 0);
        if (cancelled[h]) return (CANCELLED, 0);
        if (o.nonce != nonces[o.trader]) return (STALE_NONCE, 0);
        if (o.listingTime > block.timestamp) return (NOT_STARTED, 0);
        if (o.expirationTime <= block.timestamp) return (EXPIRED, 0);
        uint256 used = filled[h];
        if (used >= o.amount) return (FILLED, 0);
        uint256 remaining = o.amount - used;
        if (o.side == Side.Sell) {
            if (!_holds(o.collection, o.tokenId, o.trader)) return (NOT_HELD, remaining);
        } else {
            IERC20 token = IERC20(o.paymentToken);
            if (!_covers(token, o.trader, o.price)) return (NOT_FUNDED, remaining);
        }
        return (OK, remaining);
    }

    function _authorised(Input calldata input, bytes32 h) private view returns (bool) {
        if (input.kind == SigKind.OnChain) return approvedOnChain[h];
        bytes32 digest = input.kind == SigKind.Single
            ? _hashTypedDataV4(h)
            : rootDigest(MerkleProof.processProofCalldata(input.proof, h));
        return SignatureChecker.isValidSignatureNow(input.order.trader, digest, input.signature);
    }

    function _holds(address collection, uint256 tokenId, address seller) private view returns (bool) {
        (bool ok, bytes memory ret) = collection.staticcall(abi.encodeCall(IERC721.ownerOf, (tokenId)));
        if (!ok || ret.length < 32 || abi.decode(ret, (address)) != seller) return false;
        (ok, ret) = collection.staticcall(abi.encodeCall(IERC721.isApprovedForAll, (seller, address(this))));
        if (ok && ret.length >= 32 && abi.decode(ret, (bool))) return true;
        (ok, ret) = collection.staticcall(abi.encodeCall(IERC721.getApproved, (tokenId)));
        return ok && ret.length >= 32 && abi.decode(ret, (address)) == address(this);
    }

    function _covers(IERC20 token, address who, uint256 amount) private view returns (bool) {
        (bool ok, bytes memory ret) = address(token).staticcall(abi.encodeCall(IERC20.balanceOf, (who)));
        if (!ok || ret.length < 32 || abi.decode(ret, (uint256)) < amount) return false;
        (ok, ret) = address(token).staticcall(abi.encodeCall(IERC20.allowance, (who, address(this))));
        return ok && ret.length >= 32 && abi.decode(ret, (uint256)) >= amount;
    }

    function _settle(address collection, uint256 tokenId, address currency, uint256 price, address payer, address seller)
        private
        returns (uint256 royalty, uint256 fee)
    {
        address receiver;
        (receiver, royalty) = _royalty(collection, tokenId, price);
        if (feeRecipient != address(0)) fee = (price * feeBps) / 10_000;
        uint256 proceeds = price - royalty - fee;
        if (currency == NATIVE) {
            _sendNative(receiver, royalty);
            _sendNative(feeRecipient, fee);
            _sendNative(seller, proceeds);
        } else {
            IERC20 token = IERC20(currency);
            if (royalty != 0) token.safeTransferFrom(payer, receiver, royalty);
            if (fee != 0) token.safeTransferFrom(payer, feeRecipient, fee);
            if (proceeds != 0) token.safeTransferFrom(payer, seller, proceeds);
        }
    }

    function _royalty(address collection, uint256 tokenId, uint256 price)
        private
        view
        returns (address receiver, uint256 amount)
    {
        (bool ok, bytes memory ret) = collection.staticcall(abi.encodeCall(IERC2981.royaltyInfo, (tokenId, price)));
        if (!ok || ret.length < 64) return (address(0), 0);
        (receiver, amount) = abi.decode(ret, (address, uint256));
        if (receiver == address(0)) return (address(0), 0);
        uint256 cap = (price * MAX_ROYALTY_BPS) / 10_000;
        if (amount > cap) amount = cap;
    }

    function _sendNative(address to, uint256 amount) private {
        if (amount == 0) return;
        (bool ok,) = payable(to).call{value: amount, gas: PAYOUT_GAS}("");
        if (!ok) {
            credits[to] += amount;
            emit Credited(to, amount);
        }
    }
}
