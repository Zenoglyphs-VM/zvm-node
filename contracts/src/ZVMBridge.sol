// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

import {WrappedQSR} from "./WrappedQSR.sol";

/// @title ZVMBridge
/// @notice Predeployed at 0x4200000000000000000000000000000000000100.
///
/// Value enters the ZVM when someone sends ZNN or QSR to the bridge vault on
/// NoM. Every ZVM node sees that send and derives a deposit transaction: ZNN is
/// minted to the sender natively, QSR is minted here as an ERC-20. Nobody signs
/// a deposit.
///
/// Value leaves when a holder calls withdrawZNN or withdrawQSR. The amount is
/// locked (ZNN) or burned (QSR) and a withdrawal is recorded. The vault
/// committee pays it on NoM from the vault, tagging the payout with the id, and
/// every ZVM node derives a markPaid call from that NoM block. A payout that
/// does not match a recorded withdrawal is not silently accepted: it is
/// reported as an anomaly, so the peg can be audited from ZVM state alone.
///
/// No constructor and no immutables: the runtime code is placed directly in
/// the genesis state, and so are the fee parameters (slots 10–12).
///
/// A withdrawal pays a fee, the way Zenon's own bridge charges FeePercentage
/// on a wrap and accumulates it per token: a share in basis points, never less
/// than a per-token floor. The fee is kept, not paid out, so the vault on NoM
/// ends up holding more than it owes. That surplus is the bridge's revenue,
/// and the committee fuses the QSR part to the vault's own plasma, which is
/// what pays for receiving deposits and publishing payouts (docs/PLASMA.md).
/// `amount` in a withdrawal is always what NoM pays, so a payout still
/// matches its record exactly.
contract ZVMBridge {
    /// @notice Sender of deposit transactions derived from NoM facts.
    address public constant SYSTEM = 0xDeAddEAddeADdeADdEaDdEaddeadDeADdEAD0002;
    address public constant QSR_TOKEN = 0x4200000000000000000000000000000000000101;

    /// @notice zts1znnxxxxxxxxxxxxx9z4ulx and zts1qsrxxxxxxxxxxxxxmrhjll.
    bytes10 public constant ZNN = 0x14e66318c6318c6318c6;
    bytes10 public constant QSR = 0x04066318c6318c6318c6;

    /// @notice One NoM base unit (1e-8 ZNN) is 1e10 wei on the ZVM.
    uint256 public constant ZNN_SCALE = 1e10;

    uint256 public constant FEE_DENOMINATOR = 10_000;

    struct Withdrawal {
        address from;
        address to;
        bytes10 zts;
        uint64 amount; // NoM base units, what the payout sends
        uint64 l2Block;
        bytes32 paidIn; // the NoM account block that paid it
        uint64 fee; // NoM base units, kept by the vault
    }

    uint256 public withdrawalCount;
    uint256 public pendingZNN;
    uint256 public pendingQSR;
    uint256 public paidZNN;
    uint256 public paidQSR;
    uint256 public depositedQSR;

    mapping(uint256 => Withdrawal) internal _withdrawals;
    mapping(bytes32 => bool) public payoutSeen;

    /// @notice Fees kept, in NoM base units.
    uint256 public feesZNN;
    uint256 public feesQSR;
    /// @notice Set in the genesis state (slots 10, 11, 12); zero means no fee.
    uint256 public feeBps;
    uint256 public minFeeZNN;
    uint256 public minFeeQSR;
    /// @notice QSR the vault has fused on NoM, derived from its own sends to
    /// the plasma contract. The fee revenue not yet spent on plasma is
    /// feesQSR - fusedQSR, which a committee member can read from ZVM state
    /// alone before agreeing to fuse more.
    uint256 public fusedQSR;

    event WithdrawalInitiated(uint256 indexed id, address indexed from, address indexed to, bytes10 zts, uint256 amount);
    event WithdrawalFee(uint256 indexed id, bytes10 zts, uint256 fee);
    event WithdrawalPaid(uint256 indexed id, bytes32 indexed nomBlock);
    /// @param reason 1 unknown id, 2 already paid, 3 recipient/token/amount mismatch
    event PayoutAnomaly(uint256 indexed id, bytes32 indexed nomBlock, uint8 reason);
    event QSRDeposited(bytes32 indexed nomBlock, address indexed to, uint256 amount);
    event VaultFused(bytes32 indexed nomBlock, address indexed beneficiary, uint256 amount);

    error OnlySystem();
    error NotAUserAddress();
    error BadAmount();

    modifier onlySystem() {
        if (msg.sender != SYSTEM) revert OnlySystem();
        _;
    }

    /// @notice Withdraw native ZNN to a NoM user address. msg.value must be a
    /// whole number of NoM base units (a multiple of 1e10 wei).
    /// The ZNN stays locked here in full; the fee part is simply never paid out.
    function withdrawZNN(address nomRecipient) external payable returns (uint256 id) {
        if (msg.value == 0 || msg.value % ZNN_SCALE != 0) revert BadAmount();
        uint256 amount = msg.value / ZNN_SCALE;
        if (amount > type(uint64).max) revert BadAmount();
        uint256 fee = feeFor(ZNN, amount);
        id = _record(nomRecipient, ZNN, amount, fee);
        pendingZNN += amount - fee;
        feesZNN += fee;
    }

    /// @notice Withdraw bridged QSR (8 decimals, same units as NoM). The whole
    /// amount is burned; the vault pays it less the fee.
    function withdrawQSR(address nomRecipient, uint256 amount) external returns (uint256 id) {
        if (amount == 0 || amount > type(uint64).max) revert BadAmount();
        uint256 fee = feeFor(QSR, amount);
        WrappedQSR(QSR_TOKEN).bridgeBurn(msg.sender, amount);
        id = _record(nomRecipient, QSR, amount, fee);
        pendingQSR += amount - fee;
        feesQSR += fee;
    }

    /// @notice The fee a withdrawal of `amount` base units of `zts` pays: the
    /// basis-point share, or the token's floor if that is more.
    function feeFor(bytes10 zts, uint256 amount) public view returns (uint256 fee) {
        fee = amount * feeBps / FEE_DENOMINATOR;
        uint256 floor = zts == ZNN ? minFeeZNN : minFeeQSR;
        if (fee < floor) fee = floor;
    }

    function _record(address to, bytes10 zts, uint256 amount, uint256 fee) internal returns (uint256 id) {
        // Byte 0 of a NoM address is its type; only 0x00 is an account a key can
        // spend from. Anything else would burn the payout on NoM.
        if (to == address(0) || uint160(to) >> 152 != 0) revert NotAUserAddress();
        // Nothing would be left to pay.
        if (amount <= fee) revert BadAmount();
        uint64 net = uint64(amount - fee);
        id = ++withdrawalCount;
        _withdrawals[id] = Withdrawal(msg.sender, to, zts, net, uint64(block.number), bytes32(0), uint64(fee));
        emit WithdrawalInitiated(id, msg.sender, to, zts, net);
        if (fee != 0) emit WithdrawalFee(id, zts, fee);
    }

    /// @notice Derived from a NoM send out of the vault that names withdrawal `id`.
    /// Never reverts: a derived fact is recorded even when it is wrong.
    function markPaid(uint256 id, bytes32 nomBlock, address to, bytes10 zts, uint256 amount) external onlySystem {
        if (payoutSeen[nomBlock]) return;
        payoutSeen[nomBlock] = true;
        Withdrawal storage w = _withdrawals[id];
        if (w.from == address(0)) {
            emit PayoutAnomaly(id, nomBlock, 1);
            return;
        }
        if (w.paidIn != bytes32(0)) {
            emit PayoutAnomaly(id, nomBlock, 2);
            return;
        }
        if (w.to != to || w.zts != zts || w.amount != amount) {
            emit PayoutAnomaly(id, nomBlock, 3);
            return;
        }
        w.paidIn = nomBlock;
        if (zts == ZNN) {
            pendingZNN -= amount;
            paidZNN += amount;
        } else {
            pendingQSR -= amount;
            paidQSR += amount;
        }
        emit WithdrawalPaid(id, nomBlock);
    }

    /// @notice Derived from a QSR send into the vault.
    function depositQSR(bytes32 nomBlock, address to, uint256 amount) external onlySystem {
        depositedQSR += amount;
        WrappedQSR(QSR_TOKEN).bridgeMint(to, amount);
        emit QSRDeposited(nomBlock, to, amount);
    }

    /// @notice Derived from a vault send to NoM's plasma contract. Like a
    /// payout, every movement of the vault's funds is recorded here, so the peg
    /// stays auditable from ZVM state.
    function markFused(bytes32 nomBlock, address beneficiary, uint256 amount) external onlySystem {
        if (payoutSeen[nomBlock]) return;
        payoutSeen[nomBlock] = true;
        fusedQSR += amount;
        emit VaultFused(nomBlock, beneficiary, amount);
    }

    /// @notice Fee revenue in QSR that has not yet been fused.
    function fundableQSR() external view returns (uint256) {
        return feesQSR > fusedQSR ? feesQSR - fusedQSR : 0;
    }

    function withdrawal(uint256 id) external view returns (Withdrawal memory) {
        return _withdrawals[id];
    }
}
