Run fault injection tests on the current project.

Find the faultbox spec file (*.star) in this project and run all tests using `faultbox test`. Report results clearly.

Steps:
1. Find *.star files in the project root
2. Run: `faultbox test <file>.star --format json`
3. Parse the JSON output and report:
   - Number of passed/failed tests
   - For each failed test: name, reason, and replay command
   - Any diagnostics (warnings/suggestions)
4. If no .star file exists, suggest running `faultbox init --from-compose` if docker-compose.yml exists, or `faultbox init --claude` to set up from scratch

If tests fail, analyze the diagnostics and suggest specific code fixes based on the failure type.
