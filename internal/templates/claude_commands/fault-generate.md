Generate a Faultbox spec for this project.

Analyze the project and generate a fault injection test spec.

Steps:
1. Check if docker-compose.yml (or docker-compose.yaml, compose.yml) exists:
   - If yes: run `faultbox init --from-compose <file>` and save as faultbox.star
   - If no: look for a Makefile, go.mod, or package.json to identify the service, then run `faultbox init --name <service> --port <port> --protocol <proto> <binary>`
2. Show the generated spec to the user
3. If the spec has TODO items, fill them in based on the project's API endpoints and data flow
4. Suggest running `/fault-test` to verify the spec works

For existing specs, run `faultbox generate <file>.star` to auto-generate failure scenarios from registered happy-path scenarios.
