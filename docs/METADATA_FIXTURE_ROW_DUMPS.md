# Metadata fixture row-dump destinations

The test-only `uriHistoryDump` helper reads every application table and column,
serializes each typed row immediately, sorts the row strings, and serializes
the complete table map. It remains the oracle for metadata history, adoption,
backup, restore and process-interruption fixtures.

Each table scan prepares one value slice and its column destination pointers.
Every successful `Rows.Scan` writes all destinations before the immediate
`json.Marshal`. The resulting row string is retained independently before the
next scan. NULL, text and binary values retain the same JSON representation.
The complete query sequence, scan arguments, rows, sorts, error checks and
returned bytes remain required.

This moves the two slice-construction expressions outside the row loop. The
placement alone establishes no runtime allocation count, controlled elapsed
gain or production-capacity claim. Fresh qualification at the candidate source
must retain all original terminal names and outcomes, all three authenticated
complete-table images, normal and selected-race component controls, backup and
process fingerprints, joined crash children, full-client lifetimes and the
complete marketplace matrix. Every original acceptance gate remains active.
