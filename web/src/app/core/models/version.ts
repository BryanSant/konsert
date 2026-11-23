/**
 * Build-version helpers for hub/worker mismatch warnings. Mirrors
 * controller.NormalizeVersion / VersionMismatch on the Go side: empty, "dev"
 * and "unknown" versions are unknown and never count as a mismatch.
 */
export function normalizeVersion(v: string | null | undefined): string {
  const t = (v ?? '').trim();
  if (['', 'dev', 'unknown', '(devel)'].includes(t.toLowerCase())) return '';
  return t.replace(/^[vV]/, '');
}

export function isVersionMismatch(
  hubVersion: string | null | undefined,
  workerVersion: string | null | undefined,
): boolean {
  const h = normalizeVersion(hubVersion);
  const w = normalizeVersion(workerVersion);
  return h !== '' && w !== '' && h !== w;
}
