// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";
import {Math} from "@openzeppelin/contracts/utils/math/Math.sol";
import {SafeCast} from "@openzeppelin/contracts/utils/math/SafeCast.sol";

/// @title ZVMFarm
/// @notice Liquidity mining that can only pay out what it was given.
///
/// The accounting is SushiSwap's MiniChefV2, the farm Sushi runs on chains where
/// it cannot mint: pools weighted by allocation points, one reward rate per
/// second, and a signed reward debt so a withdrawal need not harvest. The budget
/// is PancakeSwap's SmartChef rule: rewards stop at `endTime`, and funding moves
/// `endTime`. Nothing on the ZVM is minted from nothing — every ZNN and QSR came
/// through the vault — so a farm that promised more than it held would fail its
/// last harvests instead.
///
/// Every rounding goes against the position, never against the farm: reward debt
/// rounds up on deposit and down on withdrawal, so the sum of all harvests is at
/// most what was emitted, and emission is at most what was funded.
contract ZVMFarm is Ownable, ReentrancyGuard {
    using SafeERC20 for IERC20;

    struct Pool {
        IERC20 stakeToken;
        uint64 allocPoint;
        uint64 lastRewardTime;
        uint256 accRewardPerShare;
        uint256 totalStaked;
    }

    struct Position {
        uint256 amount;
        int256 rewardDebt;
    }

    /// MiniChef scales by 1e12. A reward token with 8 decimals, like QSR, spread
    /// over an LP supply near 1e24 rounds to nothing every second at that scale;
    /// mulDiv keeps 1e36 from overflowing.
    uint256 private constant ACC_PRECISION = 1e36;
    uint256 public constant MAX_POOLS = 64;

    IERC20 public immutable rewardToken;
    uint256 public rewardPerSecond;
    uint256 public endTime;
    uint256 public totalAllocPoint;
    uint256 public totalFunded;
    /// Budget that does not divide evenly by the rate, carried to the next schedule.
    uint256 public unscheduled;

    Pool[] private _pools;
    mapping(uint256 => mapping(address => Position)) public positions;
    mapping(address => bool) public isStakeToken;

    event PoolAdded(uint256 indexed pid, address indexed stakeToken, uint256 allocPoint);
    event PoolSet(uint256 indexed pid, uint256 allocPoint);
    event RewardRateSet(uint256 rewardPerSecond, uint256 endTime);
    event Funded(address indexed from, uint256 amount, uint256 endTime);
    event Deposit(address indexed user, uint256 indexed pid, uint256 amount, address indexed to);
    event Withdraw(address indexed user, uint256 indexed pid, uint256 amount, address indexed to);
    event EmergencyWithdraw(address indexed user, uint256 indexed pid, uint256 amount, address indexed to);
    event Harvest(address indexed user, uint256 indexed pid, uint256 amount, address to);

    error DuplicatePool();
    error TooManyPools();
    error UnknownPool();
    error ZeroRate();
    error ZeroAmount();
    error InsufficientStake();

    constructor(IERC20 rewardToken_, uint256 rewardPerSecond_, address owner_) Ownable(owner_) {
        if (rewardPerSecond_ == 0) revert ZeroRate();
        rewardToken = rewardToken_;
        rewardPerSecond = rewardPerSecond_;
        endTime = block.timestamp;
        emit RewardRateSet(rewardPerSecond_, block.timestamp);
    }

    // --- views ------------------------------------------------------------

    function poolLength() external view returns (uint256) {
        return _pools.length;
    }

    function poolInfo(uint256 pid) external view returns (Pool memory) {
        return _pool(pid);
    }

    function getPools() external view returns (Pool[] memory) {
        return _pools;
    }

    function pendingReward(uint256 pid, address user) public view returns (uint256) {
        Pool memory pool = _pool(pid);
        return _pending(positions[pid][user], pool.accRewardPerShare + _accrued(pool));
    }

    /// Every pool's stake and pending reward for one account, in one call.
    function getPositions(address user) external view returns (uint256[] memory staked, uint256[] memory pending) {
        uint256 n = _pools.length;
        staked = new uint256[](n);
        pending = new uint256[](n);
        for (uint256 i; i < n; ++i) {
            staked[i] = positions[i][user].amount;
            pending[i] = pendingReward(i, user);
        }
    }

    /// What the farm can still emit: the scheduled remainder plus the carry.
    function remainingRewards() external view returns (uint256) {
        return _scheduledRemainder() + unscheduled;
    }

    // --- owner ------------------------------------------------------------

    function addPool(IERC20 stakeToken, uint64 allocPoint) external onlyOwner returns (uint256 pid) {
        if (isStakeToken[address(stakeToken)]) revert DuplicatePool();
        if (_pools.length >= MAX_POOLS) revert TooManyPools();
        massUpdatePools();
        isStakeToken[address(stakeToken)] = true;
        totalAllocPoint += allocPoint;
        pid = _pools.length;
        _pools.push(
            Pool({
                stakeToken: stakeToken,
                allocPoint: allocPoint,
                lastRewardTime: uint64(block.timestamp),
                accRewardPerShare: 0,
                totalStaked: 0
            })
        );
        emit PoolAdded(pid, address(stakeToken), allocPoint);
    }

    function setPool(uint256 pid, uint64 allocPoint) external onlyOwner {
        _pool(pid);
        massUpdatePools();
        totalAllocPoint = totalAllocPoint - _pools[pid].allocPoint + allocPoint;
        _pools[pid].allocPoint = allocPoint;
        emit PoolSet(pid, allocPoint);
    }

    /// Changes the rate, not the budget: what is left is spread at the new rate,
    /// so the end moves.
    function setRewardPerSecond(uint256 rate) external onlyOwner {
        if (rate == 0) revert ZeroRate();
        massUpdatePools();
        _schedule(rate, _scheduledRemainder() + unscheduled);
    }

    /// Anyone may fund. The tokens only ever leave as rewards.
    function fund(uint256 amount) external nonReentrant {
        if (amount == 0) revert ZeroAmount();
        massUpdatePools();
        uint256 before = rewardToken.balanceOf(address(this));
        rewardToken.safeTransferFrom(msg.sender, address(this), amount);
        uint256 received = rewardToken.balanceOf(address(this)) - before;
        totalFunded += received;
        _schedule(rewardPerSecond, _scheduledRemainder() + unscheduled + received);
        emit Funded(msg.sender, received, endTime);
    }

    // --- positions --------------------------------------------------------

    function deposit(uint256 pid, uint256 amount, address to) external nonReentrant {
        if (amount == 0) revert ZeroAmount();
        _pool(pid);
        Pool storage pool = _update(pid);
        // Measured, not assumed: a token that takes a fee on transfer credits less.
        uint256 before = pool.stakeToken.balanceOf(address(this));
        pool.stakeToken.safeTransferFrom(msg.sender, address(this), amount);
        uint256 received = pool.stakeToken.balanceOf(address(this)) - before;
        Position storage pos = positions[pid][to];
        pos.amount += received;
        pos.rewardDebt += SafeCast.toInt256(Math.mulDiv(received, pool.accRewardPerShare, ACC_PRECISION, Math.Rounding.Ceil));
        pool.totalStaked += received;
        emit Deposit(msg.sender, pid, received, to);
    }

    function withdraw(uint256 pid, uint256 amount, address to) external nonReentrant {
        _pool(pid);
        _withdraw(_update(pid), pid, amount, to);
    }

    function harvest(uint256 pid, address to) external nonReentrant returns (uint256) {
        _pool(pid);
        return _harvest(_update(pid), pid, to);
    }

    function harvestMany(uint256[] calldata pids, address to) external nonReentrant returns (uint256 paid) {
        for (uint256 i; i < pids.length; ++i) {
            _pool(pids[i]);
            paid += _harvest(_update(pids[i]), pids[i], to);
        }
    }

    function withdrawAndHarvest(uint256 pid, uint256 amount, address to) external nonReentrant {
        _pool(pid);
        Pool storage pool = _update(pid);
        _harvest(pool, pid, to);
        _withdraw(pool, pid, amount, to);
    }

    /// Takes the stake back without the rewards, and without touching the
    /// reward accounting — the way out if that accounting ever reverts.
    function emergencyWithdraw(uint256 pid, address to) external nonReentrant {
        Pool memory pool = _pool(pid);
        Position storage pos = positions[pid][msg.sender];
        uint256 amount = pos.amount;
        pos.amount = 0;
        pos.rewardDebt = 0;
        _pools[pid].totalStaked -= amount;
        pool.stakeToken.safeTransfer(to, amount);
        emit EmergencyWithdraw(msg.sender, pid, amount, to);
    }

    function massUpdatePools() public {
        uint256 n = _pools.length;
        for (uint256 i; i < n; ++i) {
            _update(i);
        }
    }

    // --- internals --------------------------------------------------------

    function _pool(uint256 pid) private view returns (Pool memory) {
        if (pid >= _pools.length) revert UnknownPool();
        return _pools[pid];
    }

    function _scheduledRemainder() private view returns (uint256) {
        return endTime > block.timestamp ? (endTime - block.timestamp) * rewardPerSecond : 0;
    }

    /// Reward per share accrued since the pool was last updated.
    function _accrued(Pool memory pool) private view returns (uint256) {
        uint256 t = block.timestamp < endTime ? block.timestamp : endTime;
        if (t <= pool.lastRewardTime || pool.totalStaked == 0 || totalAllocPoint == 0) return 0;
        uint256 reward = ((t - pool.lastRewardTime) * rewardPerSecond * pool.allocPoint) / totalAllocPoint;
        return Math.mulDiv(reward, ACC_PRECISION, pool.totalStaked);
    }

    function _pending(Position memory pos, uint256 acc) private pure returns (uint256) {
        int256 owed = SafeCast.toInt256(Math.mulDiv(pos.amount, acc, ACC_PRECISION)) - pos.rewardDebt;
        return owed > 0 ? uint256(owed) : 0;
    }

    function _update(uint256 pid) private returns (Pool storage pool) {
        pool = _pools[pid];
        uint256 t = block.timestamp < endTime ? block.timestamp : endTime;
        if (t <= pool.lastRewardTime) return pool;
        if (pool.totalStaked != 0 && totalAllocPoint != 0) {
            uint256 reward = ((t - pool.lastRewardTime) * rewardPerSecond * pool.allocPoint) / totalAllocPoint;
            pool.accRewardPerShare += Math.mulDiv(reward, ACC_PRECISION, pool.totalStaked);
        }
        pool.lastRewardTime = uint64(t);
    }

    /// Spreads `budget` from now at `rate`. Pools are updated before this is
    /// called, so moving every pool's clock to now only skips time the farm had
    /// no budget for.
    function _schedule(uint256 rate, uint256 budget) private {
        uint256 n = _pools.length;
        for (uint256 i; i < n; ++i) {
            if (_pools[i].lastRewardTime < block.timestamp) _pools[i].lastRewardTime = uint64(block.timestamp);
        }
        rewardPerSecond = rate;
        endTime = block.timestamp + budget / rate;
        unscheduled = budget % rate;
        emit RewardRateSet(rate, endTime);
    }

    function _withdraw(Pool storage pool, uint256 pid, uint256 amount, address to) private {
        if (amount == 0) revert ZeroAmount();
        Position storage pos = positions[pid][msg.sender];
        if (amount > pos.amount) revert InsufficientStake();
        pos.rewardDebt -= SafeCast.toInt256(Math.mulDiv(amount, pool.accRewardPerShare, ACC_PRECISION));
        pos.amount -= amount;
        pool.totalStaked -= amount;
        pool.stakeToken.safeTransfer(to, amount);
        emit Withdraw(msg.sender, pid, amount, to);
    }

    function _harvest(Pool storage pool, uint256 pid, address to) private returns (uint256 paid) {
        Position storage pos = positions[pid][msg.sender];
        int256 accumulated = SafeCast.toInt256(Math.mulDiv(pos.amount, pool.accRewardPerShare, ACC_PRECISION));
        int256 owed = accumulated - pos.rewardDebt;
        if (owed <= 0) return 0;
        pos.rewardDebt = accumulated;
        paid = uint256(owed);
        rewardToken.safeTransfer(to, paid);
        emit Harvest(msg.sender, pid, paid, to);
    }
}
