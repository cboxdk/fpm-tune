// Package fpmtune sizes PHP-FPM pools against available memory.
//
// The problem it solves is allocation, not calculation: a machine hosting many
// sites has many pools drawing on one budget, and the useful question is not
// "how many workers should a pool have" but "how should this budget be divided
// between pools that want different amounts".
//
// The packages split along a deliberate line. allocate is pure computation with
// no I/O and no dependencies — given a budget, an inventory and a set of
// baselines it returns a plan, which makes it exhaustively testable and cheap
// for other projects to embed. Everything that touches the world (observe,
// apply, state, budget) sits outside it.
package fpmtune
