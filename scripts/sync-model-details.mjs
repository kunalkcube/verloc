#!/usr/bin/env node
// Builds models.json (committed in the repo) by merging two sources:
//
//   1. model-details.md   — rich metadata (description, owned_by, tags,
//                           tiered pricing, web_search, created, released)
//                           indexed by its own `name` field.
//   2. opencode.jsonc     — complete pricing + names for all 25 verloc models
//                           (canonical $/1M-token rates used by the dashboard).
//
// Output is one entry per verloc model id from config.json's model_map.
// Metadata from model-details.md is attached by matching the opencode `name`
// (with its "(backend)" suffix stripped) to model-details' `name`.
//
// Re-run after editing either source.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(__dirname, '..');

const PATHS = {
  md: path.join(root, 'docs/model-details.md'),
  oc: process.env.OPENCODE_CONFIG || '/home/kunal/.config/opencode/opencode.jsonc',
  verloc: path.join(root, 'config.json'),
  out: path.join(root, 'internal/server/models.json'),
};

// ---------- parse model-details.md ----------
// The file is flat (all lines at indent 0) with group headers having no
// value: `pricing\t`, `input_tiers\t`, `output_tiers\t`,
// `input_cache_read_tiers\t`, `tags\t`. The next lines belong to the most
// recent group until a new group header or a known top-level key is seen.
// Tier entries are `N\t{ cost: "0.0000003", min: 0, max: 512000 }` lines.
function parseModelDetails(text) {
  const blocks = text.split(/\n\s*\n/).map(b => b.trim()).filter(Boolean);
  const out = {};
  for (const block of blocks) {
    const lines = block.split('\n');
    const strip = (s) => s && s.length >= 2 && s.startsWith('"') && s.endsWith('"') ? s.slice(1, -1) : s;
    const top = {};          // id, name, description, etc.
    const pricing = {};      // input, output, input_cache_read, web_search
    const tiers = {};        // input_tiers, output_tiers, input_cache_read_tiers: []
    const tags = [];
    const GROUP_TIERS = new Set(['input_tiers', 'output_tiers', 'input_cache_read_tiers']);
    const GROUP_TAGS = 'tags';
    let currentGroup = null; // 'input_tiers' | 'output_tiers' | 'input_cache_read_tiers' | 'tags' | null
    let currentTier = null;  // the tier object being built (for `N\t{...}` lines)

    for (const line of lines) {
      const m = line.match(/^([^\t]+)\t(.*)$/);
      if (!m) continue;
      const k = m[1], raw = m[2];

      // Tier entry: `N\t{ cost: "...", min: N, max: N }` — only valid inside a *_tiers group.
      if (currentGroup && GROUP_TIERS.has(currentGroup) && /^\d+$/.test(k)) {
        const t = parseTierValue(raw);
        if (t) tiers[currentGroup].push(t);
        continue;
      }
      // Tag entry: `N\t"value"` — only valid inside the tags group.
      if (currentGroup === GROUP_TAGS && /^\d+$/.test(k)) {
        tags.push(strip(raw));
        continue;
      }
      // Group header (key with no value): switch current group.
      if (raw === '') {
        if (GROUP_TIERS.has(k)) {
          currentGroup = k;
          tiers[k] = tiers[k] || [];
        } else if (k === GROUP_TAGS) {
          currentGroup = k;
        } else {
          currentGroup = null;
        }
        continue;
      }
      // Inline-object pricing, e.g. `pricing\t{ input: "0.0000001", output: "0.0000002", input_cache_read: "0.00000001" }`.
      // Some model-details.md entries (poolside/*) use this single-line JS
      // object form instead of the multi-line group. Parse and merge into
      // the `pricing` dict so downstream code can read pricing.input /
      // pricing.output / pricing.input_cache_read uniformly.
      if (k === 'pricing') {
        const objMatch = raw.match(/^\{([^}]+)\}$/);
        if (objMatch) {
          for (const part of objMatch[1].split(',')) {
            const kv = part.trim().match(/^([a-z_]+):\s*"?(-?[\d.]+)"?$/);
            if (kv) pricing[kv[1]] = castScalar(kv[2]);
          }
          currentGroup = null;
          continue;
        }
      }
      // Regular key/value. Known top-level fields go to `top`; pricing-related
      // go to `pricing` only if no group is active (defensive — file is flat
      // so this rarely matters, but keeps it clean).
      const sv = strip(raw);
      if (k === 'id' || k === 'object' || k === 'name' || k === 'description' ||
          k === 'owned_by' || k === 'context_window' || k === 'max_tokens' ||
          k === 'type' || k === 'created' || k === 'released') {
        top[k] = sv;
      } else {
        pricing[k] = castScalar(sv);
      }
      // A known top-level key closes any open group (file is flat, so this
      // is a no-op in practice, but it makes the parser robust).
      currentGroup = null;
    }
    if (!top.id || out[top.id]) continue; // dedupe (file has a duplicate nemotron)

    // Normalize tiered pricing: the file stores cost in $/token; the API
    // exposes everything in $/1M tokens. Multiply by 1e6 so consumers
    // reading either `pricing.input` or `pricing.input_tiers[0].cost`
    // get the same number.
    for (const arr of Object.values(tiers)) {
      for (const t of arr) {
        if (typeof t.cost === 'number') t.cost = +(t.cost * 1e6).toPrecision(6);
      }
    }

    out[top.id] = {
      id: top.id,
      object: top.object || 'model',
      created: Number(top.created) || 0,
      released: Number(top.released) || 0,
      owned_by: top.owned_by || '',
      name: top.name || top.id,
      description: top.description || '',
      context_window: Number(top.context_window) || 0,
      max_tokens: Number(top.max_tokens) || 0,
      type: top.type || 'language',
      tags,
      pricing: { ...pricing, ...tiers },
    };
  }
  return out;
}

function parseTierValue(s) {
  // `{ cost: "0.0000003", min: 0, max: 512000 }` -> { cost, min, max }
  const m = s.match(/^\{([^}]+)\}$/);
  if (!m) return null;
  const t = {};
  for (const part of m[1].split(',')) {
    const kv = part.trim().match(/^([a-z_]+):\s*"?(-?[\d.]+)"?$/);
    if (kv) t[kv[1]] = castScalar(kv[2]);
  }
  return t;
}

function castScalar(s) {
  if (s === 'true') return true;
  if (s === 'false') return false;
  if (s === '' || s == null) return 0;
  if (/^-?\d+$/.test(s)) return Number(s);
  if (/^-?\d+\.\d+$/.test(s)) return Number(s);
  return s;
}

// Canonical model-details prices are $/token (e.g. 0.0000003). Hyper-style
// blocks are already $/1M (e.g. 0.32664) and use cache_hit instead of
// input_cache_read. Treat anything >= $0.001/token as already per-1M —
// a real per-token rate that large would be $1,000/1M.
function mdPricingToPer1M(p = {}) {
  const input = Number(p.input) || 0;
  const output = Number(p.output) || 0;
  const cache = Number(p.input_cache_read || p.cache_hit) || 0;
  const mul = (input >= 0.001 || output >= 0.001) ? 1 : 1e6;
  return { input: input * mul, output: output * mul, cache_read: cache * mul };
}

// JSON.parse keeps the last duplicate key. Flag collisions in the raw
// verloc.models object so a reused slug (minimax-m3-2, kimi-k3-2, …)
// cannot silently drop a backend's pricing again.
function warnDuplicateModelKeys(text) {
  const m = text.match(/"verloc"\s*:\s*\{[\s\S]*?"models"\s*:\s*\{/);
  if (!m) return;
  const start = m.index + m[0].length;
  let depth = 1, i = start;
  while (i < text.length && depth > 0) {
    if (text[i] === '{') depth++;
    else if (text[i] === '}') depth--;
    i++;
  }
  const block = text.slice(start, i - 1);
  const keys = [];
  let d = 0;
  for (const line of block.split('\n')) {
    if (d === 0) {
      const km = line.match(/^\s+"([^"]+)":\s*\{/);
      if (km) keys.push(km[1]);
    }
    d += (line.match(/\{/g) || []).length;
    d -= (line.match(/\}/g) || []).length;
  }
  const seen = new Map();
  for (const k of keys) seen.set(k, (seen.get(k) || 0) + 1);
  const dups = [...seen.entries()].filter(([, n]) => n > 1);
  if (dups.length) {
    console.warn('WARN: duplicate keys in opencode.jsonc provider.verloc.models (last wins):',
      dups.map(([k, n]) => `${k}×${n}`).join(', '));
  }
}

// ---------- parse opencode.jsonc (verloc provider) ----------
function parseOpencodeVerloc(text) {
  warnDuplicateModelKeys(text);
  let s = text.replace(/^\uFEFF/, '');
  s = s.replace(/\/\*[\s\S]*?\*\//g, '');
  s = s.replace(/(^|[^:])\/\/.*$/gm, '$1');
  s = s.replace(/,(\s*[}\]])/g, '$1');
  const data = JSON.parse(s);
  const models = data?.provider?.verloc?.models || {};
  const out = {};
  for (const m of Object.values(models)) {
    if (!m.id) continue;
    const c = m.cost || {};
    out[m.id] = {
      id: m.id,
      name: m.name || m.id,
      context_window: m.limit?.context || 0,
      max_tokens: m.limit?.output || 0,
      pricing_per_1m: {
        input: Number(c.input) || 0,
        output: Number(c.output) || 0,
        cache_read: Number(c.cache_read) || 0,
      },
      // extra metadata opencode has that model-details.md usually doesn't:
      attachment: m.attachment,
      reasoning: m.reasoning,
      temperature: m.temperature,
      knowledge: m.knowledge,
      release_date: m.release_date,
      status: m.status,
      modalities: m.modalities,
      openrouter: m.openrouter,
    };
  }
  return out;
}

// ---------- verloc config: get list of verloc model ids + their upstreams ----------
function parseVerlocModelMap(text) {
  const data = JSON.parse(text);
  const mm = data.model_map || {};
  const entries = [];
  for (const [verlocId, val] of Object.entries(mm)) {
    // Split at the FIRST colon only. The upstream id may itself contain
    // colons (e.g. OpenRouter's `inclusionai/ling-3.0-flash:free`), and
    // `split(':', 2)` would discard everything after the second colon.
    const upstream = val.includes(':') ? val.slice(val.indexOf(':') + 1) : val;
    entries.push({ verlocId, upstream });
  }
  return entries;
}

// ---------- main: merge into one entry per verloc model id ----------
// Lookup strategy:
//   - verloc model ids (config.json model_map) and pricing (opencode.jsonc
//     verloc provider) are the canonical source of truth for the 25 models.
//   - model-details.md is a separate metadata source (descriptions, tags,
//     tiered pricing, web_search) that uses its own id/name namespace. We
//     look it up by name, stripping the "(backend)" suffix from opencode's
//     `name` field — e.g. opencode "GLM 4.7 (vercel)" matches md "GLM 4.7".
function normalizeName(n) {
  return String(n || '').toLowerCase().replace(/\s*\([^)]+\)\s*$/, '').trim();
}

function main() {
  const md = parseModelDetails(fs.readFileSync(PATHS.md, 'utf8'));
  const mdByName = new Map();
  for (const e of Object.values(md)) {
    if (e.name) mdByName.set(normalizeName(e.name), e);
  }
  // Also index by upstream id (the `id` field in model-details.md) so that
  // verloc model_map entries without a corresponding opencode.jsonc config
  // can still pick up rich metadata + pricing by matching their upstream
  // model id directly (e.g. gq/openai/gpt-oss-120b -> openai/gpt-oss-120b).
  // Strip a trailing `:free` suffix when indexing so OpenRouter-style
  // upstreams (e.g. `inclusionai/ling-3.0-flash:free`) still match
  // model-details entries that use the `-free` form.
  const mdById = new Map();
  for (const e of Object.values(md)) {
    if (e.id) {
      mdById.set(e.id, e);
      const stripped = e.id.replace(/:free$/, '-free');
      if (stripped !== e.id && !mdById.has(stripped)) mdById.set(stripped, e);
    }
  }
  const oc = parseOpencodeVerloc(fs.readFileSync(PATHS.oc, 'utf8'));
  const verlocEntries = parseVerlocModelMap(fs.readFileSync(PATHS.verloc, 'utf8'));

  const out = [];
  const stats = { total: 0, withMdMeta: 0, withOcPricing: 0, fullMerge: 0 };

  for (const { verlocId, upstream } of verlocEntries) {
    stats.total++;
    const ocEntry = oc[verlocId];
    // Try opencode-name match first (existing behavior). Fall back to
    // matching the upstream id directly in model-details.md so that
    // verloc models not yet in opencode.jsonc still get metadata + pricing.
    let mdEntry = ocEntry ? mdByName.get(normalizeName(ocEntry.name)) : null;
    if (!mdEntry) mdEntry = mdById.get(upstream) || null;
    if (!mdEntry) {
      // Strip a trailing `:free` from the upstream so OpenRouter free-tier
      // upstreams (e.g. `inclusionai/ling-3.0-flash:free`) match the
      // `-free` form used in model-details.md.
      const stripped = upstream.replace(/:free$/, '-free');
      if (stripped !== upstream) mdEntry = mdById.get(stripped) || null;
    }
    if (mdEntry) stats.withMdMeta++;
    if (ocEntry) stats.withOcPricing++;
    if (mdEntry && ocEntry) stats.fullMerge++;

    // Pricing: prefer opencode ($/1M, complete). If opencode has no pricing
    // (all zeros — common for free-tier models that lack a `cost` block),
    // fall back to model-details. Canonical md prices are $/token and need
    // ×1e6; hyper-style entries are already $/1M (0.33, 3.27, …).
    let pricing = null;
    if (ocEntry) {
      pricing = { ...ocEntry.pricing_per_1m };
      if (pricing.input === 0 && pricing.output === 0 && mdEntry) {
        pricing = mdPricingToPer1M(mdEntry.pricing);
      }
    } else if (mdEntry) {
      pricing = mdPricingToPer1M(mdEntry.pricing);
    } else {
      pricing = { input: 0, output: 0, cache_read: 0 };
    }

    // OpenRouter-compatible pricing (per-token, strings). This is what
    // /v1/models surfaces so OpenAI/OpenRouter-style clients (e.g. hermes)
    // can read it without unit confusion.
    const or_pricing = {
      prompt:            (pricing.input     / 1e6).toFixed(10).replace(/\.?0+$/, '') || '0',
      completion:        (pricing.output    / 1e6).toFixed(10).replace(/\.?0+$/, '') || '0',
    };
    if (pricing.cache_read) or_pricing.input_cache_read = (pricing.cache_read / 1e6).toFixed(10).replace(/\.?0+$/, '') || '0';

    // Build the OpenAI-style response object.
    const entry = {
      id: verlocId,
      object: 'model',
      created: mdEntry?.created || 0,
      owned_by: mdEntry?.owned_by || ocEntry?.name?.match(/\(([^)]+)\)/)?.[1] || upstream.split('/')[0],
      upstream,
      context_window: mdEntry?.context_window || ocEntry?.context_window || 0,
      max_tokens: mdEntry?.max_tokens || ocEntry?.max_tokens || 0,
      type: mdEntry?.type || 'language',
      name: ocEntry?.name || mdEntry?.name || verlocId,
      description: mdEntry?.description || '',
      tags: mdEntry?.tags || [],
      // OpenRouter-style per-token string pricing. Served as `pricing` in
      // /v1/models so OpenAI/OpenRouter-compatible agents can read it.
      pricing: or_pricing,
      // Per-1M-token numeric pricing. Served as `pricing` in /v1/pricing
      // (the dedicated endpoint the dashboard reads from).
      pricing_per_million: {
        input: pricing.input,
        output: pricing.output,
        cache_read: pricing.cache_read,
        currency: 'USD',
        per: '1M tokens',
      },
    };
    // Tiered pricing (from model-details) — keep on the per-1M endpoint;
    // OpenRouter's pricing format has no concept of tiers, so don't add
    // them to the per-token `pricing` field above.
    if (mdEntry?.pricing) {
      const t = mdEntry.pricing;
      if (t.input_tiers?.length) entry.pricing_per_million.input_tiers = t.input_tiers;
      if (t.output_tiers?.length) entry.pricing_per_million.output_tiers = t.output_tiers;
      if (t.input_cache_read_tiers?.length) entry.pricing_per_million.input_cache_read_tiers = t.input_cache_read_tiers;
    }
    if (mdEntry?.pricing?.web_search) entry.pricing_per_million.web_search = mdEntry.pricing.web_search;
    if (mdEntry?.released) entry.released = mdEntry.released;
    if (ocEntry?.release_date) entry.release_date = ocEntry.release_date;
    if (ocEntry?.attachment != null) entry.attachment = ocEntry.attachment;
    if (ocEntry?.reasoning != null) entry.reasoning = ocEntry.reasoning;
    if (ocEntry?.temperature != null) entry.temperature = ocEntry.temperature;
    if (ocEntry?.knowledge) entry.knowledge = ocEntry.knowledge;
    if (ocEntry?.status) entry.status = ocEntry.status;
    if (ocEntry?.modalities) entry.modalities = ocEntry.modalities;
    out.push(entry);
  }

  fs.writeFileSync(PATHS.out, JSON.stringify(out, null, 2) + '\n');
  console.log(`wrote ${out.length} verloc models -> ${path.relative(root, PATHS.out)}`);
  console.log(`  with rich metadata from model-details.md: ${stats.withMdMeta}/${stats.total}`);
  console.log(`  with pricing from opencode.jsonc:          ${stats.withOcPricing}/${stats.total}`);
  console.log(`  fully merged:                              ${stats.fullMerge}/${stats.total}`);

  // Sanity: every entry should have non-zero context_window and at least input+output > 0
  // (some poolside models legitimately have $0 pricing).
  const zeroCtx = out.filter(e => !e.context_window);
  const zeroPrice = out.filter(e => e.pricing.input === 0 && e.pricing.output === 0);
  if (zeroCtx.length) console.warn(`WARN: ${zeroCtx.length} models have no context_window:`, zeroCtx.map(e => e.id));
  if (zeroPrice.length) console.warn(`NOTE: ${zeroPrice.length} models have $0 pricing (likely free tier):`, zeroPrice.map(e => e.id));
  const insane = out.filter(e => e.pricing_per_million.input > 1000 || e.pricing_per_million.output > 1000);
  if (insane.length) console.warn(`WARN: ${insane.length} models have implausible $/1M rates:`, insane.map(e => `${e.id}=${e.pricing_per_million.input}/${e.pricing_per_million.output}`));
}

main();
