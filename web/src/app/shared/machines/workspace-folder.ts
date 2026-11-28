/**
 * The fleet workspace folder (SYR-W3SVZ): one folder relative to each machine's home,
 * stored as parts joined by "/" ("src", "source/repos"). Same rules as the hub's
 * normalizeWorkspaceFolder.
 */

export interface WorkspaceSuggestion {
  path: string;
  machine_ids: string[];
  count: number;
  total: number;
  repos: number;
}

export interface WorkspaceFolders {
  suggestions: WorkspaceSuggestion[];
  total: number;
  saved: string;
  effective: string;
}

/** Turns "~/src", "src/" or "~\\source\\repos" into "src" / "source/repos", or says why not. */
export function normalizeFolder(input: string): { path: string; error?: string } {
  let s = input.trim();
  if (s === '' || s === '~' || s === '~/' || s === '~\\') return { path: '' };
  if (s.startsWith('~/') || s.startsWith('~\\')) s = s.slice(2);
  else if (s.startsWith('/') || s.startsWith('\\'))
    return { path: '', error: 'Use a folder in each home, like ~/src, not an absolute path.' };
  if (/^[A-Za-z]:/.test(s))
    return { path: '', error: 'No drive letters: the folder is inside each machine’s home.' };
  const parts: string[] = [];
  for (const raw of s.split(/[\\/]+/)) {
    const p = raw.trim();
    if (!p || p === '.') continue;
    if (p === '..' || p.startsWith('~'))
      return { path: '', error: 'The folder must stay inside each machine’s home (no ..).' };
    if (/[:*?"<>|]/.test(p))
      return { path: '', error: 'That name has characters Windows does not allow in a folder.' };
    parts.push(p);
  }
  return { path: parts.join('/') };
}

/** How a machine shows the folder: ~/src, or ~\src on Windows. */
export function displayFolder(path: string, os = ''): string {
  if (!path) return '~';
  return os === 'windows' ? `~\\${path.split('/').join('\\')}` : `~/${path}`;
}

/** Parts that mean something on one OS only, and the OSes they belong to. */
const OS_SPECIFIC: Record<string, string> = {
  appdata: 'Windows',
  library: 'macOS',
  applications: 'macOS',
  '.config': 'Linux',
  '.local': 'Linux',
  documents: 'macOS and Windows',
  desktop: 'macOS and Windows',
};

/** A warning when the folder is tied to one OS ("will be created as ~/AppData/… on macOS and Linux"). */
export function osSpecificWarning(path: string): string {
  const first = path.split('/')[0]?.toLowerCase() ?? '';
  const home = OS_SPECIFIC[first];
  if (!home) return '';
  const elsewhere =
    home === 'Windows'
      ? 'macOS and Linux'
      : home === 'macOS'
        ? 'Linux and Windows'
        : home === 'Linux'
          ? 'macOS and Windows'
          : 'Linux';
  return `${displayFolder(path)} is a ${home} folder: it will be created as ${displayFolder(path)} on ${elsewhere}.`;
}

/** "on 4 of 5 machines · will be created on daybreak-wsl" for a folder. */
export function coverageText(
  path: string,
  folders: WorkspaceFolders | null,
  names: Readonly<Record<string, string>>,
): string {
  if (!path || !folders || folders.total === 0) return '';
  const sg = folders.suggestions.find((s) => s.path.toLowerCase() === path.toLowerCase());
  const have = new Set(sg?.machine_ids ?? []);
  const missing = Object.keys(names).filter((id) => !have.has(id));
  let text = `on ${have.size} of ${folders.total} ${folders.total === 1 ? 'machine' : 'machines'}`;
  if (missing.length && missing.length <= 3)
    text += ` · will be created on ${missing.map((id) => names[id]).join(', ')}`;
  else if (missing.length) text += ` · will be created on ${missing.length} machines`;
  return text;
}
