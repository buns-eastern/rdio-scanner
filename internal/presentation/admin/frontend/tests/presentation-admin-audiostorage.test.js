// the audio storage section: the approved wording, the saved key kept off the page,
// field errors from the server shown beside their fields, and the test connection answers

import './dom-shim.js';

import assert from 'node:assert/strict';
import test from 'node:test';

import { getAdminContext } from '../application/admin-context.js';

import '../../../common/frontend/presentation/ui/primitives.js';

import { formatBytes, formatMoment, pruneNote } from '../presentation/admin/rdio-admin-audiostorage.js';

import { findAll } from './dom-shim.js';

const REPLY = {
  ok: true,
  settings: { localDays: 30, cloudEnabled: true, bucket: 'rdio-recordings', endpoint: 's3.us-east-005.backblazeb2.com', keyId: '005abc', appKeySet: true },
  status: { localBytes: 14.2 * 1024 ** 3, cloudBytes: 168.4 * 1024 ** 3, lastMoveAt: 0, lastError: '', moving: false },
  pruneDays: 365,
};

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

function text(node) {
  if (!node) return '';
  if (typeof node.textContent === 'string' && node.textContent !== '') return node.textContent;

  return (node.children ?? []).map(text).join(' ');
}

async function openSection(api) {
  const context = getAdminContext();

  Object.assign(context.api, api);

  const el = new (customElements.get('rdio-admin-audiostorage'))();

  el.connectedCallback();

  await tick();

  clearInterval(el.poll);

  return el;
}

function inputOf(el, key) {
  const row = findAll(el, (node) => node.dataset?.control === key)[0];

  return findAll(row, (node) => node.tag === 'input')[0];
}

function errorOf(el, key) {
  const row = findAll(el, (node) => node.dataset?.control === key)[0];
  const box = findAll(row, (node) => node.classList?.contains('rdio-form-error'))[0];

  return box?.textContent ?? '';
}

function button(el, label) {
  return findAll(el, (node) => node.tag === 'button' && node.textContent === label)[0];
}

test('the section reads with the approved wording', async () => {
  const el = await openSection({ getAudioStorage: async () => structuredClone(REPLY) });

  const all = text(el);

  for (const phrase of [
    'On local disk',
    'In cloud storage',
    'Last move',
    'Never',
    'Keep on local disk',
    'Recordings newer than this many days stay on the disk of the computer running Rdio Scanner. Older recordings move to cloud storage.',
    'Move older recordings to cloud storage',
    'Moves recordings older than the days above to a Backblaze B2 bucket, once a night. When off, all recordings stay on the local disk.',
    'Bucket name',
    'The name of your bucket in Backblaze B2.',
    'Endpoint',
    "The endpoint shown on your bucket's page in Backblaze B2.",
    'Key ID',
    'The keyID of an application key that can read, write, and delete in this bucket.',
    'Application key',
    'Backblaze shows this only once, when you create the key. It is hidden here after you save.',
    'Test connection',
    'Checks that the bucket can be read, written to, and deleted from.',
    'Prune Days in Options is 365. Calls older than that are deleted, and so are their recordings, wherever they are stored.',
  ]) {
    assert.ok(all.includes(phrase), `missing: ${phrase}`);
  }

  assert.ok(all.includes('14.2 GB') && all.includes('168.4 GB'));
});

test('a saved key is never put in the box; the box says it is kept', async () => {
  const el = await openSection({ getAudioStorage: async () => structuredClone(REPLY) });

  const key = inputOf(el, 'appKey');

  assert.equal(key.value, '');
  assert.equal(key.placeholder, 'Saved. Leave blank to keep it.');
  assert.equal(key.type, 'password');
});

test('a save the server refuses shows each error beside its field and saves nothing', async () => {
  const sent = [];

  const el = await openSection({
    getAudioStorage: async () => structuredClone(REPLY),
    saveAudioStorage: async (settings) => {
      sent.push(settings);

      return { ...structuredClone(REPLY), ok: false, errors: { localDays: 'Keep on local disk must be fewer days than Prune Days in Options, so recordings reach cloud storage before they are deleted.', bucket: 'Bucket name is required' } };
    },
  });

  const days = inputOf(el, 'localDays');
  days.value = '400';
  days.dispatchEvent(new Event('input'));

  button(el, 'Save').click();

  await tick();
  await tick();

  assert.equal(sent.length, 1);
  assert.equal(sent[0].localDays, 400);
  assert.equal(sent[0].appKey, '', 'a blank key box is sent blank, which keeps the saved key');
  assert.match(errorOf(el, 'localDays'), /must be fewer days than Prune Days/);
  assert.equal(errorOf(el, 'bucket'), 'Bucket name is required');
});

test('test connection says what happened in the approved words', async () => {
  let answer = { ok: true, reason: '' };

  const el = await openSection({
    getAudioStorage: async () => structuredClone(REPLY),
    testAudioStorage: async () => answer,
  });

  button(el, 'Test connection').click();
  await tick();

  assert.ok(text(el).includes('Connected. Read, write, and delete work.'));

  answer = { ok: false, reason: 'the key was refused' };

  button(el, 'Test connection').click();
  await tick();

  assert.ok(text(el).includes("Couldn't connect: the key was refused"));
});

test('a move in progress and a failed move read as the tile says them', async () => {
  const el = await openSection({
    getAudioStorage: async () => ({ ...structuredClone(REPLY), status: { ...REPLY.status, moving: true } }),
  });

  assert.ok(text(el).includes('Moving now…'));

  const failed = await openSection({
    getAudioStorage: async () => ({ ...structuredClone(REPLY), status: { ...REPLY.status, lastMoveAt: Date.now(), lastError: "the endpoint couldn't be reached" } }),
  });

  assert.ok(text(failed).includes("Last move failed: the endpoint couldn't be reached"));
  assert.ok(text(failed).includes('Today '));
});

test('the figures read the way the tiles show them', () => {
  assert.equal(formatBytes(0), '0 MB');
  assert.equal(formatBytes(812 * 1024 ** 2), '812 MB');
  assert.equal(formatBytes(14.24 * 1024 ** 3), '14.2 GB');

  const now = new Date(2026, 10, 16, 9, 0);

  assert.equal(formatMoment(0, now), 'Never');
  assert.match(formatMoment(new Date(2026, 10, 16, 3, 0).getTime(), now), /^Today /);
  assert.match(formatMoment(new Date(2026, 10, 15, 3, 0).getTime(), now), /^Yesterday /);
  assert.match(formatMoment(new Date(2026, 10, 2, 3, 0).getTime(), now), /^Nov 2, /);

  assert.equal(pruneNote(0), 'Prune Days in Options is 0, so calls and their recordings are kept forever.');
});
