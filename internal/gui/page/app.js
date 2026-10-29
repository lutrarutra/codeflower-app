// The launcher window: checks the container engine, starts sandboxes and
// shows how to connect VS Code to them.
'use strict';

const AGENT_NOTES = {
  claude: 'Anthropic. Sign in with a Claude subscription or an API key.',
  codex: 'OpenAI. Sign in with ChatGPT or an OpenAI API key.',
  dsh: 'DeepSeek, or any OpenAI-compatible endpoint, with an API key.'
};

const $ = (selector, root = document) => root.querySelector(selector);

const state = {
  system: null,
  agent: 'claude',
  folders: { inputs: [], outputs: [], code: [] },
  guideTab: 0,
  workers: []
};

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: options.body ? { 'Content-Type': 'application/json' } : {},
    body: options.body ? JSON.stringify(options.body) : undefined
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `The launcher answered ${response.status}`);
  return data;
}

function element(tag, properties = {}, children = []) {
  const node = document.createElement(tag);
  for (const [name, value] of Object.entries(properties)) {
    if (name === 'class') node.className = value;
    else if (name === 'text') node.textContent = value;
    else if (name.startsWith('on')) node.addEventListener(name.slice(2), value);
    else node.setAttribute(name, value);
  }
  for (const child of [].concat(children)) if (child) node.append(child);
  return node;
}

async function copy(text, button) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const area = element('textarea', { style: 'position:fixed;opacity:0' });
    area.value = text;
    document.body.append(area);
    area.select();
    document.execCommand('copy');
    area.remove();
  }
  const label = button.textContent;
  button.textContent = 'Copied';
  setTimeout(() => (button.textContent = label), 1500);
}

function commandBlock(command) {
  const button = element('button', { type: 'button', class: 'pill pill-small', text: 'Copy' });
  button.addEventListener('click', () => copy(command, button));
  return element('div', { class: 'command' }, [element('pre', { text: command }), button]);
}

function stepList(steps) {
  return element(
    'ol',
    { class: 'steps' },
    steps.map((step) =>
      element('li', {}, [
        element('span', { text: step.text }),
        step.link ? element('span', { text: ' ' }) : null,
        step.link ? element('a', { href: step.link, target: '_blank', rel: 'noreferrer', text: 'Instructions ↗' }) : null,
        step.command ? commandBlock(step.command) : null
      ])
    )
  );
}

function statusLine(kind, title, detail) {
  return element('div', { class: 'status' }, [
    element('span', { class: `status-icon ${kind}`, text: kind === 'ok' ? '✓' : kind === 'warn' ? '!' : '×' }),
    element('div', { class: 'grow' }, [element('strong', { text: title }), detail ? element('p', { class: 'muted small', text: detail }) : null])
  ]);
}

function recheckRow() {
  const button = element('button', { type: 'button', class: 'pill', text: 'Check again' });
  button.addEventListener('click', () => loadSystem());
  return element('div', { class: 'recheck' }, [button]);
}

function renderEngine() {
  const body = $('#engine-body');
  const system = state.system;
  body.replaceChildren();
  const engines = element(
    'div',
    { class: 'engines' },
    system.engines.map((engine) =>
      element('span', {
        class: engine.ready ? 'ready' : '',
        text: `${engine.label}: ${engine.ready ? `ready${engine.version ? ` ${engine.version}` : ''}` : engine.installed ? 'not running' : 'not installed'}`
      })
    )
  );

  if (system.engine) {
    const engine = system.engines.find((item) => item.name === system.engine);
    body.append(statusLine('ok', `${engine.label} is ready`, `Sandboxes run in containers with ${engine.label}, isolated from the rest of this computer.`), engines);
    const image = system.image;
    if (image && !image.ready) {
      body.append(
        element('div', { style: 'margin-top:1rem' }, [
          statusLine('warn', `The sandbox image "${image.name}" is not on this computer yet`, image.error),
          stepList(image.help || [])
        ]),
        recheckRow()
      );
    }
    return;
  }

  if (system.fix && system.fix.length) {
    body.append(statusLine('warn', system.problem, null), engines, stepList(system.fix), recheckRow());
    return;
  }

  body.append(
    statusLine('bad', 'No container engine found', `code flower runs each agent in a sandbox, which needs Docker, Podman or Apptainer. Install one for ${system.guide.system}:`),
    engines
  );
  const options = system.guide.options;
  state.guideTab = Math.min(state.guideTab, options.length - 1);
  const tabs = element(
    'div',
    { class: 'guide-tabs', role: 'tablist' },
    options.map((option, index) =>
      element('button', {
        type: 'button',
        class: 'pill pill-small',
        role: 'tab',
        'aria-selected': String(index === state.guideTab),
        text: option.title,
        onclick: () => {
          state.guideTab = index;
          renderEngine();
        }
      })
    )
  );
  const option = options[state.guideTab];
  body.append(tabs, option.note ? element('p', { class: 'muted', text: option.note }) : null, stepList(option.steps), recheckRow());
}

function renderAgents() {
  const container = $('#agents');
  container.replaceChildren(
    ...state.system.agents.map((agent) => {
      const button = element('button', { type: 'button', class: 'agent', role: 'radio', 'aria-checked': String(agent.id === state.agent) }, [
        element('strong', {}, [element('span', { class: 'check', text: '✓' }), agent.label]),
        element('span', { class: 'muted small', text: AGENT_NOTES[agent.id] || '' })
      ]);
      button.addEventListener('click', () => {
        state.agent = agent.id;
        renderAgents();
      });
      return button;
    })
  );
}

/** A path with the home folder written as ~. */
function short(path) {
  const home = state.system?.home;
  if (home && (path === home || path.startsWith(home + '/') || path.startsWith(home + '\\'))) return '~' + path.slice(home.length);
  return path;
}

const FOLDER_KINDS = {
  inputs: { root: '/data/input', title: 'Choose a folder with your data', noun: 'input' },
  outputs: { root: '/data/output', title: 'Choose a folder for the results', noun: 'output' },
  code: { root: '/data/code', title: 'Choose a repository or package for the agent to develop', noun: 'code' }
};

/** Where each folder appears in the sandbox; the same names as the launcher's folderName. */
function targets(kind) {
  const used = new Set();
  return state.folders[kind].map((path) => {
    const base = path.split(/[\\/]/).filter(Boolean).pop() || '';
    let name = base.replace(/[^A-Za-z0-9._-]+/g, '_').replace(/^\.+/, '');
    if (!name.replace(/_/g, '')) name = 'folder';
    let target = `${FOLDER_KINDS[kind].root}/${name}`;
    for (let number = 2; used.has(target); number++) target = `${FOLDER_KINDS[kind].root}/${name}-${number}`;
    used.add(target);
    return target;
  });
}

function renderFolders() {
  for (const kind of Object.keys(FOLDER_KINDS)) {
    const where = targets(kind);
    $(`#${kind}-list`).replaceChildren(
      ...state.folders[kind].map((path, index) =>
        element('li', {}, [
          element('span', { class: 'path', title: path }, [element('bdi', { text: short(path) })]),
          element('span', { class: 'target', text: `→ ${where[index]}` }),
          element('button', {
            type: 'button',
            class: 'icon-button',
            title: `Don't mount this ${FOLDER_KINDS[kind].noun} folder`,
            'aria-label': `Remove ${path}`,
            text: '×',
            onclick: () => {
              state.folders[kind].splice(index, 1);
              renderFolders();
            }
          })
        ])
      )
    );
  }
}

function canStart() {
  const system = state.system;
  return !!(system && system.engine && system.image && system.image.ready);
}

function renderStart() {
  $('#start').disabled = !canStart();
}

async function loadSystem() {
  $('#engine-body').replaceChildren(element('p', { class: 'muted' }, [element('span', { class: 'spinner' }), ' Looking for Docker, Podman or Apptainer…']));
  const query = new URLSearchParams({ engine: $('#engine').value, image: $('#image').value.trim() });
  try {
    state.system = await api(`/api/system?${query}`);
  } catch (error) {
    $('#engine-body').replaceChildren(statusLine('bad', 'Could not check this computer', error.message), recheckRow());
    return;
  }
  renderEngine();
  renderStart();
}

async function firstLoad() {
  try {
    state.system = await api('/api/system');
  } catch (error) {
    $('#engine-body').replaceChildren(statusLine('bad', 'Could not reach the launcher', error.message));
    return;
  }
  const settings = state.system.settings || {};
  state.agent = settings.agent || 'claude';
  for (const kind of Object.keys(FOLDER_KINDS)) state.folders[kind] = [...(settings[kind] || [])];
  $('#project').value = settings.project || 'default';
  $('#engine').value = settings.engine || 'auto';
  $('#image').value = settings.image || '';
  $('#version').textContent = state.system.version ? `launcher ${state.system.version}` : '';
  $('#projects-dir').textContent = state.system.projects;
  renderAgents();
  renderFolders();
  renderEngine();
  renderStart();
}

// Folder browser

const browser = {
  which: 'inputs',
  path: '',
  listing: null
};

async function browse(path) {
  $('#browser-error').textContent = '';
  try {
    const listing = await api(`/api/folders?${new URLSearchParams({ path })}`);
    browser.listing = listing;
    browser.path = listing.path;
  } catch (error) {
    $('#browser-error').textContent = error.message;
    return;
  }
  const listing = browser.listing;
  $('#browser-location').value = listing.path;
  $('#browser-up').disabled = !listing.parent;
  $('#browser-places').replaceChildren(
    ...listing.places.map((place) => element('button', { type: 'button', text: place.name, title: place.path, onclick: () => browse(place.path) }))
  );
  const list = $('#browser-list');
  list.replaceChildren(
    ...(listing.folders.length
      ? listing.folders.map((folder) =>
          element('li', { role: 'option', tabindex: '0', text: folder.name, title: folder.path, onclick: () => browse(folder.path), onkeydown: (event) => event.key === 'Enter' && (event.preventDefault(), browse(folder.path)) })
        )
      : [element('li', { class: 'empty', text: 'No folders here' })])
  );
  list.scrollTop = 0;
  const files = listing.files;
  $('#browser-summary').textContent = `${listing.folders.length} folder${listing.folders.length === 1 ? '' : 's'}, ${files} file${files === 1 ? '' : 's'}`;
}

function openBrowser(which) {
  browser.which = which;
  $('#browser-title').textContent = FOLDER_KINDS[which].title;
  $('#browser-new').hidden = which === 'inputs';
  $('#browser').showModal();
  // Start next to the last folder chosen, where the next one often is.
  const last = state.folders[which].at(-1);
  browse(last ? last.replace(/[\\/][^\\/]+[\\/]?$/, '') || last : state.system?.home || '');
}

$('#browser-up').addEventListener('click', () => browser.listing?.parent && browse(browser.listing.parent));
$('#browser-go').addEventListener('click', () => browse($('#browser-location').value.trim()));
$('#browser-location').addEventListener('keydown', (event) => {
  if (event.key === 'Enter') {
    event.preventDefault();
    browse($('#browser-location').value.trim());
  }
});
$('#browser-choose').addEventListener('click', () => {
  const chosen = state.folders[browser.which];
  if (!chosen.includes(browser.path)) chosen.push(browser.path);
  renderFolders();
  $('#browser').close();
});
$('#browser-new').addEventListener('click', async () => {
  const name = prompt('Name of the new folder:', 'results');
  if (!name) return;
  try {
    const folder = await api('/api/folders', { method: 'POST', body: { parent: browser.path, name } });
    await browse(folder.path);
  } catch (error) {
    $('#browser-error').textContent = error.message;
  }
});

for (const button of document.querySelectorAll('[data-browse]')) {
  button.addEventListener('click', () => openBrowser(button.dataset.browse));
}

$('#engine').addEventListener('change', () => loadSystem());
$('#image').addEventListener('change', () => loadSystem());

// Starting and watching sandboxes

$('#start-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  $('#start-error').textContent = '';
  const { inputs, outputs, code } = state.folders;
  if (!inputs.length && !code.length && !confirm('No input or code folder is chosen, so the agent will not see any of your data. Start anyway?')) return;
  const button = $('#start');
  button.disabled = true;
  button.textContent = 'Starting…';
  try {
    await api('/api/workers', {
      method: 'POST',
      body: {
        agent: state.agent,
        inputs,
        outputs,
        code,
        project: $('#project').value.trim(),
        engine: $('#engine').value,
        image: $('#image').value.trim()
      }
    });
    await refreshWorkers();
    $('#workers').scrollIntoView({ behavior: 'smooth', block: 'start' });
  } catch (error) {
    $('#start-error').textContent = error.message;
  } finally {
    button.textContent = 'Start sandbox';
    renderStart();
  }
});

const STATUS_TEXT = { starting: 'starting', ready: 'running', stopping: 'stopping', stopped: 'stopped', failed: 'failed' };
const cards = new Map();

function vscodeLink(url) {
  return `vscode://codeflower.codeflower/connect?${new URLSearchParams({ address: url })}`;
}

function renderWorker(worker) {
  let card = cards.get(worker.id);
  if (!card) {
    card = $('#worker-template').content.firstElementChild.cloneNode(true);
    $('.stop', card).addEventListener('click', async () => {
      try {
        await api(`/api/workers/${worker.id}/stop`, { method: 'POST', body: {} });
      } finally {
        refreshWorkers();
      }
    });
    $('.forget', card).addEventListener('click', async () => {
      await api(`/api/workers/${worker.id}`, { method: 'DELETE', body: {} }).catch(() => {});
      refreshWorkers();
    });
    $('.copy', card).addEventListener('click', (event) => copy(card.dataset.url, event.currentTarget));
    cards.set(worker.id, card);
  }
  const badge = $('.badge', card);
  badge.className = `badge ${worker.status}`;
  badge.textContent = STATUS_TEXT[worker.status] || worker.status;
  $('h3', card).textContent = `${worker.label} · ${worker.project}`;
  const count = (paths, noun) => (paths.length ? `${paths.length} ${noun} folder${paths.length === 1 ? '' : 's'}` : null);
  $('.meta', card).textContent = [
    worker.engine,
    count(worker.inputs, 'input') || 'no input folder',
    count(worker.outputs, 'output'),
    count(worker.code, 'code')
  ]
    .filter(Boolean)
    .join(' · ');
  $('.meta', card).title = [
    ...worker.inputs.map((path) => `input ${path}`),
    ...worker.outputs.map((path) => `output ${path}`),
    ...worker.code.map((path) => `code ${path}`)
  ].join('\n');
  const running = worker.status === 'starting' || worker.status === 'ready';
  $('.stop', card).hidden = !running;
  $('.stop', card).disabled = worker.status === 'stopping';
  $('.forget', card).hidden = running || worker.status === 'stopping';
  const connect = $('.connect', card);
  connect.hidden = !worker.url;
  if (worker.url) {
    card.dataset.url = worker.url;
    // The token stays hidden on screen; Copy and Open use the whole line.
    $('.url', card).textContent = worker.url.replace(/token=[^&\s]+/, 'token=••••••••');
    $('.open', card).href = vscodeLink(worker.url);
  }
  $('.worker-error', card).textContent = worker.error || '';
  const pre = $('.log pre', card);
  const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
  pre.textContent = worker.log.join('\n');
  if (atBottom) pre.scrollTop = pre.scrollHeight;
  if (worker.status === 'failed') $('.log', card).open = true;
  return card;
}

async function refreshWorkers() {
  try {
    state.workers = await api('/api/workers');
  } catch {
    return;
  }
  const container = $('#workers');
  const nodes = state.workers.map(renderWorker);
  for (const [id, card] of cards) {
    if (!state.workers.some((worker) => worker.id === id)) {
      card.remove();
      cards.delete(id);
    }
  }
  container.replaceChildren(...nodes);
}

$('#quit').addEventListener('click', async () => {
  const running = state.workers.filter((worker) => ['starting', 'ready'].includes(worker.status)).length;
  if (running && !confirm(`Stop ${running} running sandbox${running === 1 ? '' : 'es'} and quit the launcher?`)) return;
  await api('/api/quit', { method: 'POST', body: {} }).catch(() => {});
  document.body.replaceChildren(
    element('main', { style: 'padding-top:4rem;text-align:center' }, [
      element('h1', { text: 'The launcher has stopped.' }),
      element('p', { class: 'muted', text: 'You can close this tab. Start code flower again to run another sandbox.' })
    ])
  );
});

// This computer's processors and memory, refreshed like htop

/** Bytes as htop writes them: 512M, 15.8G. */
function bytes(value) {
  const units = ['B', 'K', 'M', 'G', 'T'];
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index++;
  }
  return `${value >= 100 || index < 2 ? Math.round(value) : value.toFixed(1)}${units[index]}`;
}

function meter(segments) {
  return element(
    'div',
    { class: 'meter' },
    segments.filter(([, percent]) => percent > 0).map(([kind, percent]) => element('span', { class: kind, style: `width:${percent}%` }))
  );
}

function setMeter(id, segments) {
  $(`#${id}`).replaceChildren(...meter(segments).childNodes);
}

function renderResources(resources) {
  const busy = (core) => core.user + core.system + core.iowait;
  const cores = resources.cores;
  $('#cores').replaceChildren(
    ...cores.map((core, index) =>
      element('div', { class: 'core', title: `core ${index + 1}: ${Math.round(core.user)}% user, ${Math.round(core.system)}% system, ${Math.round(core.iowait)}% waiting for disk` }, [
        element('span', { class: 'core-label', text: String(index + 1) }),
        meter([['user', core.user], ['system', core.system], ['iowait', core.iowait]]),
        element('span', { class: 'core-value', text: `${busy(core).toFixed(1)}%` })
      ])
    )
  );
  const memory = resources.memory;
  const share = (value, total) => (total ? (100 * value) / total : 0);
  setMeter('memory-meter', [['used', share(memory.used, memory.total)], ['cache', share(memory.cached, memory.total)]]);
  $('#cache-key').hidden = !memory.cached;
  $('#memory-value').textContent = `${bytes(memory.used)} / ${bytes(memory.total)}`;
  $('#swap-row').hidden = !memory.swapTotal;
  setMeter('swap-meter', [['system', share(memory.swapUsed, memory.swapTotal)]]);
  $('#swap-value').textContent = `${bytes(memory.swapUsed)} / ${bytes(memory.swapTotal)}`;
  const average = cores.length ? cores.reduce((sum, core) => sum + busy(core), 0) / cores.length : 0;
  $('#resources-summary').textContent = [
    `${cores.length} core${cores.length === 1 ? '' : 's'}, ${Math.round(average)}% busy`,
    resources.load ? `load ${resources.load.map((value) => value.toFixed(2)).join(' ')}` : null
  ]
    .filter(Boolean)
    .join(' · ');
  // Docker Desktop's virtual machine has its own share of this computer.
  const desktop = state.system?.engine === 'docker' && /macOS|Windows/.test(state.system.system || '');
  $('#resources-note').textContent = desktop
    ? 'Sandboxes run in Docker Desktop\'s virtual machine, which may use only part of these processors and memory (Docker Desktop → Settings → Resources).'
    : '';
}

let loadingResources = false;
async function refreshResources() {
  if (loadingResources || document.hidden) return;
  loadingResources = true;
  try {
    renderResources(await api('/api/resources'));
  } catch {
    // The launcher may be busy or gone; the next refresh tries again.
  } finally {
    loadingResources = false;
  }
}

firstLoad();
refreshWorkers();
refreshResources();
setInterval(refreshWorkers, 1500);
setInterval(refreshResources, 1000);
