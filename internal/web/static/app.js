// Finds an address's schedule, shows when to put the bins out, and builds
// the calendar feed link. Addresses can be saved on the server, each with
// its own /a/{id} link. The address being viewed is kept in this browser's
// localStorage; the default saved address is the fallback for every
// browser.

const ADDRESS_KEY = 'phila-trashday.address';
const ALARM_KEY = 'phila-trashday.alarm';
const CARTO_URL = 'https://phl.carto.com/api/v2/sql';
const DAY_MS = 24 * 60 * 60 * 1000;

// How far ahead the list goes before "Show the rest of the year".
const UPCOMING_DAYS = 42;

// How long "Remove" waits for its confirming second click.
const CONFIRM_MS = 4000;

// A saved address's page: /a/ and its ID.
const SAVED_PATH = /^\/a\/([0-9a-f-]{36})$/i;

const $ = (id) => document.getElementById(id);

const state = {
  // data is the last /schedule.json response.
  data: null,
  // saved is the last /api/addresses response.
  saved: { addresses: [], saving: false },
  showAll: false,
};

// Storage can throw in private windows or with site data blocked, and the
// page works without it.
function load(key) {
  try {
    return localStorage.getItem(key) || '';
  } catch {
    return '';
  }
}

function save(key, value) {
  try {
    if (value) {
      localStorage.setItem(key, value);
    } else {
      localStorage.removeItem(key);
    }
  } catch {
    // Not remembered, which is fine.
  }
}

// Addresses

const STREET_TYPES = {
  street: 'ST', avenue: 'AVE', boulevard: 'BLVD', drive: 'DR', road: 'RD',
  lane: 'LN', court: 'CT', place: 'PL', circle: 'CIR', terrace: 'TER',
  parkway: 'PKWY', square: 'SQ', trail: 'TRL',
};

const DIRECTIONS = {
  north: 'N', south: 'S', east: 'E', west: 'W',
  northeast: 'NE', northwest: 'NW', southeast: 'SE', southwest: 'SW',
};

// normalizeAddress writes an address the way the city's records do, so
// "1830 Fitzwater Street" finds "1830 FITZWATER ST". Adapted from opa-ai.
export function normalizeAddress(input) {
  return input
    .trim()
    .replaceAll('.', '')
    .split(/\s+/)
    .map((word, i) => {
      const lower = word.toLowerCase();
      if (DIRECTIONS[lower]) {
        return DIRECTIONS[lower];
      }
      if (STREET_TYPES[lower] && i > 0) {
        return STREET_TYPES[lower];
      }
      const ordinal = /^(\d+)(st|nd|rd|th)$/i.exec(word);
      if (ordinal && i > 0) {
        return ordinal[1] + ordinal[2].toUpperCase();
      }
      return word.toUpperCase();
    })
    .join(' ');
}

// displayAddress turns "2714 S MARVINE ST" into "2714 S Marvine St".
export function displayAddress(address) {
  return address
    .split(' ')
    .map((word) => {
      if (/^(N|S|E|W|NE|NW|SE|SW)$/.test(word)) {
        return word;
      }
      if (/^\d+(ST|ND|RD|TH)$/.test(word)) {
        return word.toLowerCase();
      }
      return word.charAt(0) + word.slice(1).toLowerCase();
    })
    .join(' ');
}

// searchPrefixes turns an address still being typed into the prefixes to
// look for. When the last word could be the start of a street type, like
// "str" or "aven", the abbreviated form is tried too, since the records say
// ST and AVE and would never match STR. The literal form stays, because
// "1830 S STR" could just as well be the start of STRAWBERRY.
export function searchPrefixes(query) {
  const words = query.trim().split(/\s+/);
  const prefixes = [normalizeAddress(words.join(' '))];
  const last = words.at(-1).toLowerCase();
  if (words.length > 2 && last.length >= 3) {
    const type = Object.keys(STREET_TYPES).find((t) => t.startsWith(last));
    if (type) {
      const abbreviated = normalizeAddress([...words.slice(0, -1), STREET_TYPES[type]].join(' '));
      if (abbreviated !== prefixes[0]) {
        prefixes.push(abbreviated);
      }
    }
  }
  return prefixes.map((p) => p.replace(/[^A-Z0-9 ,'#/&-]/g, '')).filter((p) => p.length >= 3);
}

let suggestSeq = 0;

// suggest asks the city's property records for addresses starting with
// query. Only characters an address can have are sent, so the one quote
// that's escaped is the only special character left.
async function suggest(query) {
  const prefixes = searchPrefixes(query);
  if (prefixes.length === 0) {
    return [];
  }
  const where = prefixes.map((p) => `UPPER(location) LIKE '${p.replaceAll("'", "''")}%'`).join(' OR ');
  const sql = `SELECT DISTINCT location FROM opa_properties_public WHERE ${where} ORDER BY location LIMIT 8`;
  try {
    const res = await fetch(`${CARTO_URL}?q=${encodeURIComponent(sql)}`);
    if (!res.ok) {
      return [];
    }
    const body = await res.json();
    return (body.rows || []).map((row) => row.location).filter(Boolean);
  } catch {
    return [];
  }
}

// Dates. The server sends calendar dates as YYYY-MM-DD; they're read as
// local midnight so comparisons match the visitor's today.

function parseDate(s) {
  const [y, m, d] = s.split('-').map(Number);
  return new Date(y, m - 1, d);
}

function today() {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate());
}

function daysFrom(from, to) {
  return Math.round((to - from) / DAY_MS);
}

function addDays(date, n) {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate() + n);
}

function iso(date) {
  const p = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${p(date.getMonth() + 1)}-${p(date.getDate())}`;
}

const fmt = (options) => new Intl.DateTimeFormat('en-US', options);
const longDate = fmt({ weekday: 'long', month: 'long', day: 'numeric' });
const weekdayName = fmt({ weekday: 'long' });
const shortWeekday = fmt({ weekday: 'short' });
const monthName = fmt({ month: 'long', year: 'numeric' });
const monthDay = fmt({ month: 'short', day: 'numeric' });
const holidayDate = fmt({ weekday: 'short', month: 'short', day: 'numeric' });

function capitalize(s) {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function binsPhrase(kinds) {
  return kinds.includes('recycling') ? 'trash and recycling' : 'trash';
}

// The schedule

// holidayFor names the holiday that moved a pickup: the latest one in its
// week on or before the day it's normally on.
function holidayFor(collection, holidays) {
  const scheduled = parseDate(collection.scheduled_date);
  const monday = addDays(scheduled, -((scheduled.getDay() + 6) % 7));
  const match = holidays
    .filter((h) => {
      const d = parseDate(h.date);
      return d >= monday && d <= scheduled;
    })
    .at(-1);
  return match ? match.name : '';
}

// skippedPickups lists the second trash pickups that holidays cancel, so
// the list can say there's none that week instead of leaving a gap.
function skippedPickups(data, from) {
  if (!data.secondary_day) {
    return [];
  }
  const weekdays = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
  const offset = (weekdays.indexOf(data.secondary_day) + 6) % 7;
  const taken = new Set(data.collections.map((c) => c.date));
  const skipped = new Map();
  for (const h of data.holidays) {
    const holiday = parseDate(h.date);
    const monday = addDays(holiday, -((holiday.getDay() + 6) % 7));
    const date = addDays(monday, offset);
    if (date >= from && !taken.has(iso(date)) && !skipped.has(iso(date))) {
      skipped.set(iso(date), { date: iso(date), skipped: true, holiday: h.name });
    }
  }
  return [...skipped.values()];
}

function renderHero(data) {
  const now = today();
  const next = data.collections.find((c) => parseDate(c.date) >= now);
  const hero = $('hero');
  hero.classList.remove('is-night');

  if (!next) {
    $('answer').textContent = 'No pickups found.';
    $('instruction').textContent = 'The city has no collection days for this address.';
    $('when').textContent = '';
    return;
  }

  const date = parseDate(next.date);
  const days = daysFrom(now, date);
  const bins = binsPhrase(next.kinds);
  let moved = '';
  if (next.moved) {
    const name = holidayFor(next, data.holidays);
    moved = ` It's a day later than usual${name ? ` because of ${name}` : ''}.`;
  }

  if (days === 0) {
    $('answer').textContent = 'Today.';
    $('instruction').textContent = `Pickup is today: ${bins}.`;
    $('when').textContent = moved.trim();
    return;
  }

  if (days === 1) {
    hero.classList.add('is-night');
    $('answer').textContent = 'Tonight.';
  } else {
    const night = addDays(date, -1);
    const label = `${weekdayName.format(night)} night`;
    $('answer').textContent = days > 7 ? `${label}, ${monthDay.format(night)}.` : `${label}.`;
  }
  $('instruction').textContent = `Put out ${bins} after 7 PM.`;
  const whenText = days === 1
    ? `Pickup is tomorrow, ${longDate.format(date)}.`
    : `Pickup is ${longDate.format(date)}, in ${days} days.`;
  $('when').textContent = whenText + moved;
}

function pickupRow(item, data, isNext, nextIsTomorrow) {
  const date = parseDate(item.date);
  const li = document.createElement('li');
  li.className = 'pickup';

  const dateCell = document.createElement('div');
  dateCell.className = 'pickup-date';
  const day = document.createElement('span');
  day.className = 'pickup-day';
  day.textContent = String(date.getDate());
  const weekday = document.createElement('span');
  weekday.className = 'pickup-weekday';
  weekday.textContent = shortWeekday.format(date);
  dateCell.append(day, weekday);

  const body = document.createElement('div');
  const bins = document.createElement('div');
  bins.className = 'bins';
  const note = document.createElement('p');
  note.className = 'pickup-note';

  if (item.skipped) {
    li.classList.add('is-skipped');
    bins.textContent = 'No trash pickup';
    note.textContent = `Skipped for ${item.holiday} week.`;
  } else {
    for (const kind of ['trash', 'recycling']) {
      if (item.kinds.includes(kind)) {
        const bin = document.createElement('span');
        bin.className = `bin bin-${kind}`;
        bin.textContent = capitalize(kind);
        bins.append(bin);
      }
    }
    if (isNext) {
      li.classList.add('is-next');
      if (nextIsTomorrow) {
        const tag = document.createElement('span');
        tag.className = 'pickup-tag';
        tag.textContent = 'Put out tonight';
        bins.append(tag);
      }
    }
    if (item.moved) {
      const name = holidayFor(item, data.holidays);
      const from = weekdayName.format(parseDate(item.scheduled_date));
      note.textContent = `Moved from ${from}${name ? ` for ${name}` : ''}.`;
    }
  }

  body.append(bins);
  if (note.textContent) {
    body.append(note);
  }
  li.append(dateCell, body);
  return li;
}

function renderUpcoming(data) {
  const now = today();
  const items = [
    ...data.collections.filter((c) => parseDate(c.date) >= now),
    ...skippedPickups(data, now),
  ].sort((a, b) => a.date.localeCompare(b.date));

  const cutoff = addDays(now, UPCOMING_DAYS);
  const shown = state.showAll ? items : items.filter((item) => parseDate(item.date) <= cutoff);
  const next = items.find((item) => !item.skipped);
  const nextIsTomorrow = next && daysFrom(now, parseDate(next.date)) === 1;

  const container = $('upcoming');
  container.replaceChildren();
  let list = null;
  let month = '';
  for (const item of shown) {
    const label = monthName.format(parseDate(item.date));
    if (label !== month) {
      month = label;
      const heading = document.createElement('h3');
      heading.className = 'month';
      heading.textContent = label;
      list = document.createElement('ul');
      list.className = 'pickups';
      container.append(heading, list);
    }
    list.append(pickupRow(item, data, item === next, nextIsTomorrow));
  }
  $('more').hidden = state.showAll || shown.length === items.length;
}

function renderHolidays(data) {
  const now = today();
  const upcoming = data.holidays.filter((h) => parseDate(h.date) >= now);
  $('holidays-block').hidden = upcoming.length === 0;
  const list = $('holidays');
  list.replaceChildren(...upcoming.map((h) => {
    const li = document.createElement('li');
    const time = document.createElement('time');
    time.dateTime = h.date;
    time.textContent = holidayDate.format(parseDate(h.date));
    const name = document.createElement('span');
    name.textContent = h.name || 'Holiday';
    li.append(time, name);
    return li;
  }));
}

// The calendar feed

// feedURL is a saved address's own link, which keeps working if it's
// renamed or its address changes, or else the link by address.
function feedURL(data, file) {
  if (data.saved) {
    return new URL(`/a/${data.saved.id}/${file}`, window.location.origin);
  }
  const url = new URL(`/${file}`, window.location.origin);
  url.searchParams.set('address', data.address);
  return url;
}

// feedNote says where the feed link works and how to keep its reminders.
function feedNote(isPublic) {
  if (isPublic) {
    return 'This link works from anywhere, so iCloud and Skylight can subscribe to it. '
      + 'On a Mac, uncheck Remove Alerts when Calendar asks, or the reminders are dropped.';
  }
  if (state.saved.public_url) {
    return 'This link only works on your network. Save this address to get one iCloud and Skylight can reach.';
  }
  return 'On a Mac, choose On My Mac when Calendar asks where to add it. '
    + 'Calendars kept in iCloud are refreshed by Apple\'s servers, which can\'t reach this one.';
}

function renderFeed(data) {
  // A saved address's public link, when there is one, works for calendar
  // services as well as on the network, so it's the one to subscribe to.
  const saved = data.saved && state.saved.addresses.find((a) => a.id === data.saved.id);
  const publicFeed = saved ? saved.public_feed : '';
  const ics = publicFeed ? new URL(publicFeed) : feedURL(data, 'schedule.ics');
  ics.searchParams.set('alarm', $('alarm').value);
  // A colon is fine in a query string, and 20:00 reads better than 20%3A00.
  const link = ics.toString().replace('%3A', ':');
  $('feed-url').value = link;
  $('subscribe').href = link.replace(/^https?:/, 'webcal:');
  $('json-link').href = feedURL(data, 'schedule.json').toString();
  $('feed-note').textContent = feedNote(Boolean(publicFeed));
  renderSave(data);
}

let removeTimer = 0;

function resetRemove() {
  clearTimeout(removeTimer);
  removeTimer = 0;
  $('remove-saved').textContent = 'Remove';
}

function renderSave(data) {
  $('save').hidden = !state.saved.saving;
  $('save-error').hidden = true;
  resetRemove();
  $('save-new').hidden = Boolean(data.saved);
  $('save-existing').hidden = !data.saved;
  if (!data.saved) {
    return;
  }
  const name = data.saved.label ? `as ${data.saved.label}` : 'without a label';
  $('save-status').textContent = data.saved.default
    ? `Saved ${name}. It's the default, so every device on your network opens to it.`
    : `Saved ${name}.`;
  $('make-default').hidden = data.saved.default;
}

// renderSavedList fills a list of saved addresses with links to their
// pages, marking the one being viewed.
function renderSavedList(list, current) {
  list.replaceChildren(...state.saved.addresses.map((a) => {
    const li = document.createElement('li');
    const link = document.createElement('a');
    link.href = `/a/${a.id}`;
    if (a.id === current) {
      link.setAttribute('aria-current', 'page');
    }
    const name = document.createElement('span');
    name.className = 'saved-name';
    name.textContent = a.label || displayAddress(a.address);
    link.append(name);
    if (a.label) {
      const address = document.createElement('span');
      address.className = 'saved-address';
      address.textContent = displayAddress(a.address);
      link.append(address);
    }
    if (a.default) {
      const tag = document.createElement('span');
      tag.className = 'saved-default';
      tag.textContent = 'Default';
      link.append(tag);
    }
    li.append(link);
    return li;
  }));
}

function renderSaved() {
  const current = state.data && state.data.saved ? state.data.saved.id : '';
  const any = state.saved.addresses.length > 0;
  $('saved-block').hidden = !any;
  $('picks').hidden = !any;
  renderSavedList($('saved-list'), current);
  renderSavedList($('picks-list'), '');
}

function renderWarnings(data) {
  const warnings = $('warnings');
  warnings.hidden = data.warnings.length === 0;
  warnings.textContent = data.warnings.map((w) => capitalize(w) + '.').join(' ');
}

function render() {
  const data = state.data;
  $('place-address').textContent = displayAddress(data.address);
  $('place').hidden = false;
  $('search-view').hidden = true;
  $('answer-view').hidden = false;
  $('details').hidden = false;
  renderHero(data);
  renderUpcoming(data);
  renderHolidays(data);
  renderFeed(data);
  renderSaved();
  renderWarnings(data);
  document.title = `Trash night: ${displayAddress(data.address)}`;
  // A saved address shows its own link in the address bar, so it can be
  // bookmarked or shared.
  const path = data.saved ? `/a/${data.saved.id}` : '/';
  if (window.location.pathname !== path) {
    history.replaceState(null, '', path);
  }
}

// Views

function showSearch(message) {
  $('hero').classList.remove('is-night');
  $('search-view').hidden = false;
  $('answer-view').hidden = true;
  $('details').hidden = true;
  $('place').hidden = true;
  $('search-error').hidden = !message;
  $('search-error').textContent = message || '';
  document.title = 'Trash night';
  if (window.location.pathname !== '/') {
    history.replaceState(null, '', '/');
  }
  renderSaved();
  $('address').focus();
}

async function errorMessage(res) {
  try {
    const body = await res.json();
    return capitalize(body.error) + '.';
  } catch {
    return `The server answered ${res.status}. Try again in a few minutes.`;
  }
}

async function showAddress(address) {
  $('show').disabled = true;
  try {
    const res = await fetch(`/schedule.json?address=${encodeURIComponent(address)}`);
    if (!res.ok) {
      showSearch(await errorMessage(res));
      return;
    }
    state.data = await res.json();
    state.showAll = false;
    save(ADDRESS_KEY, state.data.address);
    render();
  } catch {
    showSearch('This server didn\'t answer. Check that it\'s running and try again.');
  } finally {
    $('show').disabled = false;
  }
}

async function loadSaved() {
  try {
    const res = await fetch('/api/addresses');
    if (res.ok) {
      state.saved = await res.json();
    }
  } catch {
    // Nothing saved, which the page handles.
  }
}

// changeSaved sends a change to the saved addresses, then reloads them and
// the schedule, whose saved details may have changed.
async function changeSaved(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    $('save-error').textContent = await errorMessage(res);
    $('save-error').hidden = false;
    return;
  }
  await loadSaved();
  await showAddress(state.data.address);
}

// The address search box: a combobox over the city's records.

function wireSearch() {
  const input = $('address');
  const list = $('suggestions');
  let options = [];
  let active = -1;
  let timer = 0;

  function close() {
    list.hidden = true;
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
    active = -1;
  }

  function highlight(i) {
    active = i;
    [...list.children].forEach((li, j) => li.setAttribute('aria-selected', String(j === i)));
    if (i >= 0) {
      input.setAttribute('aria-activedescendant', list.children[i].id);
    }
  }

  function choose(address) {
    input.value = displayAddress(address);
    close();
    showAddress(address);
  }

  function open(addresses) {
    options = addresses;
    list.replaceChildren(...addresses.map((address, i) => {
      const li = document.createElement('li');
      li.id = `suggestion-${i}`;
      li.setAttribute('role', 'option');
      li.setAttribute('aria-selected', 'false');
      li.textContent = displayAddress(address);
      li.addEventListener('mousedown', (e) => {
        e.preventDefault();
        choose(address);
      });
      li.addEventListener('mousemove', () => highlight(i));
      return li;
    }));
    if (addresses.length === 0) {
      close();
      return;
    }
    list.hidden = false;
    input.setAttribute('aria-expanded', 'true');
    highlight(-1);
  }

  input.addEventListener('input', () => {
    clearTimeout(timer);
    $('search-error').hidden = true;
    const query = input.value;
    timer = setTimeout(async () => {
      const seq = ++suggestSeq;
      const addresses = await suggest(query);
      // Answers can arrive out of order; only the newest query's count.
      if (seq === suggestSeq) {
        open(addresses);
      }
    }, 250);
  });

  input.addEventListener('keydown', (e) => {
    if (list.hidden) {
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      highlight(Math.min(active + 1, options.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      highlight(Math.max(active - 1, 0));
    } else if (e.key === 'Enter' && active >= 0) {
      e.preventDefault();
      choose(options[active]);
    } else if (e.key === 'Escape') {
      close();
    }
  });

  input.addEventListener('blur', close);

  $('search-form').addEventListener('submit', (e) => {
    e.preventDefault();
    close();
    const address = normalizeAddress(input.value);
    if (address === '') {
      showSearch('Enter an address, like 1830 Fitzwater St.');
      return;
    }
    showAddress(address);
  });
}

function wireDetails() {
  $('change').addEventListener('click', () => {
    $('address').value = '';
    showSearch('');
  });
  $('more').addEventListener('click', () => {
    state.showAll = true;
    renderUpcoming(state.data);
  });

  const alarm = load(ALARM_KEY);
  if ([...$('alarm').options].some((o) => o.value === alarm)) {
    $('alarm').value = alarm;
  }
  $('alarm').addEventListener('change', () => {
    save(ALARM_KEY, $('alarm').value);
    renderFeed(state.data);
  });

  $('copy').addEventListener('click', async () => {
    const button = $('copy');
    try {
      await navigator.clipboard.writeText($('feed-url').value);
      button.textContent = 'Copied';
    } catch {
      $('feed-url').select();
      button.textContent = 'Press Ctrl+C to copy';
    }
    setTimeout(() => {
      button.textContent = 'Copy link';
    }, 2000);
  });

  $('save-new').addEventListener('submit', (e) => {
    e.preventDefault();
    changeSaved('POST', '/api/addresses', { address: state.data.address, label: $('save-label').value });
    $('save-label').value = '';
  });
  $('make-default').addEventListener('click', () => {
    changeSaved('PATCH', `/api/addresses/${state.data.saved.id}`, { default: true });
  });
  // Removing breaks every calendar subscribed to the address's link, so it
  // takes a second click.
  $('remove-saved').addEventListener('click', () => {
    if (!removeTimer) {
      $('remove-saved').textContent = 'Its links will stop working. Click again to remove';
      removeTimer = setTimeout(resetRemove, CONFIRM_MS);
      return;
    }
    resetRemove();
    changeSaved('DELETE', `/api/addresses/${state.data.saved.id}`);
  });
}

async function start() {
  wireSearch();
  wireDetails();
  await loadSaved();

  const match = SAVED_PATH.exec(window.location.pathname);
  if (match) {
    const saved = state.saved.addresses.find((a) => a.id === match[1].toLowerCase());
    if (saved) {
      await showAddress(saved.address);
    } else {
      showSearch('That saved address has been removed. Search for it again, or pick another one.');
    }
    return;
  }

  const fallback = state.saved.addresses.find((a) => a.default);
  const address = load(ADDRESS_KEY) || (fallback ? fallback.address : '');
  if (address) {
    await showAddress(address);
  } else {
    showSearch('');
  }
}

start();
