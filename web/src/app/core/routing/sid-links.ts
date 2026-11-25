import { UrlMatchResult, UrlSegment } from '@angular/router';

/**
 * Every user-facing short ID prefix minted by `internal/ids` on the hub.
 * Keep in sync with `internal/ids/ids.go`.
 */
export const SID_PREFIXES = [
  't', // task
  'c', // chat
  'm', // machine
  'p', // project
  'r', // repo
  's', // space
  'ws', // workspace
  'wt', // worktree
  'h', // hold
  'rq', // user request
  'q', // question
  'o', // override request
  'v', // veto
  'rv', // review
  'dv', // device credential
  'pc', // pairing code
  'mc', // machine credential
] as const;

export type SidPrefix = (typeof SID_PREFIXES)[number];

const SID_PREFIX_SET: ReadonlySet<string> = new Set<string>(SID_PREFIXES);
const SID_RE = /^([a-z]{1,2})-([0-9a-z]+)$/;

export interface ParsedSid {
  prefix: SidPrefix;
  suffix: string;
}

/** Splits a user-facing short ID (e.g. `t-7qk3m`, `m-k4n8p`) into its kind prefix and suffix. */
export function parseSid(value: string): ParsedSid | null {
  const m = SID_RE.exec(value);
  if (!m) return null;
  const prefix = m[1];
  if (!SID_PREFIX_SET.has(prefix)) return null;
  return { prefix: prefix as SidPrefix, suffix: m[2] };
}

/**
 * Resolves a mode-agnostic `/<sid>` short link to its canonical mode route.
 * Returns `null` when `sid` is not a recognized short ID.
 */
export function resolveSidRoute(sid: string, options: { phone?: boolean } = {}): string | null {
  const parsed = parseSid(sid);
  if (!parsed) return null;
  const phone = options.phone === true;
  switch (parsed.prefix) {
    case 't':
    case 'c':
      return phone ? `/inbox/${sid}` : `/command/${sid}`;
    case 'm':
      return `/fleet/${sid}`;
    case 's':
    case 'mc':
      return '/fleet';
    case 'rv':
    case 'p':
    case 'r':
    case 'ws':
    case 'wt':
      return phone ? '/inbox' : '/work';
    case 'q':
    case 'rq':
    case 'o':
    case 'v':
    case 'h':
    case 'dv':
    case 'pc':
      return phone ? '/inbox' : '/command';
  }
}

/** Builds a shareable mode-agnostic short link (`<origin>/<sid>`). */
export function sidUrl(sid: string, origin = typeof location === 'undefined' ? '' : location.origin): string {
  return `${origin.replace(/\/+$/, '')}/${sid}`;
}

/** Matches a single top-level `/<sid>` segment for any known short ID prefix. */
export function sidMatcher(segments: UrlSegment[]): UrlMatchResult | null {
  if (segments.length !== 1) return null;
  const seg = segments[0];
  if (!parseSid(seg.path)) return null;
  return { consumed: segments, posParams: { sid: seg } };
}
