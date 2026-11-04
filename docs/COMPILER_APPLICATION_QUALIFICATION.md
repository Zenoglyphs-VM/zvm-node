# Complete application compiler workloads

The dedicated `TestCompilerApplicationContainerProfile` compiles the complete
application and benchmark artifact inventory consumed by `apps.LoadArtifacts`.
It adds a fresh isolated runtime after the original controlled and small vendor
profiles. Those original profiles, their commands and byte limits remain intact.
All three runtimes reuse one production binary, harness, JSON converter and
pinned Go image. The three vendor ELF files are downloaded once and reused as
explicit read-only inputs.

Five groups use the original source and compiler recipes:

| Group | Compiler | Source units | Compiler targets | Tracked exports |
| --- | --- | ---: | ---: | ---: |
| Applications | 0.8.30 | 44 | 44 | 44 |
| Benchmark tokens and helpers | 0.8.30 | 28 | 34 | 34 |
| WZNN | 0.8.30 | 1 | 1 | 1 |
| Uniswap core | 0.5.16 | 12 | 12 | 2 |
| Uniswap periphery | 0.6.6 | 27 | 27 | 1 |

All 122 target identities must be present, with no missing or extra target.
The 82 tracked export records retain exact ABI and creation-byte comparisons.
All 81 provided runtime templates must match completely, including metadata.
The original WZNN export omits its runtime template; this profile does not invent
that comparison. Both legacy groups additionally retain all 39 complete EVM,
opcode, source-map, reference-map, metadata and storage-layout lock comparisons.
No metadata, immutable or executable bytes are masked.

The final native ABI and creation bytes are also compared with the actual Go
artifact loader across all 63 final names. Its intentional shared-name overwrites follow the original
benchmark-then-application order. Per-file export records are checked separately.
Constructor initialization, deployed immutables and a real release genesis need
their independent evidence.

Source preparation accepts only five exact npm source archives, checking byte
counts, SHA-256, SHA-512 integrity and package identity. It performs no package
installation or lifecycle execution and extracts no source tree. Archives are
read with finite decompression/member budgets. Links, special members, traversal,
aliases, extensions, dirty padding and trailing data are refused. A failed
transport retains its capped prefix in the new job-owned scratch directory and
reports incomplete evidence. No retry, redirect or alternate host is accepted.
The original legacy wire-input digests remain verified; canonical transport has
identical source and settings values and its own exact digest.

The runtime has the existing 512-MiB memory ceiling, zero swap, two CPUs, 64-task
ceiling, 64-MiB tmpfs, UID/GID 10001, read-only root, no capabilities,
no-new-privileges and no external network. Only the three verified compiler
files, the existing binaries and one literal input bundle are mounted read-only.
The service uses the production 16-MiB input, 64-MiB output, 1-MiB stderr and
two-minute compiler defaults. Two workers are configured; the five complete
compiles are serialized for exact output accounting. This is not qualification
of simultaneous real-application capacity.

The original production RemoteRunner, request handling and compiler cache are
used. The cache remains three files and 48 MiB, with one release-list read and
zero runtime binary downloads. Service stop/join, unchanged synthetic host
canaries and zero new OOM events are required. Zero OOM counters do not measure
peak RSS or prove a production workload budget.

Every changed source requires fresh exact-source hosted CI, including the
unchanged native, compiler, full-client, SQL and complete financial/wallet gates.
Independent compiler authority, broader platforms, default-scale operator and
cache ownership, per-invocation containment and parent crashes, accepted release
and genesis bindings, real wallets, independent NoM finality and production
acceptance remain open.

The CI producer finalizes the public literal input bundle as mode 0444 through
its owned file descriptor, so the declared runtime UID can read it without write
permission. The job-owned parent directory remains private and the raw archive
files remain host-only. This source correction does not change or qualify the
original browser navigation deadline or its transport failure cause.

The application group has 48 compiler targets and 44 saved exports. Four local
interfaces are intentionally absent from the saved export file; they remain
required compiler targets. Their ABI, empty executable material, layout, metadata
and reference maps are pinned by the original application build lock. The same
literal source hashes and compiler settings are checked before importing that
historical material. All 82 saved exports and 81 provided runtime templates retain
their original comparisons; the original WZNN export still supplies no runtime.
All 83 modern targets additionally require their locked layout, metadata and
immutable/link-reference maps. The original 39 legacy full-material records,
63 final Go loader bindings, runtime limits and six CI gates remain required.
Neither failed source workflow is retried or reclassified by this correction.
