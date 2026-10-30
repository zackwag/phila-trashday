# phila-trashday

Tells you when to put out trash and recycling in Philadelphia. Search for any address on a small web page, and subscribe to its pickups as a calendar feed that Apple Calendar, Home Assistant or ReminderSync can read. It runs as one container on your LAN.

## What it does

- Looks addresses up in the city's [AIS API](https://api.phila.gov/ais/v1/) and remembers each lookup for 6 hours.
- Works out a year of pickups, moving them for holidays the way phila.gov describes: a weekday holiday pushes that week's primary pickup a day later and cancels the trash-only second pickup.
- Combines trash and recycling on the same day into one event, like "Trash and recycling".
- Adds an alert to every feed event, by default at 8 PM the evening before.
- Saves addresses on the server under random IDs, each with its own page and feed URLs that keep working when the address is renamed or changed. One of them is the default, which every device on the network opens to.

## The page

Open the server in a browser and type an address. Suggestions come from the city's property records ([Carto](https://phl.carto.com), queried from the browser like [opa-ai](https://github.com/zackwag/opa-ai) does). The page shows:

- When to put the bins out, as one big answer: "Tonight." (in streetlamp colors), "Tuesday night." or "Today."
- The next six weeks of pickups, including the ones a holiday moves or skips.
- The holidays that change the schedule.
- The calendar feed link, with a choice of reminder time.
- Saving the address with an optional label, like "Home", making it the default, or removing it. Saved addresses are listed below, and as quick picks under the search box.

A saved address's page is at `/a/{id}`, so it can be bookmarked or shared. The address you're viewing is kept in the browser's local storage, and a browser that has never picked one opens to the default.

## Endpoints

| Path | What |
| --- | --- |
| `/` | The page |
| `/schedule.ics?address=…&alarm=20:00` | iCalendar feed from a week back to a year ahead. `alarm` is `HH:MM` the night before, or `off`, and defaults to `ALARM_TIME` |
| `/schedule.json?address=…` | The same schedule as JSON, plus the pickup days, holidays and any warnings |
| `/a/{id}`, `/a/{id}/schedule.ics`, `/a/{id}/schedule.json` | A saved address's page and feeds |
| `/api/addresses` | `GET` the saved addresses, `POST {"address": "…", "label": "Home"}` to save one |
| `/api/addresses/{id}` | `PATCH {"label": …, "address": …, "default": true}` to change one, `DELETE` to remove it |
| `/healthz` | `200` with `ok`, or `degraded` when the extra holidays file can't be read |

Prefer a saved address's `/a/{id}/schedule.ics` for subscriptions. It keeps working if the address is renamed or moved, and it doesn't reveal the address. Leave out `address` and the default saved address is used. Addresses can be typed any way AIS understands, like `1830 fitzwater street`, and are saved once in the city's form however they're typed. The first saved address becomes the default, and removing the default makes the oldest remaining one the default. Removing an address breaks its links.

Each pickup's ID is the date it's usually on plus its slot, like `2026-11-11-primary`. A holiday changes the pickup's date but not its ID, so subscribers see a moved event rather than a deleted one and a new one.

```json
{
  "id": "2026-11-11-primary",
  "date": "2026-11-12",
  "scheduled_date": "2026-11-11",
  "moved": true,
  "slot": "primary",
  "kinds": ["trash", "recycling"],
  "summary": "Trash and recycling"
}
```

## Holidays

The city's holidays are computed, for every year, so there's nothing to keep up to date. The city publishes its list behind a CDN that blocks anything that isn't a browser, but every holiday on it follows a fixed rule: the federal holidays, plus Good Friday and the day after Thanksgiving.

| Holiday | Rule |
| --- | --- |
| New Year's Day | January 1 |
| Martin Luther King, Jr. Day | Third Monday in January |
| Presidents' Day | Third Monday in February |
| Good Friday | Two days before Easter |
| Memorial Day | Last Monday in May |
| Juneteenth | June 19 |
| Independence Day | July 4 |
| Labor Day | First Monday in September |
| Columbus Day/Indigenous Peoples' Day | Second Monday in October |
| Veterans Day | November 11 |
| Thanksgiving | Fourth Thursday in November |
| Thanksgiving Friday | The day after Thanksgiving |
| Christmas Day | December 25 |

A holiday on a Sunday is observed on Monday, as phila.gov's holidays page says. One on a Saturday stays there and doesn't move pickups, the way the city listed Independence Day in 2026. The computed 2026 list matches the city's exactly, names included.

For a one-off closure the city announces, list it in an optional file, one date per line with an optional name (see [`holidays.example.txt`](holidays.example.txt)). It's added to the computed holidays and re-read every 6 hours, so edits apply without a restart:

```text
2026-12-24 Christmas Eve
```

## Configuration

| Variable | Default | What |
| --- | --- | --- |
| `HOLIDAYS_FILE` | `/config/holidays.txt` | Optional extra holidays, on top of the city's computed ones |
| `ADDRESSES_FILE` | `/data/addresses.json` | Where saved addresses are kept |
| `PUBLIC_URL` | (none) | Where a reverse proxy serves saved addresses' feeds publicly, like `https://example.com/trashday`. The page then offers that link, so iCloud and Skylight can subscribe |
| `ALARM_TIME` | `20:00` | The feed's default reminder the evening before a pickup, as `HH:MM`, or `off` |
| `REFRESH_INTERVAL` | `6h` | How long an address lookup is reused, and how often the extra holidays file is re-read |
| `LISTEN_ADDR` | `:8080` | Where to serve |

## Running it

```yaml
services:
  phila-trashday:
    image: zackwag/phila-trashday:latest
    container_name: phila-trashday
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - phila-trashday-data:/data

volumes:
  phila-trashday-data:
```

To add extra holidays, mount a file at `/config/holidays.txt` too. Make sure the file exists first: if it doesn't, Docker creates an empty directory in its place, and `/healthz` reports the file can't be read.

The image is `FROM scratch` and runs as `nobody` (UID 65534). A named volume at `/data` starts out writable. If you bind-mount a host directory there instead, make it writable by UID 65534 (`chown 65534:65534`), or saving addresses fails with a message saying so.

Anyone who can reach the server can see and change the saved addresses. That's fine on a home LAN; put it behind authentication before exposing it any further. Another website can't change them from a visitor's browser, because changes need a JSON body (or a `DELETE`), which browsers only send cross-site after a CORS preflight that this server never approves.

To subscribe from Apple Calendar, add the feed as an "On My Mac" subscription. An iCloud subscription is fetched by Apple's servers, which can't reach a LAN address.

## Public feeds

iCloud and Skylight fetch subscribed calendars from their own servers, so they can't reach a LAN address. Expose only the saved addresses' feeds through a reverse proxy, never the page or `/api/addresses`, which have no authentication and would let anyone see or change the saved addresses. A feed URL contains a random 122-bit ID, so it can't be guessed, and a saved address's events are named by that ID, so the feed doesn't spell out the address.

With Caddy, inside the public site's block:

```text
handle_path /trashday/* {
	@feed path_regexp ^/a/[0-9a-fA-F-]{36}/schedule\.ics$
	handle @feed {
		reverse_proxy phila-trashday:8080
	}
	respond 404
}
```

Then set `PUBLIC_URL=https://example.com/trashday`. When subscribing in Calendar on a Mac, uncheck **Remove: Alerts** to keep the reminders. It's checked by default. Google Calendar ignores reminders in subscribed feeds.

## Building

```sh
go test -race ./...
docker build --build-arg VERSION=dev -t phila-trashday .
```

Releases are cut by release-please. Publishing a release runs `.github/workflows/release.yml`, which builds `linux/amd64` and `linux/arm64` images and pushes them to Docker Hub as `zackwag/phila-trashday` with the version and `latest` tags. It needs the `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` repository secrets.

## Credit

The holiday rules follow the `phila_gov` source in [Waste Collection Schedule](https://github.com/mampfes/hacs_waste_collection_schedule). One deliberate difference: when a moved pickup lands on a second holiday, it moves one more day rather than recounting every holiday in the week, so a Monday pickup in a week with Monday and Tuesday holidays goes to Wednesday instead of colliding with Tuesday's on Thursday.

The page uses [Overpass](https://github.com/RedHatOfficial/Overpass), under the SIL Open Font License (`internal/web/static/fonts/OFL.txt`).

## License

MIT
