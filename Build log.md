
This is my first time writing a buildlog and i realized that it may be too long so ill leave a quick summary at the very top.

Summary

What I set out to do: build a small Go and Postgres service where accounts hold balances and money moves between them (open account, transfer, balance, history) behind a thin JSON API. Correctness came first. The goal was to prove the four rules with tests against a real Postgres database, no mocks, since mocks can't test locks or transactions.

Where AI helped: fast scaffolding (schema, exact money parsing, a real-database test harness) exhaustive test cases I wouldn't be able to have written by hand and most usefully proving claims instead of asserting them. It reproduced the float bug, reproduced a deadlock against real Postgres, and broke the code on purpose to show the tests catch it. I also used a second Claude chat as a reviewer of the first one's work.

Where it was confidently wrong:
    1. The locking design in my plan deadlocked. The plan, drafted with AI, said SELECT ... FOR UPDATE. Two competing transfers on real Postgres gave deadlock detected, because the foreign-key check takes a lock that FOR UPDATE conflicts with. FOR NO KEY UPDATE fixed it.
    2. Tests that look right but prove little. With Go's default pool of about 4 database connections, a "20 concurrent transfers" test mostly runs in a queue. And the database's CHECK (balance >= 0) would let a test pass even with the lock removed. The tests now use 50 connections and require clean "insufficient funds" errors. Removing the lock makes them fail; removing the lock and the CHECK lets all 20 transfers overdraw a $100 account.
    3. Dangerous defaults. The test helper would drop the schema of any database it was pointed at, including my dev one. And allowing $0 opening balances, combined with Go silently ignoring unknown JSON fields, meant a typo like opening_balnce would quietly open an empty account. Both are fixed.

What I rejected:
     - Not recording failed transfers. That followed my own plan, but a stale retry could then succeed after money arrived and move money the user was told had failed. Rejections are now recorded, and retries replay the original outcome. I temporarily restored the old behaviour to confirm the new test catches it.
     - The AI committing on its own. It made my first three commits within about 2 minutes. I stopped it and made the rest myself.
     - Loosening the money type so a test would pass. I kept it strict (no negative input) and fixed the test instead.
    
What I'm not comfortable shipping: no authentication, so anyone who can reach the API can move anyone's money. Idempotency keys never expire and aren't scoped per client. A tranfer that overflows the maximum balance is rolledback rather than recorded as rejected. The demo's concurrent step has no strict barrier, so the Go tests are the real proof. History has no pagination. The race detector only runs inside Docker on my machine.


--------------------------------------------

1. First prompt I wanted claude to generate me a rough scaffolding of the project. Including having the tables for the Postgres database covering, accounts, transfers and history. This was drafted from my intial plan in another chat.

what it did:

All amounts are represented through a whole number equaling the number of cents. Storing whole cents avoids floating-point numbers, which can't represent most decimal fractions exactly. Integer arithmetic on cents is always exact.

problems: 

currently in the scope of what I have to do, i think this is fine but for future purposes, if I include other currencies like japanese yen, they do not have decimals. Or some other currencies have 3 decimal places. In this case I would probably have a hashmap with all currencies I want to support and their respective decimal places and then for every operation invloving these decimals I would replace it with a global variable that sets it to the currency of the user. Then for transfers I would reject transfers between different currencies until I can get the proper exchange values down (through a live currency api)

another problem is that I can only add and subtract which is what the assigment wants but if i implement anything like intrest where money grows mulitplicatively then there would be wierd rounding issues that ill need to cover. Or like splitting 10 dollars 3 ways, who would get the remaining cent etc. 

a problem that exists in the scope of what I have to do though is a float returning. I realized that Go stores numbers in binary and something like 0.29 doesnt have a binary representation so it will be stored as 0.28999... and when you mutiply by 100 to convert to cents to use, it would be 28.999 which then takes the floor to 28 cents which loses 1 cent.

 A risk that is in scope is a float sneaking back into the code. When I asked Claude about the cents approach, it flagged that Go stores floating-point numbers in binary, and a value like 0.29 has no exact binary representation, so it gets stored as 0.28999999999999998. Multiplying by 100 to convert to cents gives 28.999999999999996, and converting that to an integer truncates it to 28 cents, so 1 cent disappears with no error. I asked it to prove this, and it ran a small Go program showing 0.29, 1.15 and 4.35 all losing a cent, while round amounts like 10.00 and 5.50 came out fine. That's what makes the bug dangerous: tests that use round numbers would pass.

 currently this should problem should be fine since I do not have mutiplication or division.


2. next few prompts to implemnt the features of having an account and the transfering.

What I asked for: opening an account, getting the balance and history, then transfers. Transfers follow my plan: claim the idempotency key, lock both accounts, check the balance, move the money and write the ledger entries, all in one database transaction.

What it built: Opening an account writes the account and its opening ledger entry in one transaction, so an account's balance always matches the sum of its history. Tests run against a real Postgres database (a separate money_test), emptied before every test. No mocks, since mocks can't test locks or transactions.

Where the AI was wrong:

My plan deadlocked. The plan (drafted with AI in another chat) said to lock accounts with SELECT ... FOR UPDATE. Before writing any transfer code, Claude ran two competing transfers against the real database and got deadlock detected. Inserting the transfer row first makes Postgres put a light lock on both accounts to check the foreign keys, and FOR UPDATE conflicts with it. So two transfers each wait for the other, even when locking the lowest ID first. FOR NO KEY UPDATE fixed it with 0 failures. It still stops two transfers changing the same balance at once, but doesn't conflict with the foreign-key lock.

A concurrency test that isn't really concurrent. Go's database pool only opens about 4 connections by default. With 20 goroutines, most would just wait in line in Go instead of competing inside Postgres, and the test would pass while proving very little. The pool is now set to 50.

A safety net can hide a missing lock. I had a second Claude chat review the work. It pointed out that the database's CHECK (balance >= 0) would still stop overdrafts even with the lock removed, so a test that only checks "the balance isn't negative" would pass without the lock. The tests therefore require every failed transfer to be a clean "insufficient funds" error, not a database constraint error.

The test helper could wipe the wrong database. The same review found that the test setup drops the whole schema of whatever database it's pointed at, including my dev database if I mistyped the URL. It now refuses any database whose name doesn't end in _test.

overall problems I didnt mention before:
    
Claude initially acted without asking. The first three commits were made by Claude, within about 2 minutes, before I told it to stop committing. One commit message also claims the ledger is "append-only", but nothing in the database enforces that, only the code and a test do.

What worked:

My plan required every concurrency test to fail if the lock is removed, so I had Claude temporarily break the code in different ways and check that tests failed:

No lock: all 3 concurrency tests failed.
No lock and no CHECK: all 20 transfers succeeded against an account holding $100, so the balance went negative. Rule 1 actually broken.
FOR UPDATE (my original plan): almost every concurrent transfer failed with a deadlock.
No duplicate-key handling: all 3 idempotency tests failed.

Things i noticed:

I noticed when asking the AI what happends to failed transfers it replied with that they are never recorded. Since it followed the rules set in the md instructions this makes sense however for an actual banking software i feel like i'd need to record failed transactions like insuffcient funds etc. When i asked claude about this, they agreed and gave me the pros and cons for both.

------------------------------------------------ 
Claude generated
Pros of not recording failures
- It's simpler. One transaction, any error rolls back, and there's no status column or "rejected" state to handle.
- The transfers table only holds transfers that actually moved money.
- A retry after money arrives just works, which can be what the user wants if the failure was temporary.

Cons
- A stale retry can move money the user was told failed. For example:
  a. A request fails with insufficient funds,
  b. An automatic retry of that same request is still in flight, or queued.
  c. Money arrives in the account.
  d. The retry lands, succeeds, and $50 leaves an account whose owner thinks the transfer was rejected.

  "Networks retry" is the exact scenario the brief warns about.
- The same key gives different answers over ted key should "return the original result", and right now it doesn't for failures.
- There's no record of rejected attempts. Reaor auditing and fraud checks.
---------------------------------------------------

When testing this, the reports show that retry: replayed=false err=<nil> meaning the retry succeeded and moved the money despite the transfer "failing". The new implmenetation spends a rejected key for good. Then to try again, the client must send a new key. A cost of having this may be the table filling up with failed transfers making rows that wouldnt normally exist. (storage cost).


3. HTTP API and demo

I asked claude to create a json api for the bank code as well as a small demo showing simple transfers failing etc. 

it built four enpoints (open account, get balance, get history, transfer). Insufficinet funds returns 422, with the rejected transfer in the body. History shows the transfer of who the money came from or went to. It also created a situation which tests most cases however the demo's concurrent step isn't a strict proof (no barrier), so I had Claude add the real Go concurrency test as the demo's final step.

While reading the code I noticed the maximum balance is about $92 quadrillion. A transfer that would push a balance past that is rolled back with a clean 422 error, and the all-or-nothing test (rule 2) uses exactly this case to prove a failure mid-transfer undoes the debit. The one gap is that it isn't recorded as rejected the way insufficient funds is (fast addition).