# AGENTS.md

## Project overview

phila-trashday tells Philadelphia residents when to put out trash and recycling. It serves a web page that searches for any address, `/schedule.ics` and `/schedule.json` feeds per address, and saved addresses, each under a random UUID with its own `/a/{id}` page and feeds. One saved address is the default, used when a request doesn't name one. Addresses are looked up in the city's AIS API, the city's holidays are computed from their rules (with an optional file of extra ones), and it runs as a `FROM scratch` Docker container. It's a Go module with no dependencies outside the standard library, and the page is plain HTML, CSS and JavaScript embedded in the binary, with no build step.

## Setup

- Go 1.27 (see `go.mod`). No cgo.
- Network access to `api.phila.gov` for anything beyond unit tests, and to `phl.carto.com` from the browser for address suggestions.

## Build / Run

```sh
HOLIDAYS_FILE=holidays.example.txt ADDRESSES_FILE=/tmp/addresses.json LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/phila-trashday
docker build --build-arg VERSION=dev -t phila-trashday .
```

Then open `http://127.0.0.1:8080/`.

## Test

```sh
go vet ./...
go test -race -cover ./...
```

- `internal/schedule`, `internal/ais`, `internal/ical` and `internal/server` are at 100% statement coverage. Keep them there. `internal/web` is at 100% apart from a panic that can't happen unless its `//go:embed` changes.
- The page's JavaScript has no automated tests. Check changes to it in a browser: search with autocomplete, keyboard selection, the reminder picker, saving an address with a label, making it the default, removing it (two clicks), opening `/a/{id}` and a removed address's link, and an unknown address.
- Lint (`.github/workflows/lint.yml`) runs golangci-lint with `revive`, `errorlint`, `gocritic` and `misspell` (`.golangci.yml`), plus ShellCheck on `scripts/` and actionlint. Locally: `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...`.
- CI builds the Docker image and checks the container serves the page and passes its healthcheck.

## Repository structure

- `cmd/phila-trashday/`: environment config, the HTTP server and the `healthcheck` subcommand.
- `internal/schedule/`: the pickup and holiday rules, the city's holidays computed per year (`city.go`), and the extra holidays file parser. Pure functions, no I/O.
- `internal/ais/`: the AIS address lookup.
- `internal/ical/`: the iCalendar writer (escaping, line folding, alarms).
- `internal/server/`: address lookups and their cache, holiday reloads, warnings, and the schedule handlers in `server.go`; saved addresses, their file and `/api/addresses` in `saved.go`.
- `internal/web/`: the page, embedded from `static/`, with its Content-Security-Policy.

## Conventions

- Dates are `time.Time` at midnight UTC, made with `schedule.Day`, so they compare with `Equal` and work as map keys. Philadelphia's time zone only decides what today is.
- Only saved addresses' `/a/{id}/schedule.ics` feeds are meant to be public, behind a reverse proxy (`PUBLIC_URL`). Their event UIDs use the saved ID, not the address, because calendar services fetch them. Never make the page or `/api/addresses` depend on being public: they have no authentication.
- A saved address's ID is a random v4 UUID from the standard library's `uuid` package, never derived from the address: its URLs have to survive the address changing, and shouldn't reveal it.
- A pickup's ID is its usual date plus its slot. Never derive it from the moved date: subscribers like ReminderSync match events by UID, and a holiday has to look like a move, not a delete and a create.
- Don't fetch anything from phila.gov that sits behind its bot-blocking CDN (`www.phila.gov`, `api.phila.gov/phila/trashday`). AIS and Carto aren't blocked. Holidays are computed in `schedule.CityHolidays`, which is tested against the list the city's trashday endpoint returned for 2026. If the city adds or drops a holiday, change the rules and that test together, checking against the endpoint in a browser.
- The page builds the DOM with `textContent`, never `innerHTML`, since addresses come from the city's records and from visitors.
- The page's design: Overpass (modeled on road-sign lettering) as the only typeface, a curbside palette (sidewalk, asphalt, bin blue, trash-can gray, streetlamp amber, night navy), and one bold element, the hero's answer, which turns to night colors when tonight is set-out night. Keep everything else quiet.
- Doc comments on every exported identifier, comments that explain why, errors wrapped with `%w`, table-driven tests and fakes over mocks.
- Markdown follows markdownlint with MD013 off.

## Commit and PR conventions

- Conventional Commits for commit messages and PR titles, enforced by CI. PRs are squash-merged, so the PR title becomes the commit on `main`.
- Never commit to `main` directly.
- Releases use release-please, and a published release pushes the image to Docker Hub. Don't hand-edit `CHANGELOG.md`, `.release-please-manifest.json` or version numbers.
