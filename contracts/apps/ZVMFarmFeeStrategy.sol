// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";

interface IUniswapV2Pair {
    function token0() external view returns (address);
    function token1() external view returns (address);
    function getReserves() external view returns (uint112 reserve0, uint112 reserve1, uint32 blockTimestampLast);
    function balanceOf(address owner) external view returns (uint256);
    function transfer(address to, uint256 value) external returns (bool);
    function burn(address to) external returns (uint256 amount0, uint256 amount1);
    function mint(address to) external returns (uint256 liquidity);
}

interface IUniswapV2Factory {
    function feeTo() external view returns (address);
    function getPair(address tokenA, address tokenB) external view returns (address pair);
}

interface IUniswapV2Router {
    function swapExactTokensForTokens(
        uint256 amountIn,
        uint256 amountOutMin,
        address[] calldata path,
        address to,
        uint256 deadline
    ) external returns (uint256[] memory amounts);
}

interface IZVMFarm {
    function fund(uint256 amount) external;
    function rewardToken() external view returns (address);
}

/// @title ZVMFarmFeeStrategy
/// @notice Turns Uniswap V2's protocol fee into farm budget: every swap on every
/// pair pays a slice, the slice is sold for the farm's reward token, and the
/// proceeds extend the farm's emission.
///
/// ## Where the money comes from
///
/// A Uniswap V2 swap charges 0.30%, which normally accrues to the liquidity
/// providers as growth in `k`. When the factory has a `feeTo`, each pair diverts
/// **one sixth of that — 0.05% of volume** — by minting `feeTo` new LP tokens
/// worth the growth in `sqrt(k)` since the last time it looked. Setting `feeTo`
/// to this contract is the whole of the redirection; there is no hook to swap
/// against, and the one-sixth is fixed in the deployed pair's code, so "a part of
/// the swap fees" means exactly 0.05% and cannot be tuned without new pairs.
///
/// ## The part that is not obvious
///
/// **A pair only mints that LP inside `mint` and `burn`.** Swapping alone accrues
/// the fee in the reserves but mints nothing, so a pool whose liquidity never
/// moves hands this contract nothing however much it trades — until something
/// calls `mint` or `burn` on it. `harvest` is therefore written to do that itself:
/// burning the LP it already holds is what makes the pair mint the fee that has
/// accrued since last time. The first round needs a position to burn, which is
/// what `seed` is for; after that each harvest leaves the next one's seed behind,
/// because the LP minted during the burn is minted *to this contract* and is not
/// part of what gets burned.
///
/// One round of latency follows from that, and is deliberate: a harvest realises
/// the fees accrued up to the previous harvest, and arms the next one.
///
/// ## What it can do with the money
///
/// Nothing but give it to the farm. There is no sweep, no owner and no treasury:
/// every path out of this contract ends in `ZVMFarm.fund`, which is itself
/// permissionless and can only pay the tokens out as rewards. A stuck token stays
/// stuck, which is the price of that guarantee and is worth paying — the
/// alternative is a privileged address that can take the fees, which is the thing
/// this contract exists to avoid.
///
/// `fund` adds to the budget without touching `rewardPerSecond`, so fees make the
/// farm last longer rather than pay faster. That is the honest shape: emission is
/// a promise the farm has to keep, and fee revenue is not predictable enough to
/// promise with.
contract ZVMFarmFeeStrategy {
    using SafeERC20 for IERC20;

    IZVMFarm public immutable farm;
    IUniswapV2Router public immutable router;
    IUniswapV2Factory public immutable factory;
    /// The farm's reward token — QSR on the ZVM. Everything is sold for this.
    IERC20 public immutable rewardToken;
    /// Wrapped ZNN, the routing hop for a token with no direct reward pair.
    address public immutable weth;

    event Realised(address indexed pair, uint256 liquidityBurned, uint256 amount0, uint256 amount1);
    event Sold(address indexed token, uint256 amountIn, uint256 rewardOut);
    event Funded(uint256 amount);
    event Seeded(address indexed pair, address indexed from, uint256 liquidity);

    error NotFeeTo();
    error NothingToHarvest();
    error TooLittleReward(uint256 got, uint256 want);
    error NoSeed();

    constructor(IZVMFarm farm_, IUniswapV2Router router_, IUniswapV2Factory factory_, address weth_) {
        farm = farm_;
        router = router_;
        factory = factory_;
        rewardToken = IERC20(farm_.rewardToken());
        weth = weth_;
    }

    /// Realise the protocol fee on each pair, sell it for the reward token, and
    /// hand the proceeds to the farm.
    ///
    /// Permissionless, because it can only move value in one direction. It is
    /// also sandwichable in principle: the sale takes whatever price it finds.
    /// `minRewardOut` is the caller's protection, and the only real one — a
    /// slippage bound checked against the pair's own reserves would be checked
    /// against reserves an attacker has already moved.
    function harvest(address[] calldata pairs, uint256 minRewardOut) external returns (uint256 funded) {
        if (factory.feeTo() != address(this)) revert NotFeeTo();
        for (uint256 i; i < pairs.length; ++i) {
            _realise(IUniswapV2Pair(pairs[i]));
        }
        funded = _fund();
        if (funded < minRewardOut) revert TooLittleReward(funded, minRewardOut);
    }

    /// Give a pair the position `harvest` needs in order to make it mint.
    ///
    /// Anyone may: it is a donation, it is dust, and it never comes back out
    /// except as farm rewards. `amount0` and `amount1` are pulled from the caller
    /// and are in the pair's own token order; they only have to be large enough
    /// that the pair mints a non-zero amount of LP and that burning it later
    /// returns a non-zero amount of *both* tokens — which for a pair of an
    /// 18-decimal and an 8-decimal token is the binding constraint, and is why
    /// the amounts are the caller's to compute rather than a constant here.
    function seed(address pair, uint256 amount0, uint256 amount1) external returns (uint256 liquidity) {
        IUniswapV2Pair p = IUniswapV2Pair(pair);
        IERC20(p.token0()).safeTransferFrom(msg.sender, pair, amount0);
        IERC20(p.token1()).safeTransferFrom(msg.sender, pair, amount1);
        liquidity = p.mint(address(this));
        if (liquidity == 0) revert NoSeed();
        emit Seeded(pair, msg.sender, liquidity);
    }

    /// What `harvest` would realise on this pair right now: the LP it holds, and
    /// the two token amounts burning it would return. It does not include the fee
    /// the burn itself will mint, which is next round's.
    function pending(address pair) external view returns (uint256 liquidity, uint256 amount0, uint256 amount1) {
        IUniswapV2Pair p = IUniswapV2Pair(pair);
        liquidity = p.balanceOf(address(this));
        if (liquidity == 0) return (0, 0, 0);
        (uint112 r0, uint112 r1,) = p.getReserves();
        uint256 supply = IERC20(pair).totalSupply();
        if (supply == 0) return (liquidity, 0, 0);
        amount0 = (liquidity * r0) / supply;
        amount1 = (liquidity * r1) / supply;
    }

    // --- internals --------------------------------------------------------

    /// Burns everything this contract holds of the pair. The burn is what makes
    /// the pair mint the protocol fee accrued since it last looked, and that
    /// mint lands here *after* the burn has taken its snapshot — so it survives
    /// as the seed for the next harvest.
    function _realise(IUniswapV2Pair pair) private {
        uint256 lp = pair.balanceOf(address(this));
        if (lp == 0) return;
        pair.transfer(address(pair), lp);
        (uint256 amount0, uint256 amount1) = pair.burn(address(this));
        emit Realised(address(pair), lp, amount0, amount1);
        _sell(pair.token0());
        _sell(pair.token1());
    }

    /// Sells this contract's whole balance of `token` for the reward token. The
    /// reward token itself needs no sale; wrapped ZNN goes direct; anything else
    /// routes through wrapped ZNN, which is the hop every pair on this chain has.
    function _sell(address token) private {
        if (token == address(rewardToken)) return;
        uint256 amount = IERC20(token).balanceOf(address(this));
        if (amount == 0) return;

        address[] memory path;
        if (token == weth) {
            path = new address[](2);
            path[0] = token;
            path[1] = address(rewardToken);
        } else {
            if (factory.getPair(token, address(rewardToken)) != address(0)) {
                path = new address[](2);
                path[0] = token;
                path[1] = address(rewardToken);
            } else {
                path = new address[](3);
                path[0] = token;
                path[1] = weth;
                path[2] = address(rewardToken);
            }
        }
        // A pair that does not exist would revert the whole harvest, so a token
        // with no route is left alone rather than taking the others down with it.
        for (uint256 i; i + 1 < path.length; ++i) {
            if (factory.getPair(path[i], path[i + 1]) == address(0)) return;
        }

        uint256 before = rewardToken.balanceOf(address(this));
        IERC20(token).forceApprove(address(router), amount);
        router.swapExactTokensForTokens(amount, 0, path, address(this), block.timestamp);
        emit Sold(token, amount, rewardToken.balanceOf(address(this)) - before);
    }

    function _fund() private returns (uint256 amount) {
        amount = rewardToken.balanceOf(address(this));
        if (amount == 0) revert NothingToHarvest();
        rewardToken.forceApprove(address(farm), amount);
        farm.fund(amount);
        emit Funded(amount);
    }
}
