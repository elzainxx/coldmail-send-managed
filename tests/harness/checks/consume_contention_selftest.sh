#!/usr/bin/env bash
# consume_contention_selftest — BLOCKING paired control for consume_contention.sh (FW-M3-22, ADR-0015).
#
# The two tests it controls claim six things between them, and a claim that nothing contradicts is not evidence. This runs the target test against six mutants,
# each of which removes exactly one claim, and requires every one to go RED with its own signature:
#
# TestAccountConsume_NeverExceedsDailyLimit — peers are refused WHILE a holder keeps the last slot's row locked
#   noskip      SKIP LOCKED removed           -> a peer waits on the locked row instead of being refused        (FORCED CONTENTION BROKEN)
#   mutex       consume serialised by a mutex -> no two consumes can overlap, so there is no contention to prove (FORCED CONTENTION BROKEN)
#   noceiling   limit and paused predicates   -> a consume past the daily limit succeeds or hits the CHECK     (OVER-CONSUMPTION / errored once capacity was gone)
#   nogate      the gate trigger never fires  -> the interleave is not forced, and the test must say so         (forced interleave did not engage)
#
# TestAccountConsume_StaleSnapshotPeerIsRefused — a peer whose snapshot PREDATES the holder's commit is refused by the MVCC re-check
#   norecheck   the row lock removed          -> no re-check: only the CHECK constraint stops the double booking (MVCC RE-CHECK PATH FAILED)
#   nopolicy    the gate policy never calls   -> the peer never parks between its snapshot and its lock         (MVCC forced interleave did not engage)
#
# The real code is run by consume_contention.sh (its own CI step), so this script adds only the mutants; each consume_contention.sh --mutant call verifies
# that its mutation LANDED, runs only its target test, and checks that the test failed in the right place and for the right reason, so an unrelated red (a build
# error, a missing database) cannot pass for a control.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."
for m in noskip mutex noceiling nogate norecheck nopolicy; do
  bash tests/harness/checks/consume_contention.sh --mutant "$m"
done
echo "consume_contention_selftest OK: all six mutants (noskip, mutex, noceiling, nogate, norecheck, nopolicy) turn their target test red with their own signature"
