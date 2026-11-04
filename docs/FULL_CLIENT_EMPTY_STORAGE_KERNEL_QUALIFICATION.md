# Empty MessagePasser storage under whole-client SIGKILL

The additional Linux profile uses the original MessagePasser code and empty
account storage. It retains the original seeded profiles and their exact
parent-declaration, PID, phase, signal, output and join validators.

One cold reference is shared across five sequential cases:

- Derivation write before commit.
- NFT index write before commit.
- Order-book write before commit.
- Derivation rewind before commit.
- Index rewind before commit.

Each case primes the real default-persistence client and production Node CLI,
starts a second exact child at the declared live write boundary, sends that
child SIGKILL from its owning parent and verifies the actual signal exit and
bounded join. A third child performs production recovery. The parent opens
the execution client only after all three children have joined, solely for the
original complete read-only chain/business/SQL oracle. The forward cases use
the complete cold-image oracle; the rewind cases retain the original strict
source/order-observation oracle and its intentional history differences.

Every owned child checks its running genesis hash, original MessagePasser code
and actual empty storage root. The parent independently requires the exact
context record from each joined child. Genesis allocation, Engine API replies,
original signed inputs and canonical rows are not rewritten by the fixture.
Only the previously declared write hook is removed after the killed child
joins. Existing seeded helpers still require their original slot.

The six additional test events require 16 joined CLI lifetimes, 21 default
service lifetimes and five actual whole-client SIGKILL joins. They reuse the
same full-client test binary; no additional build or downloaded dependency is
introduced. The existing 26-minute job, 7-minute profile, default cache/path
persistence and original child/marker deadlines remain. All original tests
and the complete 52-profile financial/wallet matrix remain required at the
current source.

This is a bounded owned-process experiment with a synthetic NoM observation
source. It does not establish independent NoM authority or finality, complete
release-genesis/constructor provenance, real-wallet or operator capacity,
hardware power-loss durability, broader platforms or production acceptance.
