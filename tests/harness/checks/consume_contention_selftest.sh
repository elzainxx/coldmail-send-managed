#!/usr/bin/env bash
# consume_contention_selftest — BLOCKING paired control for consume_contention.sh (FW-M3-22).
#
# The test it controls (TestAccountConsume_NeverExceedsDailyLimit) claims four things, and a claim that nothing contradicts is not evidence.
# This runs the SAME test against four mutants, each of which removes exactly one of them, and requires every one to go RED with its own signature:
#
#   noskip     SKIP LOCKED removed           -> a peer waits on the locked row instead of being refused        (FORCED CONTENTION BROKEN)
#   mutex      consume serialised by a mutex -> no two consumes can overlap, so there is no contention to prove (FORCED CONTENTION BROKEN)
#   noceiling  limit and paused predicates   -> a consume past the daily limit succeeds or hits the CHECK     (OVER-CONSUMPTION / errored once capacity was gone)
#   nogate     the gate trigger never fires  -> the interleave is not forced, and the test must say so         (forced interleave did not engage)
#
# The real code is run by consume_contention.sh (its own CI step), so this script adds only the mutants; each consume_contention.sh --mutant call verifies
# that its mutation LANDED and that the test failed in the right place and for the right reason, so an unrelated red (a build error, a missing database)
# cannot pass for a control.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."
for m in noskip mutex noceiling nogate; do
  bash tests/harness/checks/consume_contention.sh --mutant "$m"
done
echo "consume_contention_selftest OK: all four mutants (noskip, mutex, noceiling, nogate) turn TestAccountConsume_NeverExceedsDailyLimit red with their own signature"
