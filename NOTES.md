# Notes

**Setup & Running**
* Started the service using the default command: `docker compose up --build`
* To test fixes, I used the provided shell scripts in the `/scripts` directory.

**Chronological Log (Timebox: ~1.5 hours)**
* **0:00** Cloned the repository and skimmed through the `README.md`.
* **0:06** Started the service via Docker, checked the terminal logs.
* **0:08** Logs looked clean on startup, nothing suspicious. Decided to keep an eye on them and use Gemini as a thought partner for debugging.
* **0:10** Service is up and running. Proceeding to run the provided scripts.
* **0:12** Ran `consistency_check.sh`. Everything matched perfectly (no errors).
* **0:14** Ran `burst_test.sh`. Got HTTP 200s, but running `consistency_check.sh` right after showed the totals didn't match the expected values. Conclusion: Data race.
* **0:19** Ran `leak_test.sh`. Sent 10 failing requests, but only 5 were actually processed. The rest just dropped with a "context deadline exceeded" error.
* **0:20** Threw all terminal logs into Gemini. It didn't point out anything I hadn't already noticed. Diving into the codebase to fix the data race first.
* **0:26** Checked `main.go`, `rules.go`, and `store.go`. At first glance, didn't notice anything super weird. Still wrapping my head around the core logic.
* **0:29** Re-read `rules.go`—basically nothing to break there, just standard data validation. `handlers.go` HTTP routing shouldn't cause issues either. Definitely deciding to look for the bug in `store.go`.
* **0:34** `pgx` itself is thread-safe, and I don't see any obvious mistakes like missing RW or normal mutexes. Decided to throw the code into the AI to help spot data inconsistencies or refactor the logic.
* **0:44** Carefully reviewed Gemini's response and realized I missed a very obvious connection pool error. In `handlers.go`, the `transfer` handler does a bunch of standard validation checks. **Problem:** the handler doesn't release the connection via `defer`. So if an error occurs early on and the function returns, the execution never reaches `conn.Release()`.
* **0:55** **Solution:** Fixed it by simply returning the connection back to the pool via `defer conn.Release()` right after acquiring it. (Honestly, I was surprised the connection bug was in the handler; I expected it to be in `store.go`). Moving on to the next problem.
* **0:58** The next problem is in the `MoveStock` method in `store.go`. Data interaction happens without any thread safety (which surprised me initially—I expected to see logic without transactions, but finding it completely unprotected without even an atomic approach was unexpected). **Problem:** `MoveStock` does a classic in-memory Read-Modify-Write. In a multi-threaded environment, this inevitably leads to concurrent data access where threads overwrite each other.
* **1:09** **Solution:** Fixed this by fully relying on PostgreSQL. Wrapped the operations in a `pgx` transaction (`conn.Begin(ctx)`) and changed the logic to use atomic SQL updates (`quantity = quantity +/- $1`). This guarantees that even if the function fails, data isn't lost and consistency is maintained. Implemented the fix (partially using Gemini's suggested SQL structure, which I considered completely valid).
* **1:15** Reviewed the modified code to ensure no weirdness.
* **1:29** Rebuilt the Docker container and ran all tests again. The leak and data race were gone, BUT during the `burst_test.sh`, I noticed some requests started failing with a 500 status. Checked DB logs: `deadlock detected`. Threw the error into Gemini and realized it happens because of cross-transfers (A -> B and B -> A simultaneously) creating cyclic locks.
* **1:34** **Solution:** Fixed the deadlock by sorting the warehouse locations alphabetically before executing the `UPDATE` statements, ensuring locks are always acquired in a strict, predictable order. Restarted the container, ran the tests again. 100% green. Consider the assignment done.

**Assumptions & Next Steps**
* **Check-Then-Act Vulnerability:** The implemented transactions and atomic updates solve the total mismatch and deadlock problems. However, the database itself lacks a hardcoded `CHECK (quantity >= 0)` constraint on the `inventory` table. Under specific conditions with massive concurrency, multiple parallel transactions could potentially pass the validation in the Go code (`available < req.Quantity`) and drive a warehouse balance into negative values during the atomic `UPDATE`.
* **What I would do with more time:** I would 100% add a `CHECK` constraint directly into the PostgreSQL schema to prevent such cases at the DB level, or implement `SELECT ... FOR UPDATE` during the initial balance check.

**AI Tool Usage Summary**
* Delegated log parsing to Gemini (confirmed my manual findings).
* Used Gemini to scan the initial codebase, which helped spot the missing `defer` in `handlers.go` and the Read-Modify-Write flaw in `store.go`.
* Used Gemini to identify the root cause of the Postgres `deadlock detected` error and applied the suggested sorting algorithm to fix lock inversion.
* Always manually reviewed and verified the AI's suggestions before integrating them into the final code.
  Wrote the original thinking log in my native language to save time and focus purely on debugging within the strict timebox (kept in `NOTES_ORIGINAL_RU.md`), then used AI to translate it into English without altering the technical context.