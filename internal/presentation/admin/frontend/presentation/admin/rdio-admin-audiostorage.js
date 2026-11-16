// rdio-admin-audiostorage: where recordings are kept. The newest days stay on the local
// disk; with cloud storage on, older recordings move to a Backblaze B2 bucket once a
// night and still play from there. Not a config section: it reads and saves its own
// settings, and the saved application key never comes back to the page.

import { getAdminContext } from '../../application/admin-context.js';
import { announce, showSnackbar } from '../../../../common/frontend/presentation/ui/primitives.js';
import { markRequired, onFieldEdit } from './row-builders.js';

// The approved wording of the section.
const ROWS = [
  {
    key: 'localDays',
    type: 'number',
    title: 'Keep on local disk',
    description:
      'Recordings newer than this many days stay on the disk of the computer running Rdio Scanner. Older recordings move to cloud storage.',
    suffix: 'days',
    required: true,
  },
  {
    key: 'cloudEnabled',
    type: 'toggle',
    title: 'Move older recordings to cloud storage',
    description:
      'Moves recordings older than the days above to a Backblaze B2 bucket, once a night. When off, all recordings stay on the local disk.',
  },
  { key: 'bucket', type: 'text', title: 'Bucket name', description: 'The name of your bucket in Backblaze B2.' },
  { key: 'endpoint', type: 'text', title: 'Endpoint', description: "The endpoint shown on your bucket's page in Backblaze B2." },
  {
    key: 'keyId',
    type: 'text',
    title: 'Key ID',
    description: 'The keyID of an application key that can read, write, and delete in this bucket.',
  },
  {
    key: 'appKey',
    type: 'password',
    title: 'Application key',
    description: 'Backblaze shows this only once, when you create the key. It is hidden here after you save.',
  },
];

const KEY_SAVED_HINT = 'Saved. Leave blank to keep it.';

// how often an open section reads the status again, so a move in progress shows.
const STATUS_POLL_MS = 30000;

const MB = 1048576;
const GB = 1073741824;

export function formatBytes(bytes) {
  const value = Number(bytes) || 0;

  if (value >= GB) return `${(value / GB).toFixed(1)} GB`;

  return `${Math.round(value / MB)} MB`;
}

// formatMoment names when the last move ran, the way the tile reads it.
export function formatMoment(ms, now = new Date()) {
  if (!ms) return 'Never';

  const at = new Date(ms);
  const time = at.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });

  const day = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const days = Math.round((day(now) - day(at)) / 86400000);

  if (days === 0) return `Today ${time}`;
  if (days === 1) return `Yesterday ${time}`;

  return `${at.toLocaleDateString([], { month: 'short', day: 'numeric' })}, ${time}`;
}

export function pruneNote(pruneDays) {
  if (!pruneDays) return 'Prune Days in Options is 0, so calls and their recordings are kept forever.';

  return `Prune Days in Options is ${pruneDays}. Calls older than that are deleted, and so are their recordings, wherever they are stored.`;
}

class RdioAdminAudioStorage extends HTMLElement {
  connectedCallback() {
    if (this.dataset.bound) return;
    this.dataset.bound = '1';

    this.saved = null;
    this.working = null;
    this.errors = {};
    this.testResult = null;

    this.load();

    this.poll = setInterval(() => this.refreshStatus(), STATUS_POLL_MS);
  }

  disconnectedCallback() {
    clearInterval(this.poll);
  }

  async load() {
    try {
      this.apply(await getAdminContext().api.getAudioStorage());
    } catch {
      this.replaceChildren(this.card([this.caption('Unable to read the audio storage settings.')]));
    }
  }

  async refreshStatus() {
    if (!this.saved) return;

    try {
      const reply = await getAdminContext().api.getAudioStorage();

      this.status = reply.status;
      this.pruneDays = reply.pruneDays;

      this.renderStats();
    } catch {
      // the next poll tries again.
    }
  }

  apply(reply) {
    this.saved = { ...reply.settings, appKey: '' };
    this.working = { ...this.saved };
    this.status = reply.status;
    this.pruneDays = reply.pruneDays;
    this.errors = {};

    this.render();
  }

  get dirty() {
    return JSON.stringify(this.working) !== JSON.stringify(this.saved);
  }

  caption(text) {
    const p = document.createElement('p');
    p.className = 'caption';
    p.textContent = text;

    return p;
  }

  card(children) {
    const root = document.createElement('div');
    root.className = 'rdio-admin-card options audio-storage';
    root.append(...children);

    return root;
  }

  render() {
    this.stats = document.createElement('div');
    this.stats.className = 'audio-storage-stats';

    const rows = ROWS.map((row) => this.row(row));

    const note = document.createElement('p');
    note.className = 'audio-storage-note';
    note.textContent = pruneNote(this.pruneDays);
    this.note = note;

    this.bar = document.createElement('div');
    this.bar.className = 'rdio-admin-section-bar';

    this.replaceChildren(this.card([this.stats, ...rows, this.testRow(), note, this.bar]));

    this.renderStats();
    this.renderBar();
  }

  renderStats() {
    if (!this.stats) return;

    const status = this.status ?? {};

    const tile = (label, value, detail) => {
      const box = document.createElement('div');
      box.className = 'audio-storage-stat';

      const name = document.createElement('span');
      name.className = 'stat-label';
      name.textContent = label;

      const number = document.createElement('strong');
      number.textContent = value;

      box.append(name, number);

      if (detail) {
        const extra = document.createElement('span');
        extra.className = 'stat-error';
        extra.textContent = detail;
        box.append(extra);
      }

      return box;
    };

    const lastMove = status.moving ? 'Moving now…' : formatMoment(status.lastMoveAt);
    const failed = !status.moving && status.lastError ? `Last move failed: ${status.lastError}` : '';

    this.stats.replaceChildren(
      tile('On local disk', formatBytes(status.localBytes)),
      tile('In cloud storage', formatBytes(status.cloudBytes)),
      tile('Last move', lastMove, failed),
    );

    if (this.note) this.note.textContent = pruneNote(this.pruneDays);
  }

  row(spec) {
    const row = document.createElement('div');
    row.className = 'row';
    row.dataset.control = spec.key;

    const description = document.createElement('p');
    description.innerHTML = '<span class="field-title"></span><br><span class="field-caption"></span>';
    description.querySelector('.field-title').textContent = spec.title;
    description.querySelector('.field-caption').textContent = spec.description;
    row.append(description);

    if (spec.type === 'toggle') {
      const toggle = document.createElement('rdio-toggle');
      toggle.setAttribute('checked', String(!!this.working[spec.key]));
      toggle.setAttribute('aria-label', spec.title);
      toggle.addEventListener('change', (ev) => this.edit(spec.key, ev.detail));
      row.append(toggle);

      return row;
    }

    const field = document.createElement('rdio-form-field');

    if (spec.required) markRequired(field, description.querySelector('.field-title'));

    const input = document.createElement('input');
    input.type = spec.type;
    input.setAttribute('aria-label', spec.title);
    input.autocomplete = 'off';

    if (spec.type === 'number') {
      input.min = '1';
      input.step = '1';
      input.required = true;
    }

    if (spec.key === 'appKey' && this.saved.appKeySet) input.placeholder = KEY_SAVED_HINT;

    input.value = this.working[spec.key] ?? '';

    onFieldEdit(input, () => {
      const value = spec.type === 'number' ? (input.value === '' ? undefined : Number(input.value)) : input.value;

      this.edit(spec.key, value);
      field.setError('');
    });

    field.append(input);

    if (spec.suffix) {
      const wrap = document.createElement('div');
      wrap.className = 'audio-storage-suffixed';

      const suffix = document.createElement('span');
      suffix.className = 'field-caption';
      suffix.textContent = spec.suffix;

      wrap.append(field, suffix);
      row.append(wrap);
    } else {
      row.append(field);
    }

    // an error the server answered a save or a test with is shown at once: the
    // operator submitted the field, which is what reaching it means.
    if (this.errors[spec.key]) {
      queueMicrotask(() => {
        field.markReached?.();
        field.setError?.(this.errors[spec.key]);
      });
    }

    return row;
  }

  testRow() {
    const row = document.createElement('div');
    row.className = 'row';
    row.dataset.control = 'test';

    const description = document.createElement('p');
    description.innerHTML = '<span class="field-title"></span><br><span class="field-caption"></span>';
    description.querySelector('.field-title').textContent = 'Test connection';
    description.querySelector('.field-caption').textContent = 'Checks that the bucket can be read, written to, and deleted from.';

    const control = document.createElement('div');
    control.className = 'audio-storage-test';

    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'rdio-button-mini';
    button.textContent = 'Test connection';

    const result = document.createElement('span');
    result.className = 'audio-storage-test-result';
    result.setAttribute('role', 'status');

    const show = () => {
      result.classList.toggle('ok', this.testResult?.ok === true);
      result.classList.toggle('failed', this.testResult?.ok === false);
      result.textContent = this.testResult?.text ?? '';
    };

    button.addEventListener('click', async () => {
      this.testResult = { text: 'Testing…' };
      show();

      button.disabled = true;

      try {
        const reply = await getAdminContext().api.testAudioStorage(this.working);

        if (reply.errors) {
          this.testResult = null;
          this.errors = reply.errors;
          this.render();

          return;
        }

        this.testResult = reply.ok
          ? { ok: true, text: 'Connected. Read, write, and delete work.' }
          : { ok: false, text: `Couldn't connect: ${reply.reason}` };
      } catch {
        this.testResult = { ok: false, text: "Couldn't connect: the endpoint couldn't be reached" };
      } finally {
        button.disabled = false;
      }

      show();
    });

    control.append(button, result);
    show();

    row.append(description, control);

    return row;
  }

  renderBar() {
    if (!this.bar) return;

    const save = document.createElement('button');
    save.type = 'button';
    save.className = 'rdio-button save';
    save.textContent = 'Save';
    save.disabled = !this.dirty;
    save.addEventListener('click', () => this.save());

    const discard = document.createElement('button');
    discard.type = 'button';
    discard.className = 'rdio-button-mini discard';
    discard.textContent = 'Discard';
    discard.disabled = !this.dirty;
    discard.addEventListener('click', () => {
      this.working = { ...this.saved };
      this.errors = {};
      this.render();
    });

    this.bar.replaceChildren(save, discard);
  }

  edit(key, value) {
    this.working = { ...this.working, [key]: value };

    if (this.errors[key]) {
      const { [key]: _, ...rest } = this.errors;
      this.errors = rest;
    }

    this.renderBar();
  }

  async save() {
    try {
      const reply = await getAdminContext().api.saveAudioStorage(this.working);

      if (!reply.ok) {
        this.errors = reply.errors ?? {};
        this.render();
        announce('Save failed: Audio storage');

        return;
      }

      this.apply(reply);
      announce('Saved');
    } catch {
      showSnackbar('Unable to save Audio storage');
      announce('Save failed: Audio storage');
    }
  }
}

customElements.define('rdio-admin-audiostorage', RdioAdminAudioStorage);
