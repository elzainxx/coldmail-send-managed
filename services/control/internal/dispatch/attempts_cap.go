// attempts_cap.go — the SQL predicate behind the attempt cap (send_managed_0028, M2-G2).
//
// THE GAP IT CLOSES. Before this, exactly ONE writer produced the terminal state `failed` — pool
// exhaustion (exhaustion.go) — and every SMTP failure that was not a hard recipient bounce returned the
// job to `queued` with UNBOUNDED retries. attempt_count only saturated (it stops at max_attempts = 3, to
// keep jobs_attempt_count_check honest), so a persistent 554, a revoked credential or a policy block
// never became terminal until the whole pool ran dry. The platform cannot settle an item that never ends.
//
// WHAT COUNTS. A "failed attempt" is a REAL failed SMTP attempt: a worker-reported classified failure
// (transport_4xx / auth_failure / throttle / timeout / unknown — outcome.go's applyClassifiedFailure) or
// the reclaim of a job that was SENDING (a lease or heartbeat timeout — reclaim.go). It is NEVER a
// dispatcher-side requeue that did not reach SMTP: no_eligible_account, no_worker_available, push_failed
// and header_injection all go through dispatch.go's requeue(), which does not touch the counters, and
// the reclaim of a job that was only ASSIGNED (a Control crash between the claim and the push) is not
// counted either. Those mean "waiting", and a job that is waiting must not be failed for it — which is why
// the counters are NEW columns rather than attempt_count, which every one of those paths bumps.
//
// WHAT HAPPENS AT THE CAP. In the SAME guarded UPDATE that would have returned the job to `queued`, the
// state becomes `failed` instead, with completed_at set, the lease cleared and terminal_reason
// 'attempts_exhausted'; the job_events trigger writes the sending -> failed event, carrying the failure
// class and message. It is one statement and one state-guarded WHERE (state='sending' / in-flight, and the
// attempt-token guard outcome.go already applies), exactly like every other transition out of flight: a
// stale or duplicate report that matches no row changes nothing, so the cap cannot fire twice and cannot
// fire on a job somebody else already resolved. The message never reached the world, so the platform
// does not charge for it.
//
// TWO DIALS, ONE PREDICATE. job_max_failed_attempts (default 6) fires when the failed attempt being
// recorded is the Nth; job_failed_max_age_sec (default 86400) fires when it is recorded for a job whose
// FIRST failed attempt is at least that old. Both are evaluated at the moment a failed attempt is
// recorded, not by a sweep: a job that is merely WAITING for capacity is never failed by age, because
// nothing is being attempted and nothing is failing. Both are read fresh from send_managed.config on
// every report and every reclaim tick (LoadConfig), so a change applies to the next failed attempt.
//
// THE EXPRESSION IS EVALUATED AGAINST THE ROW AS IT IS BEFORE THE UPDATE. In an UPDATE's SET list every
// right-hand side sees the OLD row, so `failed_attempt_count + 1` below is "the count this attempt makes",
// and `first_failed_at` is the first failure BEFORE this one (NULL on the first failure: the age test is
// then false, correctly — a job cannot be older than its first failure at the moment of its first failure).
package dispatch

import "fmt"

// attemptsExhaustedSQL returns a SQL boolean expression over the jobs row being updated: true when THIS
// update is a failed attempt AND it reaches either cap.
//
//	counts   — a SQL boolean for "this transition is a real failed SMTP attempt". `TRUE` for the outcome
//	           handler (it only runs for a worker-reported failure of a sending job); `state = 'sending'`
//	           for the reclaim, which also reclaims assigned jobs that never reached a worker.
//	capParam — the 1-based positional parameter carrying job_max_failed_attempts.
//	ageParam — the 1-based positional parameter carrying job_failed_max_age_sec.
//
// The expression is a pure function of its arguments and the row, and it is built once per statement by
// the two writers that need it, so the cap has exactly one definition.
func attemptsExhaustedSQL(counts string, capParam, ageParam int) string {
	return fmt.Sprintf(
		`((%[1]s) AND (failed_attempt_count + 1 >= $%[2]d `+
			`OR (first_failed_at IS NOT NULL AND NOW() - first_failed_at >= make_interval(secs => $%[3]d))))`,
		counts, capParam, ageParam)
}
