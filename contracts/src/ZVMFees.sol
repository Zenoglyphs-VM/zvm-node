// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

/// @title ZVMFees
/// @notice Where a relayed transaction's priority fee goes: half to the account
/// that posted it to NoM, half destroyed.
///
/// An EVM block has one `coinbase`, and one ZVM block carries batches from many
/// relayers, so the block cannot name who to pay. Derivation can: every message
/// records its NoM author. So the payment is an explicit derived transaction —
/// after the user transactions, a system call to this contract carrying the
/// per-author weights computed from that block's own messages. Every node
/// computes the same list from the same momentum; nothing is read from outside
/// the block.
///
/// This contract is the block's `coinbase` from the gas fork's activation
/// height, so the priority fees arrive here as an ordinary balance credit. The
/// base fee is not here to split: EIP-1559 sends it to the base-fee vault
/// (`0x4200…19`), an address with no code and no key, which is a burn already.
///
/// **Relayers are paid by pull, not by push.** A relayer is a NoM account, and
/// a `settle` that pushed value would let one hostile recipient revert the
/// derived transaction the whole chain depends on. `settle` only writes
/// `owed`; the relayer calls `withdraw` when it wants the value — from the
/// native lane if it has no EVM key, which is the usual case.
///
/// **"Burned" here means unspendable on the ZVM.** The backing ZNN is still in
/// the vault on NoM, so the burn is only visible in NoM's own supply once the
/// committee destroys the matching amount there. It does that by sending to the
/// inbox — unspendable by construction — and derivation turns that send into
/// `markBurnedOnNoM`, so ZVM state records how much of its own burn has been
/// matched, and anyone can read the gap.
///
/// Deployed with CREATE2 through the deterministic deployer at
/// `0x4e59b44847b379578588920cA78FbF26c0B4956C`, so its address is a pure
/// function of published values and needs no genesis change.
contract ZVMFees {
    /// @notice Sender of the transactions derivation makes from NoM facts.
    address public constant SYSTEM = 0xDeAddEAddeADdeADdEaDdEaddeadDeADdEAD0002;
    /// @notice Where the burned half goes: no code, no key, the conventional sink.
    address public constant BURN = 0x000000000000000000000000000000000000dEaD;
    /// @notice The relayer's share of what its batch's transactions tipped.
    uint256 public constant RELAYER_SHARE_BP = 5_000;
    /// @notice One NoM base unit (1e-8 ZNN) is 1e10 wei here.
    uint256 public constant ZNN_SCALE = 1e10;
    /// @notice Weights below this share of the pot are left to the burn.
    uint256 public constant DUST = 1e9;

    /// @notice Everything ever split, in wei.
    uint256 public totalCollected;
    /// @notice Credited to relayers and not yet withdrawn.
    uint256 public totalOwed;
    /// @notice Withdrawn by relayers.
    uint256 public totalPaid;
    /// @notice Sent to BURN by this contract.
    uint256 public totalBurned;
    /// @notice Of that burn, how much the committee has destroyed on NoM.
    uint256 public burnedOnNoM;

    mapping(address => uint256) public owed;
    mapping(bytes32 => bool) public mirrored;

    event Settled(uint256 indexed momentum, uint256 pot, uint256 credited, uint256 burned, uint256 relayers);
    event Credited(address indexed relayer, uint256 indexed momentum, uint256 amount);
    event Withdrawn(address indexed relayer, address indexed to, uint256 amount);
    event BurnMirrored(bytes32 indexed nomBlock, uint256 amount, uint256 cumulative);
    /// @param reason 1 the same NoM block twice, 2 more destroyed on NoM than burned here
    event BurnAnomaly(bytes32 indexed nomBlock, uint256 amount, uint8 reason);

    error OnlySystem();
    error NothingOwed();
    error TransferFailed();

    /// @notice Anyone may add to the pot; the coinbase credit needs no call.
    receive() external payable {}

    /// @notice Split what has arrived since the last settle.
    /// @dev Derived, and never allowed to revert on input: a derived
    /// transaction that reverts would drop a whole block's fees on the floor.
    /// Unnamed value stays for the next settle.
    function settle(uint256 momentum, address[] calldata relayers, uint256[] calldata weights) external {
        if (msg.sender != SYSTEM) revert OnlySystem();
        if (relayers.length != weights.length) return;

        uint256 pot = address(this).balance - totalOwed;
        if (pot == 0) return;

        uint256 total;
        for (uint256 i = 0; i < weights.length; ++i) {
            total += weights[i];
        }

        uint256 credited;
        if (total != 0) {
            for (uint256 i = 0; i < relayers.length; ++i) {
                uint256 pay = (pot * weights[i] * RELAYER_SHARE_BP) / (total * 10_000);
                if (pay < DUST) continue;
                owed[relayers[i]] += pay;
                credited += pay;
                emit Credited(relayers[i], momentum, pay);
            }
        }

        uint256 burn = pot - credited;
        totalCollected += pot;
        totalOwed += credited;
        totalBurned += burn;
        emit Settled(momentum, pot, credited, burn, relayers.length);

        // BURN has no code, so this cannot fail; if it somehow did, the value
        // stays here and the next settle counts it again rather than losing it.
        (bool ok,) = BURN.call{value: burn}("");
        if (!ok) {
            totalBurned -= burn;
            totalCollected -= pot;
        }
    }

    /// @notice Take what this account is owed. A NoM account with no EVM key
    /// calls this from the native lane.
    function withdraw(address to) external returns (uint256 amount) {
        amount = owed[msg.sender];
        if (amount == 0) revert NothingOwed();
        owed[msg.sender] = 0;
        totalOwed -= amount;
        totalPaid += amount;
        (bool ok,) = to.call{value: amount}("");
        if (!ok) revert TransferFailed();
        emit Withdrawn(msg.sender, to, amount);
    }

    /// @notice Derived from a vault send to the inbox tagged `ZVMB`: the
    /// committee destroying, on NoM, ZNN this contract has burned here.
    /// Never reverts: a derived fact is recorded even when it is wrong.
    function markBurnedOnNoM(bytes32 nomBlock, uint256 amount) external {
        if (msg.sender != SYSTEM) revert OnlySystem();
        if (mirrored[nomBlock]) {
            emit BurnAnomaly(nomBlock, amount, 1);
            return;
        }
        mirrored[nomBlock] = true;
        burnedOnNoM += amount;
        if (burnedOnNoM > totalBurned) {
            emit BurnAnomaly(nomBlock, amount, 2);
        }
        emit BurnMirrored(nomBlock, amount, burnedOnNoM);
    }

    /// @notice What this contract has burned that NoM's supply does not show
    /// yet, in wei. The committee's next mirroring send is this, rounded down
    /// to a NoM base unit.
    function unmirrored() external view returns (uint256) {
        return totalBurned > burnedOnNoM ? totalBurned - burnedOnNoM : 0;
    }
}
