# LaminarFlow
Linear Clone with some added functionality

Common commands live in [COMMANDS.md](COMMANDS.md). Run the checks with
`./scripts/test.sh` — a bare `go test ./...` skips every database test and
still prints PASS.

Decisions about how the code is structured live in [docs/adr/](docs/adr/) — why something is
the way it is, when the code itself cannot say.

The server serves the built React frontend and the API from one origin. Run
`./scripts/build-frontend.sh` to pull the bundle in from the frontend repo; without it
the server still starts and explains what is missing.
