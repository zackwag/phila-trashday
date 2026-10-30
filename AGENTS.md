# AGENTS.md

## Project overview

phila-trashday tells Philadelphia residents when to put out trash and recycling. It serves a web page that searches for any address, `/schedule.ics` and `/schedule.json` feeds per address, and a saved home address used when a request doesn't name one. Addresses are looked up in the city's AIS API, holidays come from a local file, and it runs as a `FROM scratch` Docker container. It's a Go module with no dependencies outside the standard library, and the page is plain HTML, CSS and JavaScript embedded in the binary, with no build step.

## Setup

- Go 1.27 (see `go.mod`). No cgo.
- Network access to `api.phila.gov` for anything beyond unit tests, and to `phl.carto.com` from the browser for address suggestions.

## Build / Run

```sh
HOLIDAYS_FILE=holidays.example.txt ADDRESS_FILE=/tmp/address.txt LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/phila-trashday
docker build --build-arg VERSION=dev -t phila-trashday .
```

Then open `http://127.0.0.1:8080/`.

## Test

```sh
go vet ./...
go test -race -cover ./...
```

- `internal/schedule`, `internal/ais`, `internal/ical` and `internal/server` are at 100% statement coverage. Keep them there. `internal/web` is at 100% apart from a panic that can't happen unless its `//go:embed` changes.
- The page's JavaScript has no automated tests. Check changes to it in a browser: search with autocomplete, keyboard selection, the reminder picker, saving and forgetting the home address, and an unknown address.
- Lint (`.github/workflows/lint.yml`) runs golangci-lint with `revive`, `errorlint`, `gocritic` and `misspell` (`.golangci.yml`), plus ShellCheck on `scripts/` and actionlint. Locally: `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...`.
- CI builds the Docker image and checks the container serves the page and passes its healthcheck.

## Repository structure

- `cmd/phila-trashday/`: environment config, the HTTP server and the `healthcheck` subcommand.
- `internal/schedule/`: the pickup and holiday rules, and the holidays file parser. Pure functions, no I/O.
- `internal/ais/`: the AIS address lookup.
- `internal/ical/`: the iCalendar writer (escaping, line folding, alarms).
- `internal/server/`: address lookups and their cache, the home address file, holiday reloads, warnings, and the API handlers.
- `internal/web/`: the page, embedded from `static/`, with its Content-Security-Policy.

## Conventions

- Dates are `time.Time` at midnight UTC, made with `schedule.Day`, so they compare with `Equal` and work as map keys. Philadelphia's time zone only decides what today is.
- A pickup's ID is its usual date plus its slot. Never derive it from the moved date: subscribers like ReminderSync match events by UID, and a holiday has to look like a move, not a delete and a create.
- Don't fetch anything from phila.gov that sits behind its bot-blocking CDN (`www.phila.gov`, `api.phila.gov/phila/trashday`). AIS and Carto aren't blocked. Holidays come from the file.
- The page builds the DOM with `textContent`, never `innerHTML`, since addresses come from the city's records and from visitors.
- The page's design: Overpass (modeled on road-sign lettering) as the only typeface, a curbside palette (sidewalk, asphalt, bin blue, trash-can gray, streetlamp amber, night navy), and one bold element, the hero's answer, which turns to night colors when tonight is set-out night. Keep everything else quiet.
- Doc comments on every exported identifier, comments that explain why, errors wrapped with `%w`, table-driven tests and fakes over mocks.
- Markdown follows markdownlint with MD013 off.

## Commit and PR conventions

- Conventional Commits for commit messages and PR titles, enforced by CI. PRs are squash-merged, so the PR title becomes the commit on `main`.
- Never commit to `main` directly.
- Releases use release-please, and a published release pushes the image to Docker Hub. Don't hand-edit `CHANGELOG.md`, `.release-please-manifest.json` or version numbers.
