# phila-trashday

Tells you when to put out trash and recycling in Philadelphia. Search for any address on a small web page, and subscribe to its pickups as a calendar feed that Apple Calendar, Home Assistant or ReminderSync can read. It runs as one container on your LAN.

## What it does

- Looks addresses up in the city's [AIS API](https://api.phila.gov/ais/v1/) and remembers each lookup for 6 hours.
- Works out a year of pickups, moving them for holidays the way phila.gov describes: a weekday holiday pushes that week's primary pickup a day later and cancels the trash-only second pickup.
- Combines trash and recycling on the same day into one event, like "Trash and recycling".
- Adds an alert to every feed event, by default at 8 PM the evening before.
- Saves one home address on the server, so every device on the network opens to it and its feed URL never has to change.

## The page

Open the server in a browser and type an address. Suggestions come from the city's property records ([Carto](https://phl.carto.com), queried from the browser like [opa-ai](https://github.com/zackwag/opa-ai) does). The page shows:

- When to put the bins out, as one big answer: "Tonight." (in streetlamp colors), "Tuesday night." or "Today."
- The next six weeks of pickups, including the ones a holiday moves or skips.
- The holidays that change the schedule.
- The calendar feed link, with a choice of reminder time, and a button to save the address as the home address.

The address you're viewing is kept in the browser's local storage. A browser that has never picked one opens to the home address.

## Endpoints

| Path | What |
| --- | --- |
| `/` | The page |
| `/schedule.ics?address=…&alarm=20:00` | iCalendar feed from a week back to a year ahead. `alarm` is `HH:MM` the night before, or `off`, and defaults to `ALARM_TIME` |
| `/schedule.json?address=…` | The same schedule as JSON, plus the pickup days, holidays and any warnings |
| `/api/home` | `GET` the home address, `PUT {"address": "…"}` to save one, `DELETE` to forget it |
| `/healthz` | `200` with `ok`, or `degraded` and the holiday warnings |

Leave out `address` and the home address is used, so `/schedule.ics` is a feed that follows the household. Addresses can be typed any way AIS understands, like `1830 fitzwater street`.

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

The city keeps its holiday list behind a CDN that blocks anything that isn't a browser, so this service doesn't fetch it. Holidays come from a file instead, one date per line (see [`holidays.example.txt`](holidays.example.txt)):

```text
2026-11-11 Veterans Day
2026-11-26 Thanksgiving Day
```

The file is re-read every 6 hours, so edits apply without a restart. A year with no holidays listed is still served, just without holiday moves, and `/healthz` reports `degraded` from two months before that year starts until you add it.

## Configuration

| Variable | Default | What |
| --- | --- | --- |
| `HOLIDAYS_FILE` | `/config/holidays.txt` | The holidays file |
| `ADDRESS_FILE` | `/data/address.txt` | Where the home address is saved |
| `ALARM_TIME` | `20:00` | The feed's default reminder the evening before a pickup, as `HH:MM`, or `off` |
| `REFRESH_INTERVAL` | `6h` | How long an address lookup is reused, and how often holidays are re-read |
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
      - ./holidays.txt:/config/holidays.txt:ro
      - phila-trashday-data:/data

volumes:
  phila-trashday-data:
```

The image is `FROM scratch` and runs as `nobody` (UID 65534). A named volume at `/data` starts out writable. If you bind-mount a host directory there instead, make it writable by UID 65534 (`chown 65534:65534`), or saving the home address fails with a message saying so.

Anyone who can reach the server can change the home address. That's fine on a home LAN; put it behind authentication before exposing it any further. Another website can't change it from a visitor's browser, because saving needs a JSON `PUT`, which browsers only send cross-site with a CORS preflight that this server never approves.

To subscribe from Apple Calendar, add the feed as an "On My Mac" subscription. An iCloud subscription is fetched by Apple's servers, which can't reach a LAN address.

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
