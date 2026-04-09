Diagnose fault injection test failures and suggest fixes.

Analyze the most recent test results and provide actionable fix suggestions.

Steps:
1. Find the faultbox spec file (*.star) in this project
2. Run: `faultbox test <file>.star --format json`
3. For each test, analyze the diagnostics array:
   - FAULT_FIRED_BUT_SUCCESS: the fault fired but the test passed — find the service code that handles the faulted syscall and check if errors are properly propagated
   - FAULT_NOT_FIRED: the fault was installed but never triggered — check if the service uses a different syscall variant, suggest using --debug
   - SERVICE_CRASHED: service exited non-zero — look for panic/fatal in the service code
   - TIMEOUT_DURING_FAULT: possible infinite retry — find retry loops and suggest adding timeouts
   - ASSERTION_MISMATCH: expected value doesn't match — check the error handling path
4. For each diagnosis, locate the relevant source code and suggest a specific fix
5. If all tests pass with no diagnostics, congratulate and suggest adding more fault scenarios

Focus on the root cause, not the symptom. Read the actual service code to provide targeted fix suggestions.
