/** Helpers for naming this device and reading pairing links. */

/** A friendly default name for this browser, e.g. "Chrome on Mac" or "Safari on iPhone". */
export function defaultDeviceName(
  ua: string = typeof navigator !== 'undefined' ? navigator.userAgent : '',
): string {
  const browser = /Edg\//.test(ua)
    ? 'Edge'
    : /OPR\//.test(ua)
      ? 'Opera'
      : /Firefox\/|FxiOS\//.test(ua)
        ? 'Firefox'
        : /Chrome\/|CriOS\//.test(ua)
          ? 'Chrome'
          : /Safari\//.test(ua)
            ? 'Safari'
            : 'Browser';
  const os = /iPhone/.test(ua)
    ? 'iPhone'
    : /iPad/.test(ua)
      ? 'iPad'
      : /Android/.test(ua)
        ? 'Android'
        : /Mac OS X|Macintosh/.test(ua)
          ? 'Mac'
          : /Windows/.test(ua)
            ? 'Windows'
            : /CrOS/.test(ua)
              ? 'Chromebook'
              : /Linux/.test(ua)
                ? 'Linux'
                : '';
  return os ? `${browser} on ${os}` : browser;
}

const CODE_RE = /^[0-9A-HJKMNP-TV-Z]{4}-?[0-9A-HJKMNP-TV-Z]{4}$/;

/**
 * Normalizes typed input into the XXXX-XXXX form (Crockford: I/L → 1, O → 0), or ''
 * when it cannot be a code.
 */
export function normalizeCode(input: string): string {
  const s = input.toUpperCase().replace(/[\s-]/g, '').replace(/[IL]/g, '1').replace(/O/g, '0');
  if (!CODE_RE.test(s)) return '';
  return `${s.slice(0, 4)}-${s.slice(4)}`;
}

/** The code in a /pair/{code} or /approve/{code} path, or ''. */
export function codeFromPath(path: string, prefix: 'pair' | 'approve'): string {
  const m = new RegExp(`^/${prefix}/([^/?#]+)/?$`).exec(path);
  return m ? normalizeCode(decodeURIComponent(m[1])) : '';
}

/**
 * Whether a /pair link came from the local launcher (`syre open`): marked #launch and
 * opened on a loopback hub. Such a page signs in without asking; the hub only redeems a
 * launcher code from its own machine, so the mark grants nothing by itself.
 */
export function isLauncherLink(hostname: string, hash: string): boolean {
  if (hash !== '#launch') return false;
  const h = hostname.replace(/^\[|\]$/g, '');
  return h === 'localhost' || h === '::1' || /^127\.\d+\.\d+\.\d+$/.test(h);
}

/** "4:59" for a countdown in milliseconds. */
export function formatCountdown(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000));
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`;
}
